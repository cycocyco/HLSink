# HLSink

HLS / DASH 视频下载器。Go + Wails v3 + 纯静态前端（无 npm、无 Vite、无打包器）。

## 功能

- 浏览器扩展嗅探 HLS / DASH，发送到本地 `127.0.0.1:18752`
- N_m3u8DL-RE 优先下载，遇到 Cloudflare 自动降级 yt-dlp
- 下载后可选 NVENC 硬编转 HEVC（体积减半）
- 任务暂停 / 续传 / 物理删除（含精确产物清理）
- 迷你模式（置顶小窗）
- 外部工具路径可配置，不写死
- 所有运行期产物收敛在 exe 同级 `data/`，不污染 C 盘

## 目录

```
HLSink/
├── main.go                 # Wails v3 入口：应用 + 无边框窗口 + 单实例
├── task.go                 # App / Settings / 任务生命周期 / 清缓存
├── task_store.go           # 任务状态机与事件广播
├── task_runner.go          # RE / yt-dlp / ffmpeg 调度与进度解析
├── task_server.go          # 本地 HTTP 接口（扩展用）
├── task_paths.go           # 路径安全 / 发布 / 删除 / data 目录
├── task_stream.go          # 源格式判定
├── protocol_windows.go     # hlsink:// 协议注册
├── frontend/dist/          # 纯静态前端（直接 go:embed）
│   ├── index.html
│   ├── app.js
│   ├── app.css
│   ├── style.css
│   ├── vendor/wails-runtime.js  # Wails 运行时（随前端打包，不依赖内置路由）
│   └── assets/fonts/
├── extension/              # Chromium 扩展
└── build/bin/HLSink.exe    # 构建产物
```

## 构建

```bash
# 可选：指向本机已有的 Go 模块缓存（路径换成你自己的）
export GOPATH=/path/to/go_path
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE=/path/to/go_cache
export GOPROXY=off GOSUMDB=off

go build -tags desktop,production -ldflags="-H windowsgui -w -s" -o build/bin/HLSink.exe .
```

`-H windowsgui` 去掉控制台黑框；`-w -s` 去符号表。

## 数据与缓存

全部在 **exe 同级 `data/`**（exe 目录不可写时回退用户配置目录）：

- `data/config.json` —— 用户设置（任务、下载目录、工具路径）
- `data/work/<任务ID>/` —— 分片、临时文件、日志
- `data/webview2/` —— WebView2 缓存

设置页「清理缓存」只删 `data/work`，**不会**动 `config.json` 和你的成品视频。有任务运行时会拒绝清理。

## 外部工具

三个工具都可留空（自动检测）：会依次查找 exe 旁 `tools/`、`deps/`、`bin/`，再查系统 `PATH`。

- `N_m3u8DL-RE.exe` —— HLS / DASH 分片下载
- `yt-dlp.exe` —— 网页解析 / CF 降级
- `ffmpeg.exe` —— 转码与 mux（可填 exe 路径或所在目录）

## 前端调用约定

前端不走生成的 bindings，直接用 Wails v3 内置运行时：

```js
const { Call, Events, Window } = await import('./vendor/wails-runtime.js');

// 命名空间固定为 main.App（go build 出的 main 包 PkgPath 就是 main）
Call.ByName('main.App.ListTasks');
Call.ByName('main.App.PauseTask', id);

// 事件
Events.On('task-updated', (ev) => console.log(ev.data));

// 窗口
Window.Minimise();
Window.SetSize(400, 150);
```

因此新增后端方法时，只要在 `app.js` 的 `AppApi` 里补一行即可，不需要重新生成绑定。

命名空间必须是 `main.App.`。注意 `go test` 里同一个类型的 PkgPath 会变成模块名，
与 exe 不同——别拿测试结果当准，用 `--print-fqn` 确认。

## 图标

- 母版：`build/icon-source.png`（Hlink 字标）
- 应用图标：`build/appicon.png`（1024×1024，白字标 + 黑色圆角底）
- Windows 图标：`build/windows/icon.ico`（256/128/64/48/32/24/16；16/24 单独放大了字标）
- 清单：`build/windows/wails.exe.manifest`
- 资源对象：`wails_windows_amd64.syso`（Go 构建时自动链接，负责把图标写进 exe）

换图标时，替换 `build/icon-source.png` 后重新生成：

```bash
# 1024 母版 + 多尺寸 ico（脚本见项目历史，或按 README 尺寸规范自行导出）
wails3 generate icons -input build/appicon.png -sizes 256,128,64,48,32,24,16 \
  -windowsfilename build/windows/icon.ico

# 重新生成资源对象（必须有，否则 exe 里还是默认图标）
wails3 generate syso -arch amd64 \
  -icon build/windows/icon.ico \
  -manifest build/windows/wails.exe.manifest \
  -out wails_windows_amd64.syso
```
