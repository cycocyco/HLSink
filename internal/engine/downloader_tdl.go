package engine

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// tdl：Telegram 消息下载器。
//
// 它和另外两个下载引擎有几处本质差别，都在这里消化掉，不污染编排层：
//
//   - 产物类型不定：可能是一张图、一个压缩包、一段视频，扩展名事先不可知，
//     所以输出名固定用 download.<原文件名>，靠前缀让上层能找到它。
//   - 中间文件不可观测：数据攒在内存里分块写，目录可能几十秒都是 0 字节，
//     所以 Probes 返回空，看门狗退回「日志是否还在增长」判据。
//   - 必须走代理：Telegram 的 DC 直连不通，且它只认 socks5://。
//   - 它不在降级链上：CanHandle 只认 Telegram 链接、Fallback 为空串，
//     所以 HLS / 网页的失败与它无关，它失败也不会换到别的引擎。
//     RE 和 yt-dlp 才是通用下载的主力，tdl 只是把 TG 这条支线接进同一个界面。
//
// 交互注意：`tdl login` 需要真终端，本引擎不碰登录，只做下载。

func init() { Register(tdlEngine{}) }

const (
	// tdlStallAfter tdl 每 100ms 刷一行进度，30 秒没有任何新输出基本就是死了
	// （代理断了、TG 限流、账号被踢）。比 RE 宽松，因为它要连 DC、取文件信息。
	tdlStallAfter = 60 * time.Second
	// tdlDefaultThreads tdl 的单文件线程数。别跟着 HLS 的 32 走：
	// Telegram 对高并发很敏感，官方建议是每任务 8 线程。
	tdlDefaultThreads = 8
	// tdlMaxThreads 上限就是官方建议的 8。
	// 用户设置里那个 32 是给 HLS 分片用的，套到 TG 上会被限流（甚至短暂封禁）。
	tdlMaxThreads = 8
)

var (
	// 进度行的百分比后面紧跟进度条：` 6.2% [####....]`。
	// 要求后面有 `[` 是为了避开文件名里带 % 的情况（比如 "100%_test.mp4"）。
	reTdlPct = regexp.MustCompile(`([\d.]+)%\s+\[`)
	// 速度取方括号里最后一段：`[1.00 MB in 29.063s; ~ETA: 7m35s; 35.23 KB/s]`
	reTdlSpeed = regexp.MustCompile(`;\s*([\d.]+\s*[KMGT]?B/s)\s*\]`)
	// 完成行没有百分比：`... done! [69.57 KB in 5.371s; 12.92 KB/s]`
	reTdlDone = regexp.MustCompile(`done!\s*\[`)
	// ANSI 光标控制（上移、清行）。tdl 用它们原地重绘，不是用 \r。
	reANSI = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
)

type tdlEngine struct{}

func (tdlEngine) Key() string    { return KeyTdl }
func (tdlEngine) Label() string  { return "tdl" }
func (tdlEngine) Binary() string { return "tdl.exe" }
func (tdlEngine) Priority() int  { return 30 }

func (tdlEngine) Stall() time.Duration { return tdlStallAfter }

// CanHandle 只认 Telegram 消息链接。别的链接给它是浪费一次失败。
func (tdlEngine) CanHandle(k Kind) bool { return k == KindTelegram }

// Probes 故意返回空：让看门狗改用「日志是否还在增长」。
// 理由见 tdlStallAfter 与 RunOpts.Probes 的注释。
func (tdlEngine) Probes(Job) []string { return nil }

// Fallback Telegram 链接没有别的引擎能接，失败就是失败。
func (tdlEngine) Fallback() string { return "" }

// Hint 第一次握手 + 取消息信息可能要几秒，先告诉用户在干什么。
func (tdlEngine) Hint() string { return "正在连接 Telegram" }

func (tdlEngine) Build(j Job, t Tools) (Command, error) {
	if !isTelegramLink(j.URL) {
		return Command{}, fmt.Errorf("不是 Telegram 消息链接: %s", j.URL)
	}
	args := []string{
		"dl",
		"-u", j.URL,
		"-d", j.OutDir,
		"-t", strconv.Itoa(tdlThreads(j.Threads)),
		// 固定前缀，让 findStaged 能认出产物；原文件名保留在点号后面，
		// 这样保存到用户目录时还能看出是什么文件。
		"--template", "download.{{ filenamify .FileName }}",
		// 关掉 CPU/Memory 那行装饰：既省日志体积，也少一类误匹配。
		"--disable-progress-ps",
	}

	var notes []string
	// Telegram 必须走代理，且 tdl 只认 socks5://。系统代理一般是 http://，
	// 本地混合端口（Clash 之类）两种协议都吃，换 scheme 即可。
	if p := socksProxy(j.Proxy); p != "" {
		args = append(args, "--proxy", p)
		notes = append(notes, "[HLSink] 使用代理 "+p)
	}
	return Command{Args: args, Notes: notes}, nil
}

// ParseLine 解析 tdl 的进度行。
//
// 真实样例（已用 --disable-progress-ps 去掉装饰行）：
//
//	Chat(123)~ ...  6.2% [####....] [1.00 MB in 29.063s; ~ETA: 7m35s; 35.23 KB/s]
//	Chat(123)~ ... done! [69.57 KB in 5.371s; 12.92 KB/s]
//
// 两个都要满足「含 B/s」才算进度：否则 "All files will be downloaded to..."
// 这类提示行和残留的 CPU 行都会被误判。
func (tdlEngine) ParseLine(line string) (Progress, bool) {
	line = stripANSI(line)
	if !strings.Contains(line, "B/s") {
		return Progress{}, false
	}
	if reTdlDone.MatchString(line) {
		return Progress{Percent: 100}, true
	}
	m := reTdlPct.FindStringSubmatch(line)
	if m == nil {
		return Progress{}, false
	}
	var p float64
	if _, err := fmt.Sscanf(m[1], "%f", &p); err != nil {
		return Progress{}, false
	}
	if p < 0 || p > 100 {
		return Progress{}, false
	}
	speed := ""
	if sm := reTdlSpeed.FindStringSubmatch(line); sm != nil {
		speed = strings.TrimSpace(sm[1])
	}
	return Progress{Percent: p, Speed: speed}, true
}

// stripANSI 去掉光标控制序列，留下可读文本。
func stripANSI(s string) string {
	if !strings.Contains(s, "\x1b") {
		return s
	}
	return reANSI.ReplaceAllString(s, "")
}

// tdlThreads 把通用线程数收敛到 Telegram 能接受的范围。
//
// 用户在设置里填的 32 是给 HLS 分片用的，直接套过来会被 TG 限流甚至短暂封禁，
// 所以这里单独压一道。填 0 或负数按官方建议的 8 走。
func tdlThreads(n int) int {
	if n <= 0 {
		return tdlDefaultThreads
	}
	if n > tdlMaxThreads {
		return tdlMaxThreads
	}
	return n
}

// socksProxy 把任意代理地址改写成 socks5:// 形式。
//
// tdl 的 --proxy 只接受 socks；而我们从注册表读到的系统代理通常是 http://。
// 本地代理软件的混合端口两种协议都监听，所以换协议名就能用。
func socksProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw // 解析不了就原样交给 tdl，让它自己报错，别在这里吞掉
	}
	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h", "socks4":
		return raw
	default:
		u.Scheme = "socks5"
		return u.String()
	}
}

// isTelegramLink 判断是否是 t.me 类的消息链接。
func isTelegramLink(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return false
	}
	return isTelegramHost(u.Host)
}
