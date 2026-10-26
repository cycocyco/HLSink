//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// appProtocolScheme 是扩展与桌面端约定的本地启动协议。
// 扩展在下载器离线时打开 hlsink://open，Windows 会按 HKCU 注册表拉起本程序。
const appProtocolScheme = "hlsink"

// registerURLProtocol 注册当前用户的 hlsink:// 协议。
//
// 使用 HKCU 而不是 HKLM，不需要管理员权限。每次正常启动都会刷新 exe 路径，
// 因此程序被移动后，下一次启动仍会把协议指向正确位置。
func registerURLProtocol() error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("获取程序路径失败: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return fmt.Errorf("解析程序路径失败: %w", err)
	}

	base := `Software\Classes\` + appProtocolScheme
	key, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("创建协议注册项失败: %w", err)
	}
	defer key.Close()

	if err := key.SetStringValue("", "URL:HLSink 视频下载器"); err != nil {
		return fmt.Errorf("写入协议说明失败: %w", err)
	}
	if err := key.SetStringValue("URL Protocol", ""); err != nil {
		return fmt.Errorf("写入 URL Protocol 失败: %w", err)
	}

	icon, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\DefaultIcon`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("创建协议图标项失败: %w", err)
	}
	defer icon.Close()
	if err := icon.SetStringValue("", exe+",0"); err != nil {
		return fmt.Errorf("写入协议图标失败: %w", err)
	}

	commandKey, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\shell\open\command`, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return fmt.Errorf("创建协议命令项失败: %w", err)
	}
	defer commandKey.Close()
	command := fmt.Sprintf(`"%s" "%%1"`, exe)
	if err := commandKey.SetStringValue("", command); err != nil {
		return fmt.Errorf("写入协议命令失败: %w", err)
	}
	return nil
}
