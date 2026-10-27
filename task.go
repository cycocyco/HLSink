package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// 任务状态。前端依赖这些字符串来显示文案/按钮，改动需同步 UI。
const (
	statusQueued        = "queued"         // 排队中（等待并发槽位）
	statusRunning       = "running"        // 下载中
	statusPausing       = "pausing"        // 正在暂停（前端可短暂显示）
	statusTranscoding   = "transcoding"    // 转码中
	statusDeleting      = "deleting"       // 正在删除
	statusPaused        = "paused"         // 已暂停（分片保留，可续传）
	statusCleanupFailed = "cleanup_failed" // 删除时文件清理失败，任务保留可重试
	statusDone          = "done"           // 完成
	statusError         = "error"          // 失败
)

// 下载线程数默认值与上下限。HLS 站点通常按连接限速，适当提高并发能吃满带宽；
// 站点严格限流时可让用户下调，避免 429。
const (
	defaultThreadCount = 32
	minThreadCount     = 1
	maxThreadCount     = 64
)

// maxActiveTasks 队列上限（非终态任务数），防止无限堆积。
const maxActiveTasks = 100

// Task 表示一个下载任务。
//
// 导出字段会下发给前端（保持旧字段兼容）；内部调度字段一律 json:"-"，
// 既不会泄露绝对路径，也防止客户端通过 HTTP/Wails 注入了不该由外部决定的字段。
type Task struct {
	ID       string            `json:"id"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	Referer  string            `json:"referer,omitempty"`
	Title    string            `json:"title,omitempty"`
	Filename string            `json:"filename,omitempty"`
	Status   string            `json:"status"`
	Progress float64           `json:"progress"`
	Speed    string            `json:"speed,omitempty"`
	Segments string            `json:"segments,omitempty"` // 已下载/总片数 如 "12/1760"
	Message  string            `json:"message,omitempty"`

	// 结果与诊断信息。OutputPath 是发布到保存目录后的精确成品路径，删除任务只删它。
	DownloadPath string    `json:"downloadPath,omitempty"` // 任务 staging 内的原始产物
	OutputPath   string    `json:"outputPath,omitempty"`   // 保存目录内的最终成品
	Engine       string    `json:"engine,omitempty"`       // "re" / "yt-dlp"
	Kind         string    `json:"kind,omitempty"`         // 源格式：hls / dash / page（决定用哪个引擎）
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`

	// 以下字段仅在进程内使用：不下发、不接受注入。
	RunID   string `json:"-"` // 当前 run 的唯一标识，用于丢弃过期 run 的写回
	WorkDir string `json:"-"` // 任务私有工作目录（下载/分片/转码/日志）
	SaveDir string `json:"-"` // 创建任务时的下载目录快照

	transcode   bool // 创建任务时的 HEVC 开关快照
	threadCount int  // 创建任务时的下载线程数快照
}

// clone 返回任务的深拷贝（含 Headers），用于列表与事件快照，避免数据竞争。
func (t *Task) clone() *Task {
	if t == nil {
		return nil
	}
	c := *t
	if t.Headers != nil {
		h := make(map[string]string, len(t.Headers))
		for k, v := range t.Headers {
			h[k] = v
		}
		c.Headers = h
	}
	return &c
}

// Settings 用户设置。hevctTranscode 是历史字段名，保留以兼容旧配置文件。
//
// REPath / YtdlpPath / FFmpegPath 是外部工具路径（不再写死）：
// 可填完整 exe 路径、所在目录，或 PATH 里的命令名；留空则自动在 exe 旁
// tools/deps/bin 目录与系统 PATH 中查找。
type Settings struct {
	HEVCTranscode bool   `json:"hevctTranscode"`
	DownloadDir   string `json:"downloadDir"`
	ThreadCount   int    `json:"threadCount"`
	REPath        string `json:"rePath"`
	YtdlpPath     string `json:"ytdlpPath"`
	FFmpegPath    string `json:"ffmpegPath"`
}

// App Wails 绑定的应用对象。
type App struct {
	// v3 不再需要 context：直接持有应用与主窗口句柄。
	app *application.App
	win *application.WebviewWindow

	mu       sync.Mutex
	tasks    map[string]*Task
	order    []string
	runs     map[string]*runState // taskID -> 当前 run
	settings Settings
	mini     bool
	pinned   bool // 迷你模式是否置顶（用户可在 mini 卡片上切换）
	server   *httpServer

	rootCtx    context.Context
	cancelRoot context.CancelFunc

	downloadSem  chan struct{} // 下载并发槽位
	transcodeSem chan struct{} // 转码并发槽位

	// 缓存目录统计：遍历可能很慢（几万个分片），所以放到后台算，
	// 调用方最多等 cacheWaitTimeout，拿不到就先返回上次结果并标记 pending。
	cacheMu      sync.Mutex
	cacheBytes   int64
	cacheRunning bool
	cacheDone    chan struct{}
	cacheGen     uint64

	shutdownOnce sync.Once
}

// attach 注入 v3 的应用与窗口句柄（在 application.New 之后调用）。
func (a *App) attach(app *application.App, win *application.WebviewWindow) {
	a.mu.Lock()
	a.app = app
	a.win = win
	a.mu.Unlock()
}

// bringToFront 把主窗口重新拉到前台（单实例第二次启动时使用）。
func (a *App) bringToFront() {
	a.mu.Lock()
	win := a.win
	a.mu.Unlock()
	if win == nil {
		return
	}
	win.UnMinimise()
	win.Show()
	win.Focus()
}

// NewApp 构造应用实例。
func NewApp() *App {
	rootCtx, cancel := context.WithCancel(context.Background())
	return &App{
		pinned:       true,
		tasks:        map[string]*Task{},
		runs:         map[string]*runState{},
		downloadSem:  make(chan struct{}, downloadConcurrency),
		transcodeSem: make(chan struct{}, transcodeConcurrency),
		rootCtx:      rootCtx,
		cancelRoot:   cancel,
		settings: Settings{
			HEVCTranscode: true,
			DownloadDir:   defaultDownloadDir(),
			ThreadCount:   defaultThreadCount,
			// 工具路径默认留空 = 自动探测（exe 旁 tools/deps/bin + 系统 PATH）。
			REPath:     "",
			YtdlpPath:  "",
			FFmpegPath: "",
		},
	}
}

// defaultDownloadDir 返回系统下载目录。
func defaultDownloadDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return filepath.Join(home, "Downloads")
}

// normalizeThreadCount 把线程数限制在安全范围内；0/负数视为默认 32。
func normalizeThreadCount(n int) int {
	if n <= 0 {
		return defaultThreadCount
	}
	if n < minThreadCount {
		return minThreadCount
	}
	if n > maxThreadCount {
		return maxThreadCount
	}
	return n
}

// ServiceStartup 由 Wails v3 在应用启动时调用（实现 ServiceStartup 接口）。
func (a *App) ServiceStartup(_ context.Context, _ application.ServiceOptions) error {
	a.loadConfig()
	if err := registerURLProtocol(); err != nil {
		// 协议注册失败不应影响下载主流程；扩展的“启动 HLSink”按钮会退化为提示。
		println("register URL protocol failed:", err.Error())
	}
	go a.startTaskServer()
	return nil
}

// ServiceShutdown 由 Wails v3 在应用退出时调用：取消任务、关 HTTP、杀光外部进程。
func (a *App) ServiceShutdown() error {
	a.shutdown()
	return nil
}

// shutdown 收尾：可重复调用，只会执行一次。
//
// 关键约束：这个函数在关窗路径上，必须几乎瞬时返回。
// 因此只做“发起”动作（取消 context、下发 taskkill），不等待任何结果：
// 进程退出后 socket 与子进程都会被系统回收，等在这里只会让点 X 变卡。
func (a *App) shutdown() {
	a.shutdownOnce.Do(func() {
		a.cancelRoot()

		a.mu.Lock()
		srv := a.server
		runs := make([]*runState, 0, len(a.runs))
		for _, rs := range a.runs {
			runs = append(runs, rs)
		}
		a.mu.Unlock()

		if srv != nil {
			go srv.close(200 * time.Millisecond)
		}
		for _, rs := range runs {
			rs.cancel()
			// killAll 内部只发起 taskkill（不等待），所以这里同步调用也不会卡。
			rs.killAll()
		}
	})
}

// ServerStatus 返回扩展接口是否已就绪，供前端状态灯精确判断（免去 CORS 探测）。
func (a *App) ServerStatus() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.server != nil
}

// currentSettings 返回设置快照（用于工具路径等只读场景）。
func (a *App) currentSettings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

// ---- 设置持久化 ----

// configPath 返回配置文件路径：<exe目录>/data/config.json（不可写时回退用户目录）。
func configPath() string {
	return filepath.Join(appDataDir(), "config.json")
}

// loadConfig 读回配置，失败时保持默认值（不阻塞启动）。
func (a *App) loadConfig() {
	data, err := os.ReadFile(configPath())
	if err != nil {
		return
	}
	var s Settings
	if err := json.Unmarshal(data, &s); err != nil {
		return
	}
	// 用原始键集合判断字段是否存在：配置里没有 hevctTranscode 时保持默认开启，
	// 否则 json 会把缺失字段解成 false，静默把转码关掉。
	var present map[string]json.RawMessage
	_ = json.Unmarshal(data, &present)

	a.mu.Lock()
	if strings.TrimSpace(s.DownloadDir) != "" {
		a.settings.DownloadDir = s.DownloadDir
	}
	if _, ok := present["hevctTranscode"]; ok {
		a.settings.HEVCTranscode = s.HEVCTranscode
	}
	a.settings.ThreadCount = normalizeThreadCount(s.ThreadCount)
	a.settings.REPath = strings.TrimSpace(s.REPath)
	a.settings.YtdlpPath = strings.TrimSpace(s.YtdlpPath)
	a.settings.FFmpegPath = strings.TrimSpace(s.FFmpegPath)
	a.mu.Unlock()
}

// persistSettings 原子保存配置：写临时文件 + fsync + rename，并检查每一步错误。
func (a *App) persistSettings(s Settings) error {
	p := configPath()
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建配置目录失败: %w", err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "config-*.json.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时配置失败: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("同步临时配置失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置失败: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("替换配置文件失败: %w", err)
	}
	return nil
}

// ---- Wails 绑定：给前端调用 ----

// ListTasks 返回任务列表深拷贝快照。
func (a *App) ListTasks() []*Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Task, 0, len(a.order))
	for _, id := range a.order {
		if t, ok := a.tasks[id]; ok {
			out = append(out, t.clone())
		}
	}
	return out
}

// AddTask 手动添加一个链接（网页地址、HLS/DASH 地址均可）。
//
// 校验规则与 HTTP /task 完全一致：只允许 http/https，格式由服务端判定。
// 网页地址会交给 yt-dlp 解析站点内的视频；返回新建（或已存在）任务的 ID。
func (a *App) AddTask(rawURL, title string) (string, error) {
	input, err := normalizeTaskRequest(&TaskRequest{URL: rawURL, Title: title})
	if err != nil {
		return "", err
	}
	id, _, err := a.createTask(input)
	if err != nil {
		return "", err
	}
	return id, nil
}

// GetSettings 返回当前设置。
func (a *App) GetSettings() Settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.settings
}

// SaveSettings 校验并持久化设置；返回保存后的设置与可能的错误。
// 设置变更只影响之后创建的任务（每个任务创建时会快照保存目录与转码开关）。
func (a *App) SaveSettings(s Settings) (Settings, error) {
	dir := strings.TrimSpace(s.DownloadDir)
	if dir != "" {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return a.GetSettings(), fmt.Errorf("下载目录无效: %w", err)
		}
		if err := os.MkdirAll(abs, 0o755); err != nil {
			return a.GetSettings(), fmt.Errorf("下载目录不可用: %w", err)
		}
		dir = abs
	}

	a.mu.Lock()
	if dir != "" {
		a.settings.DownloadDir = dir
	}
	a.settings.HEVCTranscode = s.HEVCTranscode
	if s.ThreadCount > 0 {
		a.settings.ThreadCount = normalizeThreadCount(s.ThreadCount)
	} else if a.settings.ThreadCount <= 0 {
		a.settings.ThreadCount = defaultThreadCount
	}
	a.settings.REPath = strings.TrimSpace(s.REPath)
	a.settings.YtdlpPath = strings.TrimSpace(s.YtdlpPath)
	a.settings.FFmpegPath = strings.TrimSpace(s.FFmpegPath)
	saved := a.settings
	a.mu.Unlock()

	if err := a.persistSettings(saved); err != nil {
		return saved, err
	}
	a.emit("settings-changed", saved)
	return saved, nil
}

// ToggleMini 切换正常/迷你窗口形态。迷你模式置顶，还原时取消置顶。
func (a *App) ToggleMini() {
	a.mu.Lock()
	a.mini = !a.mini
	mini := a.mini
	win := a.win
	onTop := a.pinned
	a.mu.Unlock()

	if win != nil {
		if mini {
			win.SetAlwaysOnTop(onTop)
			// 先放开最小尺寸，否则 SetSize 会被主窗口的最小宽高顶住。
			win.SetMinSize(260, 72)
			win.SetSize(400, 150)
		} else {
			win.SetAlwaysOnTop(false)
			win.SetMinSize(480, 420)
			win.SetSize(640, 620)
		}
	}
	// 窗口尺寸先就位再通知前端；前端会根据是否空闲进一步收成 300x84。
	a.emit("mini-changed", mini)
}

// ToggleAlwaysOnTop 切换窗口置顶，返回切换后的状态（迷你模式的图钉按钮）。
func (a *App) ToggleAlwaysOnTop() bool {
	a.mu.Lock()
	win := a.win
	a.pinned = !a.pinned
	on := a.pinned
	a.mu.Unlock()

	if win != nil {
		win.SetAlwaysOnTop(on)
	}
	return on
}

// AlwaysOnTop 返回当前置顶状态，供前端初始化图钉高亮。
func (a *App) AlwaysOnTop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pinned
}

// PickFolder 弹 Windows 原生目录选择对话框（下载目录）。
func (a *App) PickFolder() string {
	return a.pickDirectory("选择下载文件夹")
}

// PickToolFolder 弹目录选择框，用于指定外部工具（RE / yt-dlp / ffmpeg）所在文件夹。
func (a *App) PickToolFolder() string {
	return a.pickDirectory("选择工具所在文件夹")
}

// pickDirectory 统一的目录选择实现。
func (a *App) pickDirectory(title string) string {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return ""
	}
	dialog := app.Dialog.OpenFile()
	dialog.SetTitle(title)
	dialog.CanChooseDirectories(true)
	dialog.CanChooseFiles(false)
	dir, err := dialog.PromptForSingleSelection()
	if err != nil {
		return ""
	}
	return dir
}

// cacheWaitTimeout 是 CacheInfo 愿意同步等待的时长。
// 超过就立刻返回上次结果并标记 pending，由前端稍后重试，绝不把 UI 卡在遍历上。
const cacheWaitTimeout = 200 * time.Millisecond

// CacheInfo 返回缓存目录与占用字节数（用于设置页展示）。
//
// 目录遍历在后台 goroutine 里做（同一时刻只跑一个），本调用最多等
// cacheWaitTimeout。返回值里的 pending 表示"还在统计，数字可能偏旧"。
func (a *App) CacheInfo() map[string]interface{} {
	root := workRoot()

	a.cacheMu.Lock()
	if !a.cacheRunning {
		a.cacheRunning = true
		gen := a.cacheGen
		done := make(chan struct{})
		a.cacheDone = done
		go func() {
			size, _ := dirSize(root)
			a.cacheMu.Lock()
			if a.cacheGen == gen {
				a.cacheBytes = size
			}
			a.cacheRunning = false
			close(done)
			a.cacheMu.Unlock()
		}()
	}
	done := a.cacheDone
	a.cacheMu.Unlock()

	pending := true
	if done != nil {
		timer := time.NewTimer(cacheWaitTimeout)
		select {
		case <-done:
			pending = false
		case <-timer.C:
		}
		timer.Stop()
	}

	a.cacheMu.Lock()
	size := a.cacheBytes
	a.cacheMu.Unlock()

	return map[string]interface{}{
		"path":    root,
		"bytes":   size,
		"pending": pending,
	}
}

// ClearCache 清理下载缓存（data/work），但保留 config.json 与用户成品。
//
// 有任务正在运行 / 收尾时拒绝清理，避免删掉正在写入的分片。
func (a *App) ClearCache() (int64, error) {
	a.mu.Lock()
	running := len(a.runs)
	a.mu.Unlock()
	if running > 0 {
		return 0, fmt.Errorf("有任务正在运行，请先暂停或等待完成再清理缓存")
	}

	root := workRoot()
	// 优先用后台统计到的数字，避免为了一次展示再全量遍历一遍。
	// 只有从未统计过（0）时才现算一次。
	a.cacheMu.Lock()
	size := a.cacheBytes
	a.cacheMu.Unlock()
	if size == 0 {
		size, _ = dirSize(root)
	}
	if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
		return 0, fmt.Errorf("清理缓存失败: %w", err)
	}
	// 重新建出空的 work 根，保证后续任务可直接使用。
	if err := os.MkdirAll(root, 0o755); err != nil {
		return 0, fmt.Errorf("重建缓存目录失败: %w", err)
	}

	// 让正在跑的旧统计失效，并立即把展示值归零。
	a.cacheMu.Lock()
	a.cacheGen++
	a.cacheBytes = 0
	a.cacheMu.Unlock()

	return size, nil
}

// dirSize 统计目录占用字节数（不存在返回 0）。
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目失败不影响整体统计
		}
		if d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return total, err
	}
	return total, nil
}

// PauseTask 暂停任务。
//
// 顺序（关键）：锁内校验并标 pausing → 取消 run（RunID 失效、context cancel）→
// 杀进程树 → 等旧 run 的 done → 状态改 paused。转码中不支持暂停（返回明确错误）。
func (a *App) PauseTask(id string) error {
	a.mu.Lock()
	t, ok := a.tasks[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("任务不存在: %s", id)
	}
	switch t.Status {
	case statusTranscoding:
		a.mu.Unlock()
		return fmt.Errorf("正在转码，无法暂停（转码不支持断点，请等待完成）")
	case statusDeleting, statusCleanupFailed:
		a.mu.Unlock()
		return fmt.Errorf("任务正在删除中，无法暂停")
	case statusDone, statusError, statusPaused, statusPausing:
		a.mu.Unlock()
		return nil // 幂等：终态或已暂停，无需处理
	}
	t.Status = statusPausing
	t.Speed = ""
	t.Segments = ""
	rs := a.runs[id]
	snap := t.clone()
	a.mu.Unlock()

	a.emit("task-updated", snap)

	if rs != nil {
		rs.cancel()
		rs.killAll()
		rs.waitDone(runStopTimeout)
	}

	a.mu.Lock()
	t2, exists := a.tasks[id]
	if exists && t2.Status == statusPausing {
		t2.Status = statusPaused
		t2.Speed = ""
		t2.Segments = ""
		t2.Message = "已暂停"
		done := t2.clone()
		a.mu.Unlock()
		a.emit("task-updated", done)
		return nil
	}
	a.mu.Unlock()
	return nil
}

// ResumeTask 恢复任务。只接受 paused 状态，锁内 CAS 为 queued 后启动新 run，
// 双击也不会多开（第二次调用已不是 paused）。
func (a *App) ResumeTask(id string) error {
	a.mu.Lock()
	t, ok := a.tasks[id]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("任务不存在: %s", id)
	}
	if t.Status != statusPaused {
		a.mu.Unlock()
		return fmt.Errorf("只有已暂停的任务可以继续（当前状态 %s）", t.Status)
	}
	if a.runs[id] != nil {
		a.mu.Unlock()
		return fmt.Errorf("任务仍在收尾，请稍后再试")
	}
	t.Status = statusQueued
	t.Message = ""
	t.Error = ""
	t.Speed = ""
	t.Segments = ""
	snap := t.clone()
	a.mu.Unlock()

	a.emit("task-updated", snap)
	return a.launchRun(id)
}

// DeleteTask 物理删除任务及其全部产物。
//
// 顺序：锁内标 deleting（禁止任何新 run）→ 取消 run、杀进程树 → 等 done →
// 删 workDir（严格校验在 app work root 内）→ 删精确 OutputPath → 残留检查。
// 全部成功才移除任务；有残留则保留任务并置 cleanup_failed，可再次调用重试删除。
// 绝不按名称/前缀扫描下载目录，绝不删未记录的文件。
func (a *App) DeleteTask(id string) error {
	a.mu.Lock()
	t, ok := a.tasks[id]
	if !ok {
		// 已经删掉了（可能是后台清理刚刚完成），对用户来说就是成功。
		a.mu.Unlock()
		return nil
	}
	if t.Status == statusDeleting {
		a.mu.Unlock()
		return fmt.Errorf("任务正在删除中")
	}
	t.Status = statusDeleting
	t.Speed = ""
	t.Segments = ""
	rs := a.runs[id]
	snap := t.clone()
	a.mu.Unlock()

	a.emit("task-updated", snap)

	if rs != nil {
		rs.cancel()
		rs.killAll()
		rs.waitDone(runStopTimeout)
	}

	// run 退出后再取一次精确路径：发布可能刚好完成，OutputPath 此时才是最终值。
	a.mu.Lock()
	var workDir, outPath, dlPath, saveDir string
	if cur, exists := a.tasks[id]; exists {
		workDir, outPath, dlPath, saveDir = cur.WorkDir, cur.OutputPath, cur.DownloadPath, cur.SaveDir
	}
	a.mu.Unlock()

	if workDir == "" {
		workDir = taskWorkDir(id)
	}

	var problems []string
	if err := removeWorkDir(workDir); err != nil {
		problems = append(problems, err.Error())
	}
	if err := removeOutputFile(outPath, saveDir); err != nil {
		problems = append(problems, err.Error())
	}
	if dlPath != "" && !samePath(dlPath, outPath) {
		if err := removeOutputFile(dlPath, workDir); err != nil {
			problems = append(problems, err.Error())
		}
	}

	// 残留检查：任务消失前，任何记录过的路径都必须真的不存在。
	if stillExists(workDir) {
		problems = append(problems, "工作目录仍然存在: "+workDir)
	}
	if outPath != "" && stillExists(outPath) {
		problems = append(problems, "成品仍然存在: "+outPath)
	}
	if dlPath != "" && stillExists(dlPath) {
		problems = append(problems, "中间产物仍然存在: "+dlPath)
	}

	if len(problems) > 0 {
		msg := strings.Join(problems, "；")
		a.mutate(id, nil, func(x *Task) {
			x.Status = statusCleanupFailed
			x.Error = msg
			// 卡片上别甩一长串 unlink 报错，用户只需要知道"还在处理"。
			x.Message = "文件被占用，正在后台清理"
			x.Speed = ""
		})
		// 失败大概率是句柄还没释放（Windows 上很常见），后台再收拾几轮，
		// 成功后直接把任务从列表移除，用户不用盯着这行红字再点一次。
		a.scheduleLateCleanup(id, workDir, outPath, dlPath, saveDir)
		// 对用户而言这次删除已经受理了：任务已经从"下载中"变成"清理中"，
		// 后台会继续删。不返回错误，避免弹一个"删除失败"然后又自己消失。
		return nil
	}

	a.finalizeRemove(id)
	return nil
}

// finalizeRemove 把任务从内存列表移除并通知前端。可重复调用（幂等）。
func (a *App) finalizeRemove(id string) {
	a.mu.Lock()
	_, exists := a.tasks[id]
	delete(a.tasks, id)
	delete(a.runs, id)
	for i, x := range a.order {
		if x == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	a.mu.Unlock()

	if exists {
		a.emit("task-removed", id)
	}
}

// scheduleLateCleanup 删除失败后在后台继续收拾残局。
//
// 典型现场：yt-dlp 的 download.mp4.part 刚被 taskkill 结束，句柄还挂在
// 系统里（杀软/索引也会短暂占用），几秒后再删就干净了。这里按递增间隔
// 重试，全部成功才把任务移出列表，避免"任务没了但垃圾还在"。
func (a *App) scheduleLateCleanup(id, workDir, outPath, dlPath, saveDir string) {
	go func() {
		for _, wait := range []time.Duration{3 * time.Second, 7 * time.Second, 15 * time.Second, 30 * time.Second} {
			time.Sleep(wait)
			// 任务已被别的路径删掉（用户又点了一次删除）就不再处理。
			if a.taskSnapshot(id) == nil {
				return
			}
			if err := removeWorkDir(workDir); err != nil {
				continue
			}
			if err := removeOutputFile(outPath, saveDir); err != nil {
				continue
			}
			if dlPath != "" && !samePath(dlPath, outPath) {
				if err := removeOutputFile(dlPath, workDir); err != nil {
					continue
				}
			}
			a.finalizeRemove(id)
			return
		}
		// 四轮都没删掉：把文案切回"需要人工再点一次"，任务仍留在列表里，
		// 绝不假装删成功（文件确实还在）。
		if a.taskSnapshot(id) != nil {
			a.mutate(id, nil, func(x *Task) {
				x.Status = statusCleanupFailed
				x.Message = "文件被占用，自动清理失败，可再次点击重试"
			})
		}
	}()
}
