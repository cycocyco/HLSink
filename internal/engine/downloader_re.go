package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// N_m3u8DL-RE：HLS / DASH 分片下载器。
//
// 它是分片下载的主力：HLS 的 TS 直拼、DASH 的 fMP4 交 ffmpeg 混流，
// 都是它做得比通用下载器好的地方。失败时才降级到 yt-dlp。
//
// 本文件是唯一允许出现 RE 参数的地方。改 RE 的用法不用翻别的文件。

func init() { Register(reEngine{}) }

// reStallAfter RE 连续这么久没有任何字节落地就认定它下不动，直接切 yt-dlp。
// 与其等它把每片都重试到超时（慢站点能磨几分钟），不如早点换引擎。
const reStallAfter = 10 * time.Second

var reFrag = regexp.MustCompile(`(\d+)/(\d+)\s+\d`)

type reEngine struct{}

func (reEngine) Key() string    { return KeyRE }
func (reEngine) Label() string  { return "N_m3u8DL-RE" }
func (reEngine) Binary() string { return "N_m3u8DL-RE.exe" }
func (reEngine) Priority() int  { return 10 }

func (reEngine) Stall() time.Duration { return reStallAfter }

func (reEngine) CanHandle(k Kind) bool { return k == KindHLS || k == KindDASH }

func (reEngine) Probes(j Job) []string { return []string{j.OutDir, j.TmpDir} }

// Hint RE 一上来就是下载分片，没有需要额外解释的准备阶段。
func (reEngine) Hint() string { return "" }

// Fallback RE 下不动或被盾拦时换 yt-dlp：它是通用站点解析器，覆盖面最广。
func (reEngine) Fallback() string { return KeyYtdlp }

// Build 拼装 RE 的参数（不含可执行文件本身，由调用方补 Command.Exe）。
//
// RE 只吃清单文件，网页地址不归它管（CanHandle 已排除）。
func (reEngine) Build(j Job, t Tools) (Command, error) {
	// DASH 的 fMP4 分片交给 ffmpeg 正常混流；HLS 沿用二进制合并（TS 直拼更稳更快）。
	mux := []string{"-M", "format=mp4:mux importer=raw"}
	if j.Kind == KindDASH {
		mux = []string{"-M", "format=mp4"}
	}

	args := []string{
		j.URL,
		"--save-dir", j.OutDir,
		"--tmp-dir", j.TmpDir,
		"--save-name", j.Name,
		"--thread-count", strconv.Itoa(j.Threads), // 分片并发；默认 32，替代 RE 自带的 12
		"-mt", // 音视频轨道并行下载，避免先视频后音频
		"--disable-update-check",
		"--auto-select", // 无交互环境必须自动选轨
		"--no-log",
	}
	// 只有 ffmpeg 确实存在才传：传一个不存在的路径，RE 会在混流阶段
	// 直接退出（exit status 1），报错信息还完全看不出是 ffmpeg 的问题。
	if t.FFmpeg != "" {
		args = append(args, "--ffmpeg-binary-path", t.FFmpeg)
	}
	args = append(args, mux...)

	for _, h := range headerLines(j.Headers, nil) {
		args = append(args, "-H", h)
	}
	// Referer 是独立字段；若 Headers 里已经有就不再补，避免两条冲突的头。
	if ref := CleanHeaderValue(j.Referer); ref != "" && !hasHeader(j.Headers, "Referer") {
		args = append(args, "-H", "Referer: "+ref)
	}
	return Command{Args: args}, nil
}

// ParseLine 从 RE 的一行进度里抓百分比、速度、分片数。
//
// RE 用 \r 原地刷写，调用方会把文本切成行再逐行喂进来。
func (reEngine) ParseLine(line string) (Progress, bool) {
	idx := strings.Index(line, "%")
	if idx < 0 || idx > 12 {
		return Progress{}, false
	}
	var p float64
	if _, err := fmt.Sscanf(strings.TrimSpace(line[:idx]), "%f", &p); err != nil {
		return Progress{}, false
	}
	if p < 0 || p > 100 {
		return Progress{}, false
	}
	speed := ""
	if i := strings.Index(line, "Bps"); i > 0 {
		if start := strings.LastIndex(line[:i], " "); start >= 0 {
			speed = strings.TrimSpace(line[start : i+3])
		}
	}
	seg := ""
	if m := reFrag.FindStringSubmatch(line); m != nil {
		seg = m[1] + "/" + m[2]
	}
	return Progress{Percent: p, Speed: speed, Segments: seg}, true
}
