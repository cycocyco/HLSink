package main

import (
	"net/url"
	"strings"
)

// 源格式分类。
//
// hls / dash 是流媒体清单，由 N_m3u8DL-RE 处理（分片并发 + 合并）；
// page 是「网页地址或非已知流地址」，交给 yt-dlp 自行解析（可覆盖大量站点）。
const (
	kindHLS  = "hls"
	kindDASH = "dash"
	kindPage = "page"
)

// normalizeStreamKind 把外部传入的 kind 规整为内部值；非法返回空字符串。
func normalizeStreamKind(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "hls", "m3u8", "m3u":
		return kindHLS
	case "dash", "mpd":
		return kindDASH
	case "page", "web", "url":
		return kindPage
	default:
		return ""
	}
}

// isKnownStreamKind 判断是否是受支持的格式。
func isKnownStreamKind(kind string) bool {
	switch kind {
	case kindHLS, kindDASH, kindPage:
		return true
	default:
		return false
	}
}

// detectStreamKind 按 URL 后缀判定格式；无法判定则当作网页地址（交给 yt-dlp）。
// 未知后缀一律走 page，而不是猜测：yt-dlp 的站点解析器比后缀猜测可靠得多。
func detectStreamKind(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return kindPage
	}
	path := strings.ToLower(u.Path)
	switch {
	case strings.HasSuffix(path, ".m3u8"), strings.HasSuffix(path, ".m3u"):
		return kindHLS
	case strings.HasSuffix(path, ".mpd"):
		return kindDASH
	default:
		return kindPage
	}
}

// streamKindLabel 格式的中文展示名。
func streamKindLabel(kind string) string {
	switch normalizeStreamKind(kind) {
	case kindHLS:
		return "HLS"
	case kindDASH:
		return "DASH"
	default:
		return "网页"
	}
}
