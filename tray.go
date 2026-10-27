package main

import (
	_ "embed"
	"fmt"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed build/appicon.png
var trayIconPNG []byte

// =========================================================================
// 托盘 + 轻量模式
//
// 需求：点 ❌ 不退出软件，而是「进托盘 + 把前台渲染全部停掉，极致省内存」。
//
// 关键实现选择：窗口是 Close() 掉（真正销毁），不是 Hide()。
// 只 Hide() 的话 WebView2 的渲染进程、GPU 进程、JS 堆照样活着，几十 MB 一点不少；
// 销毁窗口后这些进程才会退出，进程内存能回落到个位数 MB —— 这才是"轻量模式"的
// 实际含义。代价是恢复时重建窗口要几百毫秒，这个代价是刻意的取舍。
//
// 能这么做的前提是 application.Options.Windows.DisableQuitOnLastWindowClosed：
// 否则最后一个窗口一销毁，Windows 消息循环就收到 WM_QUIT，整个进程跟着退出，
// 托盘也就没意义了。
// =========================================================================

// mainWindowOptions 主窗口的完整外观。首次启动和托盘恢复共用，保证重建出来的
// 窗口和原来一模一样（尺寸、圆角、主题、in-memory 资源都一致）。
func mainWindowOptions() application.WebviewWindowOptions {
	return application.WebviewWindowOptions{
		Title:     "HLSink",
		Width:     640,
		Height:    620,
		MinWidth:  480,
		MinHeight: 420,
		// 不需要最大化：把上限锁在初始尺寸，拖到屏幕边缘或双击标题区也不会放大。
		MaxWidth:         640,
		MaxHeight:        620,
		Frameless:        true, // 自绘标题栏，与界面同色融合
		BackgroundColour: application.NewRGB(11, 12, 13),
		URL:              "/",
		Windows: application.WindowsWindow{
			// 注意：不要设 DisableFramelessWindowDecorations。
			// 该选项为 true 时 v3 会保留系统标题栏（framelessWithDecorations 判定为 false），
			// 同时丢失 Win11 圆角。保持默认才会有真正的无边框 + 圆角。
			Theme: application.Dark,
		},
	}
}

// setupTray 创建常驻托盘（必须在 app.Run 之前调用）。
func (a *App) setupTray(app *application.App) *application.SystemTray {
	tray := app.SystemTray.New()
	tray.SetIcon(trayIconPNG)
	tray.SetTooltip("HLSink · HLS / DASH 下载器")

	menu := application.NewMenu()
	menu.Add("显示主界面").OnClick(func(*application.Context) { a.ShowMainWindow() })
	menu.AddSeparator()
	menu.Add("退出 HLSink").OnClick(func(*application.Context) { a.QuitFromTray() })
	tray.SetMenu(menu)

	// 左键单击：有窗口就拉到前台，没窗口（轻量模式）就重建一个。
	// 右键交给 v3 的默认行为（设过 Menu 就会自动弹菜单）。
	tray.OnClick(func() { a.ShowMainWindow() })

	a.mu.Lock()
	a.tray = tray
	a.mu.Unlock()
	return tray
}

// createMainWindow 创建主窗口并接管「窗口销毁」的记账。
//
// 这里注册的 WindowClosing 监听器只做一件事：把 a.win 置空。
// 不这么做的话，托盘点「显示主界面」时拿到的还是一个已销毁的句柄，
// Show() 静默失败，用户会以为软件死了。
func (a *App) createMainWindow(app *application.App) *application.WebviewWindow {
	if app == nil {
		return nil
	}
	// 串行化：托盘连点、单实例唤起可能同时进来，别建出两个窗口。
	a.winMu.Lock()
	defer a.winMu.Unlock()

	if existing := a.currentWindow(); existing != nil {
		existing.Show()
		existing.Focus()
		return existing
	}

	win := app.Window.NewWithOptions(mainWindowOptions())
	win.OnWindowEvent(events.Common.WindowClosing, func(*application.WindowEvent) {
		a.mu.Lock()
		if a.win == win {
			a.win = nil
		}
		a.mu.Unlock()
	})

	a.mu.Lock()
	a.win = win
	a.mu.Unlock()

	win.Show()
	win.Focus()
	return win
}

// currentWindow 安全地读当前窗口句柄（可能为 nil）。
func (a *App) currentWindow() *application.WebviewWindow {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.win
}

// ShowMainWindow 显示主界面：窗口还在就拉前台，已经被销毁就重建。
// 托盘左键、托盘菜单、单实例第二次启动都走这里。
func (a *App) ShowMainWindow() {
	a.mu.Lock()
	app := a.app
	win := a.win
	a.mu.Unlock()

	if app == nil {
		return
	}
	if win != nil {
		win.UnMinimise()
		win.Show()
		win.Focus()
		return
	}
	a.createMainWindow(app)
}

// QuitFromTray 托盘菜单里的「退出」：这里才是真正结束进程。
//
// 先把后台收干净（取消 rootCtx、杀掉 RE / yt-dlp / ffmpeg），再让 app.Quit()
// 走自己的清理。反过来的话 Quit 会等 ServiceShutdown，用户会觉得点了没反应。
func (a *App) QuitFromTray() {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return
	}
	a.shutdown()
	app.Quit()
}

// IsInTray 报告当前是否处于轻量模式（无窗口）。
func (a *App) IsInTray() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.win == nil
}

// UpdateTrayHint 把任务汇总到托盘提示上。
//
// 轻量模式下这是用户唯一能看到的信息，所以必须更新；但进度事件一秒几十次，
// 每次都跨线程 SetTooltip 会把托盘线程压垮，所以节流到 800ms 一次，
// 且文本没变就完全不发。
func (a *App) UpdateTrayHint() {
	a.mu.Lock()
	tray := a.tray
	if tray == nil {
		a.mu.Unlock()
		return
	}
	if time.Since(a.trayHintAt) < 800*time.Millisecond {
		a.mu.Unlock()
		return
	}
	a.trayHintAt = time.Now()

	running, queued, finished := 0, 0, 0
	for _, id := range a.order {
		t := a.tasks[id]
		if t == nil {
			continue
		}
		switch t.Status {
		case statusRunning, statusTranscoding:
			running++
		case statusQueued:
			queued++
		case statusDone:
			finished++
		}
	}
	total := len(a.order)
	a.mu.Unlock()

	// 锁外再动托盘：SetTooltip 是 InvokeSync 到主线程，持锁调用有死锁风险。
	hint := "HLSink · 空闲"
	switch {
	case running > 0 && queued > 0:
		hint = fmt.Sprintf("HLSink · %d 个下载中 · %d 个排队", running, queued)
	case running > 0:
		hint = fmt.Sprintf("HLSink · %d 个下载中", running)
	case queued > 0:
		hint = fmt.Sprintf("HLSink · %d 个排队", queued)
	case total > 0 && finished == total:
		hint = fmt.Sprintf("HLSink · %d 个任务已完成", finished)
	}

	a.mu.Lock()
	changed := hint != a.trayHintText
	if changed {
		a.trayHintText = hint
	}
	a.mu.Unlock()
	if changed {
		tray.SetTooltip(hint)
	}
}
