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
	default:
		return ""
	}
}

// Known 判断是否是受支持的类型。
func (k Kind) Known() bool {
	switch k {
	case KindHLS, KindDASH, KindPage:
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

// Label 返回类型的中文展示名。
func (k Kind) Label() string {
	switch NormalizeKind(string(k)) {
	case KindHLS:
		return "HLS"
	case KindDASH:
		return "DASH"
	default:
		return "网页"
	}
}
