# HLSink

一个 Windows 上的视频下载器。浏览器里能播的 HLS / DASH 视频，点一下扩展把链接丢过来，它负责下完、合并、可选转码，最后落到你指定的文件夹。

名字是 HLS + Sink（顺手倒进槽里）。

## 它到底在干什么

1. 浏览器扩展盯着页面里的网络请求，看到 `.m3u8` / `.mpd` 就记下来，连 cookie、referer 一起带上——很多站不带这些下不动
2. 主程序拿到链接，调 **N_m3u8DL-RE** 去下分片
3. 被 Cloudflare 挡住的时候，换 **yt-dlp** 再试一次
4. 下完可以选转 HEVC，用显卡硬编，体积差不多减半
5. 成品挪到设置里的下载目录，重名自动加 (1)(2)，不覆盖

下载、合并、转码全是别人的工具，这个项目只负责把它们串起来，加一个自己画的界面。

## 用之前得自己准备三个工具

程序里不带引擎，这三个自己下好放进来（放 exe 旁边的 `tools/`、`deps/`、`bin/` 都行，也可以在设置里直接指路径）：

| 工具 | 管什么 | 缺了会怎样 |
|---|---|---|
| `N_m3u8DL-RE.exe` | HLS / DASH 分片下载 | 下不了，会明确提示你没找到 |
| `yt-dlp.exe` | 网页解析、Cloudflare 兜底 | 只能下直链，网页地址没法处理 |
| `ffmpeg.exe` | 合并音视频、转 HEVC | 不能转码；yt-dlp 那条分支也用不了 |

三个都能留空自动探测，顺序是 exe 旁的 `tools` → `deps` → `bin`，最后查系统 PATH。

## 怎么用

1. 打开 `HLSink.exe`，在设置里选好下载目录（默认是 `用户目录\Downloads`）
2. Edge / Chrome 装 `extension/` 里的扩展（开发者模式 → 加载解压的扩展）
3. 在网页上播视频，点扩展图标，它会列出嗅到的流，选一个丢给程序

不想装扩展也行，主界面的「添加链接」可以直接粘地址：网页地址、`.m3u8`、`.mpd` 都吃。

## 点 ❌ 不等于退出

右上角那个 ❌ 是「收进托盘」，不是关软件。点了之后窗口和整个 WebView 会被销毁——是真的销毁，不是藏起来，任务管理器里那几个 `msedgewebview2.exe` 会消失，常驻内存掉到几 MB。下载、转码、扩展推送这些后台活照常干。

- 托盘图标**左键**：把界面叫回来（重新建窗口，几百毫秒）
- 托盘图标**右键**：「显示主界面」「退出 HLSink」
- 鼠标悬停在托盘图标上：能看到「2 个下载中 · 1 个排队」这类状态

挂着几个大文件慢慢下的时候直接把界面关掉，这就是它最省资源的用法。

## 编译

```bash
go build -tags desktop,production -ldflags="-H windowsgui -w -s" -o build/bin/HLSink.exe .
```

`-H windowsgui` 去掉控制台黑框，`-w -s` 去符号表。

依赖都是公开模块，`go mod download` 就行。本机已经有 Go 模块缓存的话可以指过去省下载：

```bash
export GOPATH=/path/to/go_path
export GOMODCACHE="$GOPATH/pkg/mod"
export GOCACHE=/path/to/go_cache
export GOPROXY=off GOSUMDB=off
```

有个诊断开关，能打印当前 exe 里绑定的真实名字（前端调用名必须和它一致）：

```bash
./build/bin/HLSink.exe --print-fqn
# PkgPath="main" TypeName="App"
```

## 文件都放哪

便携的。运行期产生的东西全在 exe 同级的 `data/` 里，不往 C 盘塞：

- `data/config.json` —— 你的设置
- `data/work/<任务ID>/` —— 分片、临时文件、日志
- `data/webview2/` —— WebView2 自己的缓存

设置页的「清理缓存」只删 `data/work`，不碰配置，也不碰你已经下好的视频。有任务在跑时会拒绝清理，免得删掉正在写的分片。

整个文件夹可以拷到别的机器直接用。

## 改代码的话看这里

前端不走 Wails 的代码生成，直接用它的运行时，所以没有 npm、没有 node_modules、没有构建步骤。`frontend/dist` 里的文件由 `go:embed` 打进 exe，改完重新 `go build` 就行。

```js
const { Call, Events, Window } = await import('./vendor/wails-runtime.js');

const APP_NS = 'main.App.';
const call = (method, ...args) => Call.ByName(APP_NS + method, ...args);

call('ListTasks');
call('PauseTask', id);

Events.On('task-updated', (ev) => console.log(ev.data));

Window.Minimise();
Window.SetSize(400, 150);
```

加后端方法只需要在 `app.js` 的 `AppApi` 里补一行，不用生成绑定。

`APP_NS` 这里有个坑：`go build` 出 exe 时 `package main` 的包路径是 `main`，所以是 `main.App.Xxx`；但在 `go test` 里同一个类型会变成模块名 `hlsink`。两个不一样，别拿测试打印出来的名字当准，用 `--print-fqn` 问 exe 自己。

## 目录

```
HLSink/
├── main.go                 # 入口：应用、无边框窗口、单实例
├── task.go                 # App / 设置 / 任务生命周期 / 清缓存
├── task_store.go           # 状态机与事件广播
├── task_runner.go          # 编排：排队、选引擎、失败降级、发布成品
├── task_server.go          # 给扩展用的本地 HTTP 接口
├── task_paths.go           # 路径安全、发布、删除
├── internal/engine/        # 外部工具模块，每个工具一个文件
│   ├── engine.go           # 接口：Downloader / Transcoder
│   ├── registry.go         # 注册表（按优先级排降级链）
│   ├── kind.go             # 判断是 hls / dash / 网页
│   ├── tools.go            # 找 exe：填目录、填 exe、留空自动找
│   ├── run.go              # 跑进程、停滞看门狗、杀进程树
│   ├── headers.go          # 请求头清洗
│   ├── downloader_re.go    # N_m3u8DL-RE
│   ├── downloader_ytdlp.go # yt-dlp
│   └── transcoder_ffmpeg.go# ffmpeg
├── protocol_windows.go     # 注册 hlsink:// 协议
├── frontend/dist/          # 前端源码（就是发布文件，没有编译步骤）
│   ├── index.html
│   ├── app.js
│   ├── app.css / style.css
│   ├── vendor/wails-runtime.js
│   └── assets/fonts/
├── extension/              # Chromium 扩展
└── build/                  # 图标与清单
```

外部工具的调用全部关在 `internal/engine` 里，编排层（`task_runner.go`）不出现任何具体工具的参数。想接第四个工具（比如 tdl、aria2）：

1. 新建一个 `internal/engine/downloader_xxx.go`，实现 `Downloader` 那几个方法，`init()` 里 `Register(...)`
2. `Settings` 加一个路径字段，`App.toolMap()` 补一行映射
3. 前端设置页加一个输入框

参数怎么拼、进度怎么读、多久算卡死，全在新文件里，编排层不用动。


## 图标

- `build/icon-source.png` —— 原始字标
- `build/appicon.png` —— 1024×1024 成品
- `build/windows/icon.ico` —— 256/128/64/48/32/24/16 七个尺寸
- `wails_windows_amd64.syso` —— 真正让图标进 exe 的东西

只放 ico 不生成 syso 的话，exe 里还是 Go 的默认图标。换图标的流程：

```bash
wails3 generate icons -input build/appicon.png -sizes 256,128,64,48,32,24,16 \
  -windowsfilename build/windows/icon.ico

wails3 generate syso -arch amd64 \
  -icon build/windows/icon.ico \
  -manifest build/windows/wails.exe.manifest \
  -out wails_windows_amd64.syso
```

## 几个实话

- **只能下能拿到直连地址的视频**。DRM 加密的（Netflix、Disney+ 那类）搞不定，也不打算搞
- **线程数别一味开大**。默认 32，有些站直接 429，调小反而更快
- **转码要 NVIDIA 显卡**。用的是 `hevc_nvenc`，没有 N 卡就在设置里关掉，只下载不转
- **暂停靠杀进程**，所以是分片级续传，不是字节级
- **删除是物理删除**，连成品一起删，删完不可恢复，删除前会确认

用出问题的时候，日志在 `data/work/<任务ID>/logs/`。
