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
			// 彻底杜绝 WebView2 在 %LOCALAPPDATA% 下生成几百兆缓存。
			WebviewUserDataPath: filepath.Join(dataDir, "webview2"),
		},
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "HLSink_SingleInstance_Mutex_Lock",
			OnSecondInstanceLaunch: func(data application.SecondInstanceData) {
				// 第二次启动时把已有窗口拉到前台，而不是再开一个。
				core.bringToFront()
			},
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
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
	})

	// 把应用与窗口句柄回传给业务层，彻底告别 v2 的 context 传参。
	core.attach(app, win)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}

	// app.Run 返回后主循环已结束，补一次兜底收尾（幂等）。
	core.shutdown()
}
