//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// 系统代理读取。
//
// 为什么需要：RE 用 .NET 的 HttpClient，默认继承 Windows 的"系统代理"；
// yt-dlp 用的 curl_cffi 只认环境变量（http_proxy/https_proxy），不读注册表。
// 于是同一个地址 RE 能下、yt-dlp 报 TLS connect error。这里把系统代理显式
// 取出来喂给 yt-dlp，让两个引擎的网络行为一致。

const ieProxyKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// systemProxy 返回可用的系统代理地址（形如 http://127.0.0.1:7890）。
// 未开启代理、或只配置了 PAC 脚本时返回空串（PAC 无法直接交给 curl 使用）。
func systemProxy() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, ieProxyKey, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer func() { _ = key.Close() }()

	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if err != nil || enabled == 0 {
		return ""
	}
	raw, _, err := key.GetStringValue("ProxyServer")
	if err != nil {
		return ""
	}
	return normalizeProxy(raw)
}

// normalizeProxy 把注册表里的 ProxyServer 规范成 URL。
//
// 注册表有两种写法：
//
//	127.0.0.1:7890                       —— 单一代理
//	http=127.0.0.1:7890;https=127.0.0.1:7890;ftp=...  —— 按协议分段
//
// 分段写法优先取 https（视频站点基本都是 https），其次 http。
func normalizeProxy(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "=") {
		var httpPart, httpsPart string
		for _, seg := range strings.Split(raw, ";") {
			kv := strings.SplitN(seg, "=", 2)
			if len(kv) != 2 {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(kv[0])) {
			case "http":
				httpPart = strings.TrimSpace(kv[1])
			case "https":
				httpsPart = strings.TrimSpace(kv[1])
			}
		}
		switch {
		case httpsPart != "":
			raw = httpsPart
		case httpPart != "":
			raw = httpPart
		default:
			return ""
		}
	}
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw
}
