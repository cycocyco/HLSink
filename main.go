package main

import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 纯静态前端（无 npm / 无 Vite），直接烘焙进 exe。
//
//go:embed all:frontend/dist
var assets embed.FS

func main() {
	core := NewApp()

	// 诊断开关：打印本 exe 内绑定的真实全限定名（前端 ByName 必须与之一致）。
	// 用法：HLSink.exe --print-fqn
	// 注意：go test 里同一个类型的 PkgPath 是模块名（hlsink），与 exe 的 "main" 不同，
	// 拿测试结果来对照会得出错误结论。
	if len(os.Args) > 1 && os.Args[1] == "--print-fqn" {
		typ := reflect.ValueOf(core).Type()
		fmt.Printf("PkgPath=%q TypeName=%q\n", typ.Elem().PkgPath(), typ.Elem().Name())
		return
	}

	// 所有运行期产物（配置、缓存、WebView2）全部收敛到 exe 同级的 data 目录，
	// 保持绿色便携，不往 C 盘 AppData 里塞东西。
	dataDir := appDataDir()

	app := application.New(application.Options{
		Name:        "HLSink",
		Description: "HLS / DASH 视频下载器",
		Services: []application.Service{
			application.NewService(core),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Windows: application.WindowsOptions{
			// 关掉「最后一个窗口关闭即退出」：点 ❌ 之后进程要留在托盘里继续跑任务。
			// 没有这一项，窗口一销毁消息循环就收到 WM_QUIT，托盘根本来不及用。
			DisableQuitOnLastWindowClosed: true,
			// 彻底杜绝 WebView2 在 %LOCALAPPDATA% 下生成几百兆缓存。
			WebviewUserDataPath: filepath.Join(dataDir, "webview2"),
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "HLSink_SingleInstance_Mutex_Lock",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				// 第二次启动时把界面叫出来：窗口可能还在，也可能已经被销毁（托盘模式）。
				core.ShowMainWindow()
			},
		},
	})

	core.attachApp(app)
	// 托盘必须先建好再开窗口：万一窗口起来就崩，用户至少还能从托盘退出。
	core.setupTray(app)
	core.createMainWindow(app)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}

	// app.Run 返回后主循环已结束，补一次兜底收尾（幂等）。
	core.shutdown()
}
