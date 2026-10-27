package engine

import (
	"net/url"
	"strings"
)

// NormalizeKind 把外部传入的 kind 规整为内部值；非法返回空字符串。
//
// 外部来源（浏览器扩展、HTTP 调用）可能传 m3u8/mpd/url 这类别名，
// 统一在这里归一，避免每个调用点各写一份 switch。
func NormalizeKind(raw string) Kind {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hls", "m3u8", "m3u":
		return KindHLS
	case "dash", "mpd":
		return KindDASH
	case "page", "web", "url":
		return KindPage
	case "telegram", "tg", "tdl":
		return KindTelegram
	default:
		return ""
	}
}

// Known 判断是否是受支持的类型。
func (k Kind) Known() bool {
	switch k {
	case KindHLS, KindDASH, KindPage, KindTelegram:
		return true
	default:
		return false
	}
}

// DetectKind 按 URL 路径后缀判定类型；无法判定则当作网页地址。
//
// 未知后缀一律走 page，而不是猜：站点解析器比后缀猜测可靠得多。
func DetectKind(rawURL string) Kind {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return KindPage
	}
	// Telegram 消息链接先判：它的路径是 /频道名/消息ID，不带文件后缀，
	// 落到下面会一律判成 page，从而被 yt-dlp 接走（它处理不了）。
	if isTelegramHost(u.Host) {
		return KindTelegram
	}
	path := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(path, ".m3u8"), strings.HasSuffix(path, ".m3u"):
		return KindHLS
	case strings.HasSuffix(path, ".mpd"):
		return KindDASH
	default:
		return KindPage
	}
}

// isTelegramHost 判断是否是 Telegram 的消息链接域名。
func isTelegramHost(host string) bool {
	h := strings.ToLower(host)
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	switch h {
	case "t.me", "telegram.me", "telegram.dog", "tx.me":
		return true
	}
	return false
}

// Label 返回类型的中文展示名。
func (k Kind) Label() string {
	switch NormalizeKind(string(k)) {
	case KindHLS:
		return "HLS"
	case KindDASH:
		return "DASH"
	case KindTelegram:
		return "Telegram"
	default:
		return "网页"
	}
}
