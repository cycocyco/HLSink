package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"hlsink/internal/engine"
)

// LoginTelegram 打开一个真实控制台窗口，跑 tdl 的扫码登录。
//
// 为什么要弹窗而不是后台跑：
// tdl 的登录用 survey 那套交互库，二维码也是往终端里打的 ASCII 图。它必须拿到
// 一个真 TTY 才能工作——管道里喂回车会直接报 "Incorrect function"，后台跑则连
// 二维码都看不见。所以只能给它一个控制台，让用户自己扫码。
//
// 为什么用 -T qr 而不是导入桌面端 tdata：
// 导入拿到的是桌面端那把 auth key 的副本。同一个 key 只该有一个主人在用，
// 桌面端一旦重新登录/换 key，副本立刻作废，表现就是「昨天能下，今天说未授权」。
// 扫码是一个独立设备，在 Telegram 的「设备」列表里单独一行，跟桌面端互不影响。
func (a *App) LoginTelegram() error {
	d, ok := engine.Get(engine.KeyTdl)
	if !ok {
		return fmt.Errorf("tdl 引擎未注册")
	}
	lg, ok := d.(engine.TerminalLogin)
	if !ok {
		return fmt.Errorf("%s 不需要登录", d.Label())
	}

	exe, err := engine.Executable(a.toolMap()[d.Key()], d.Binary(), d.Label())
	if err != nil {
		return err
	}

	// 登录也要走同一个代理：tdl 连 DC 的方式和下载完全一样。
	bat, err := a.writeLoginScript(exe, lg.LoginArgs(a.proxyURL()), lg.LoginTitle())
	if err != nil {
		return err
	}

	// 用 start 拉起，让控制台窗口与主进程解耦：
	// 用户扫完码可以自己关，我们也不用 Wait（-H windowsgui 下没有控制台可挂）。
	cmd := exec.Command("cmd.exe", "/c", "start", "", bat)
	cmd.Dir = filepath.Dir(bat)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("打开登录窗口失败: %w", err)
	}
	// 让 start 自身退出即可，不等子窗口——它要开很久（等人扫码）。
	go func() { _ = cmd.Wait() }()
	return nil
}

// writeLoginScript 生成登录用的批处理脚本。
//
// 不直接把命令拼进 `start "标题" tdl.exe login ...`：start 对引号的处理和 cmd
// 的转义规则叠在一起，路径里有空格就会散架。写个 .bat 让 cmd 自己解析最稳。
func (a *App) writeLoginScript(exe string, args []string, title string) (string, error) {
	dir := filepath.Join(workRoot(), "login")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建登录脚本目录失败: %w", err)
	}
	path := filepath.Join(dir, "tdl-login.bat")

	quoted := make([]string, 0, len(args))
	for _, s := range args {
		quoted = append(quoted, batQuote(s))
	}

	var b strings.Builder
	b.WriteString("@echo off\r\n")
	// 二维码是 UTF-8 画的，控制台默认 GBK 会把提示文字弄成乱码。
	b.WriteString("chcp 65001 >nul\r\n")
	b.WriteString("title " + title + "\r\n")
	b.WriteString("echo.\r\n")
	b.WriteString("echo   手机 Telegram：设置 - 设备 - 连接设备，扫下面的二维码\r\n")
	b.WriteString("echo   扫码成功后这个窗口可以关掉，回到 HLSink 重新添加任务就行\r\n")
	b.WriteString("echo.\r\n")
	// exe 路径一律加引号：中文目录 + 空格都可能出现，cmd 对未加引号的长路径容忍度很低。
	b.WriteString("\"" + exe + "\" " + strings.Join(quoted, " ") + "\r\n")
	b.WriteString("echo.\r\n")
	b.WriteString("echo   命令已结束，按任意键关闭窗口。\r\n")
	b.WriteString("pause >nul\r\n")

	// 末尾统一 \r\n，避免 cmd 把行尾吃进参数里。
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("写入登录脚本失败: %w", err)
	}
	return path, nil
}

// batQuote 按 cmd 的规则加引号：含空格或 & 之类必须裹起来。
// 路径里出现 % 会被当变量展开，这里也没法优雅解决——正常安装路径不会有。
func batQuote(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t&()[]{}^=;!'+,`~") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}
