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

// normalizeProxyInput 规范化用户在设置里填的代理地址。
//
// 允许三种写法：
//
//	127.0.0.1:7890          → http://127.0.0.1:7890
//	http://127.0.0.1:7897   → 原样
//	（空）                  → 空，表示跟随系统代理
//
// 填了但解析不出主机名时原样返回，让引擎自己报错——总比在这里悄悄吞掉、
// 然后用户以为"设置了却没生效"要好。
func normalizeProxyInput(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	return raw
}

// proxyURL 返回本次下载该用的代理：设置里填了就用它，否则跟随系统。
//
// 为什么不让所有引擎都直接用系统代理：tdl 压根不读注册表；而机器上常同时
// 跑两三个代理端口（7890/7897/10808），系统里只能启用一个，下载要用的可能
// 不是"当前启用的那个"。所以给用户一个显式入口。
func (a *App) proxyURL() string {
	a.mu.Lock()
	custom := a.settings.ProxyURL
	a.mu.Unlock()
	if custom = normalizeProxyInput(custom); custom != "" {
		return custom
	}
	return systemProxy()
}
