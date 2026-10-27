package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// yt-dlp：通用站点解析器，同时是 RE 的降级目标。
//
// 两条使用路径：
//   - 网页地址（KindPage）：由它自行解析站点里的视频流；
//   - 降级（RE 被 CF 拦或无进展）：--impersonate chrome 用 curl_cffi 的
//     Chrome TLS 指纹，能过掉一部分盾。
//
// 本文件是唯一允许出现 yt-dlp 参数的地方。

func init() { Register(ytdlpEngine{}) }

// ytdlpStallAfter yt-dlp 的停滞上限。它没有下一级可降级，阈值放宽到
// 45 秒（解析网页 + 过盾偶尔慢），只是为了不让任务永远卡在 0%。
const ytdlpStallAfter = 45 * time.Second

var ytFrag = regexp.MustCompile(`frag (\d+)/(\d+)`)

type ytdlpEngine struct{}

func (ytdlpEngine) Key() string    { return KeyYtdlp }
func (ytdlpEngine) Label() string  { return "yt-dlp" }
func (ytdlpEngine) Binary() string { return "yt-dlp.exe" }
func (ytdlpEngine) Priority() int  { return 20 }

func (ytdlpEngine) Stall() time.Duration { return ytdlpStallAfter }

// CanHandle 只认网页地址。已知的 HLS/DASH 清单先给 RE，RE 失败才由编排层降级过来。
func (ytdlpEngine) CanHandle(k Kind) bool { return k == KindPage }

func (ytdlpEngine) Probes(j Job) []string { return []string{j.OutDir} }

// Hint 网页地址要先解析出真实视频流，这一段可能停几十秒，得让用户知道在干什么。
func (ytdlpEngine) Hint() string { return "正在解析网页地址" }

// Fallback 已经在链条末端，没有下一级可降级。
func (ytdlpEngine) Fallback() string { return "" }

func (ytdlpEngine) Build(j Job, t Tools) (Command, error) {
	args := []string{
		j.URL,
		"--impersonate", "chrome", // curl_cffi Chrome TLS 指纹
		"--merge-output-format", "mp4",
		"-P", j.OutDir,
		"-o", j.Name + ".%(ext)s",
		"--newline", // 进度逐行输出，便于解析
		"--no-playlist",
		"-N", strconv.Itoa(j.Threads), // 分片并发与 RE 使用同一设置
		// 连接/读取卡住时别无限等：10 秒没有数据就断开重试，
		// 否则站点半死不活时任务会永远停在 0%。
		"--socket-timeout", "10",
	}
	// 显式指定自带 ffmpeg，避免依赖系统 PATH 或别的软件装的那份。
	if t.FFmpegDir != "" {
		args = append(args, "--ffmpeg-location", t.FFmpegDir)
	}

	var notes []string
	// yt-dlp（curl_cffi）不会自动读 Windows 的"系统代理"设置，
	// 用户开着代理时它会直连，于是报 TLS connect error。
	// RE 走 .NET 默认代理是通的，两边行为必须对齐。
	if p := strings.TrimSpace(j.Proxy); p != "" {
		args = append(args, "--proxy", p)
		notes = append(notes, "[HLSink] 使用系统代理 "+p)
	}

	// Referer 为空就不伪造，交给站点默认行为。
	if ref := CleanHeaderValue(j.Referer); ref != "" {
		args = append(args, "--referer", ref)
	}
	// UA 走独立参数，Referer 已单独处理，其余全部 --add-header。
	ua, extra := splitForYtdlp(j.Headers)
	if ua != "" {
		args = append(args, "--user-agent", ua)
	}
	for _, h := range extra {
		args = append(args, "--add-header", h)
	}
	return Command{Args: args, Notes: notes}, nil
}

// splitForYtdlp 拆出 User-Agent（单独传）与需要通过 --add-header 传的头。
// Referer 由 --referer 传，避免与站点默认行为冲突。
func splitForYtdlp(headers map[string]string) (ua string, extra []string) {
	if len(headers) == 0 {
		return "", nil
	}
	for _, k := range sortedHeaderKeys(headers) {
		v := CleanHeaderValue(headers[k])
		if v == "" {
			continue
		}
		switch {
		case strings.EqualFold(k, "User-Agent"):
			ua = v
		case strings.EqualFold(k, "Referer"):
			// 交给 --referer
		default:
			extra = append(extra, k+": "+v)
		}
	}
	return ua, extra
}

// ParseLine 解析 yt-dlp --newline 的进度行：
// "[download]  42.3% of 123.45MiB at 2.5MiB/s ETA 00:42"
func (ytdlpEngine) ParseLine(line string) (Progress, bool) {
	const prefix = "[download]"
	if !strings.HasPrefix(line, prefix) {
		return Progress{}, false
	}
	idx := strings.Index(line, "%")
	if idx <= len(prefix) {
		return Progress{}, false
	}
	var p float64
	if _, err := fmt.Sscanf(strings.TrimSpace(line[len(prefix):idx]), "%f", &p); err != nil {
		return Progress{}, false
	}
	if p < 0 || p > 100 {
		return Progress{}, false
	}
	speed := ""
	if i := strings.Index(line, " at "); i > 0 {
		speed = strings.TrimSpace(line[i+4:])
		if j := strings.Index(speed, " ETA"); j > 0 {
			speed = strings.TrimSpace(speed[:j])
		}
	}
	seg := ""
	if m := ytFrag.FindStringSubmatch(line); m != nil {
		seg = m[1] + "/" + m[2]
	}
	return Progress{Percent: p, Speed: speed, Segments: seg}, true
}
