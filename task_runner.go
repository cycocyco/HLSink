package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---- 外部工具路径解析（不再写死） ----

// toolSubdirs 是目录里找不到可执行文件时的下探顺序。
//
// ffmpeg 官方 Windows 包是 <root>\bin\ffmpeg.exe 这种结构，用户填的大多是
// 解压根目录，所以必须往下探一层，否则会拼出一个不存在的路径。
var toolSubdirs = []string{"bin", "tools", "deps"}

// findToolInDir 在 dir 本身及其常见子目录里查找 binary，找不到返回空串。
func findToolInDir(dir, binary string) string {
	if st, err := os.Stat(filepath.Join(dir, binary)); err == nil && !st.IsDir() {
		return filepath.Join(dir, binary)
	}
	for _, sub := range toolSubdirs {
		cand := filepath.Join(dir, sub, binary)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// resolveToolPath 把用户设置解析成可执行文件的绝对路径。
//
// raw 支持：完整 exe 路径 / 所在目录 / PATH 中的命令名；留空则自动在
// exe 旁及其 tools|deps|bin 子目录、系统 PATH 中查找。找不到返回空字符串。
func resolveToolPath(raw, binary string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if st, err := os.Stat(raw); err == nil {
			if st.IsDir() {
				if p := findToolInDir(raw, binary); p != "" {
					return p
				}
				// 目录里确实没有：仍是拼一个具体路径返回，
				// 让调用方的报错能指出"在哪个位置找的哪个文件"，便于排查。
				return filepath.Join(raw, binary)
			}
			return raw
		}
		if p, err := exec.LookPath(raw); err == nil {
			return p
		}
		return filepath.Join(raw, binary)
	}
	if exe, err := os.Executable(); err == nil {
		if p := findToolInDir(filepath.Dir(exe), binary); p != "" {
			return p
		}
	}
	if p, err := exec.LookPath(binary); err == nil {
		return p
	}
	return ""
}

// toolRE 返回 N_m3u8DL-RE 可执行文件路径。
func (a *App) toolRE() string {
	return resolveToolPath(a.currentSettings().REPath, "N_m3u8DL-RE.exe")
}

// toolYtdlp 返回 yt-dlp 可执行文件路径。
func (a *App) toolYtdlp() string {
	return resolveToolPath(a.currentSettings().YtdlpPath, "yt-dlp.exe")
}

// toolFFmpeg 返回 ffmpeg 可执行文件路径。
func (a *App) toolFFmpeg() string {
	return resolveToolPath(a.currentSettings().FFmpegPath, "ffmpeg.exe")
}

// toolFFmpegDir 返回 ffmpeg 所在目录（供 yt-dlp --ffmpeg-location 使用）。
// 文件不存在时返回空：宁可让 yt-dlp 自己去 PATH 找，也不要喂它一个假目录。
func (a *App) toolFFmpegDir() string {
	ff := a.toolFFmpeg()
	if ff == "" {
		return ""
	}
	if st, err := os.Stat(ff); err != nil || st.IsDir() {
		return ""
	}
	return filepath.Dir(ff)
}

// ToolStatus 是单个外部工具的解析结果，供设置页回显「到底认没认出来」。
type ToolStatus struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Note  string `json:"note"`
}

// CheckTools 把三个外部工具的解析结果回显给设置页。
//
// 用户填目录、填 exe、留空自动找，最终都经过 resolveToolPath，
// 这里直接把命中结果报出来——填完不用猜，也不用等到下载失败才发现。
func (a *App) CheckTools() []ToolStatus {
	s := a.currentSettings()
	one := func(key, name, raw, binary string) ToolStatus {
		p := resolveToolPath(raw, binary)
		out := ToolStatus{Key: key, Name: name, Path: p}
		if p == "" {
			out.Note = "未找到，请指定目录或 exe"
			return out
		}
		if st, err := os.Stat(p); err != nil || st.IsDir() {
			out.Note = "该位置没有 " + binary
			return out
		}
		out.Found = true
		out.Note = "已就绪"
		return out
	}
	return []ToolStatus{
		one("re", "N_m3u8DL-RE", s.REPath, "N_m3u8DL-RE.exe"),
		one("ytdlp", "yt-dlp", s.YtdlpPath, "yt-dlp.exe"),
		one("ffmpeg", "ffmpeg", s.FFmpegPath, "ffmpeg.exe"),
	}
}

// 真正有界的并发：最多 2 个下载 + 1 个转码同时跑，其余任务保持 queued。
const (
	downloadConcurrency  = 2
	transcodeConcurrency = 1
)

const (
	// runStopTimeout 暂停/删除时等待 run 收尾的上限。
	// 这是同步绑定调用，前端会一直 await，所以不能太长；taskkill /F 通常 1s 内结束。
	runStopTimeout = 8 * time.Second
	// tailInterval 日志进度轮询间隔。
	tailInterval = 300 * time.Millisecond
	// stagedName RE / yt-dlp 在 staging 内统一使用的文件名前缀。
	stagedName = "download"
)

// 引擎标识（下发给前端）。
const (
	engineRE    = "re"
	engineYtdlp = "yt-dlp"
)

// errCancelled 表示 run 因暂停/删除/退出被取消，不能当作下载失败处理。
var errCancelled = errors.New("任务已取消")

var (
	reFrag  = regexp.MustCompile(`(\d+)/(\d+)\s+\d`)
	reFrag2 = regexp.MustCompile(`frag (\d+)/(\d+)`)
)

// runState 表示一次运行（下载 + 可选转码）的全部可取消资源。
type runState struct {
	runID   string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	logPath string

	mu    sync.Mutex
	procs map[int]*os.Process
}

// track 登记一个外部进程，供暂停/删除/退出时杀进程树。
func (rs *runState) track(cmd *exec.Cmd) {
	if rs == nil || cmd == nil || cmd.Process == nil {
		return
	}
	rs.mu.Lock()
	rs.procs[cmd.Process.Pid] = cmd.Process
	rs.mu.Unlock()
}

// untrack 进程正常退出后取消登记。
func (rs *runState) untrack(cmd *exec.Cmd) {
	if rs == nil || cmd == nil || cmd.Process == nil {
		return
	}
	rs.mu.Lock()
	delete(rs.procs, cmd.Process.Pid)
	rs.mu.Unlock()
}

// killAll 杀掉所有登记进程的进程树（RE / yt-dlp / ffmpeg）。
func (rs *runState) killAll() {
	if rs == nil {
		return
	}
	rs.mu.Lock()
	procs := make([]*os.Process, 0, len(rs.procs))
	for _, p := range rs.procs {
		procs = append(procs, p)
	}
	rs.mu.Unlock()
	for _, p := range procs {
		killTree(p)
	}
}

// waitDone 等待 run 结束，超时返回。
func (rs *runState) waitDone(timeout time.Duration) {
	if rs == nil {
		return
	}
	if timeout <= 0 {
		select {
		case <-rs.done:
		default:
		}
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-rs.done:
	case <-timer.C:
	}
}

// hideWindow 压掉外部 exe 的控制台黑框（仅 Windows）。
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// killTree 杀掉进程及其子进程（Windows: taskkill /T /F）。
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(p.Pid), "/T", "/F")
	hideWindow(cmd)
	// 只发起、不等待：taskkill 是独立进程，父进程退出后它照样会跑完。
	// 若这里 cmd.Run() 等结果，多个任务叠加就会把点 X 拖成秒级。
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
	_ = p.Kill()
}

// acquireSlot 获取并发槽位；context 取消时立刻返回 false（排队中的任务可被暂停/删除）。
func acquireSlot(ctx context.Context, sem chan struct{}) bool {
	if ctx.Err() != nil {
		return false
	}
	select {
	case sem <- struct{}{}:
		// select 在“槽位可写”和“ctx 已取消”同时就绪时可能随机选中前者；
		// 拿到槽位后再确认一次，已取消则立即归还，避免测试/调度语义抖动。
		if ctx.Err() != nil {
			releaseSlot(sem)
			return false
		}
		return true
	case <-ctx.Done():
		return false
	}
}

// releaseSlot 归还并发槽位。
func releaseSlot(sem chan struct{}) {
	select {
	case <-sem:
	default:
	}
}

// launchRun 为任务启动一次新的 run：新的 RunID + 独立日志文件。
//
// 注意：工作目录只在 run 内部创建，这样「删除中」的任务不可能被迟到的 run
// 重新创建出目录（否则删除后会残留空目录）。
func (a *App) launchRun(taskID string) error {
	snap := a.taskSnapshot(taskID)
	if snap == nil {
		return fmt.Errorf("任务不存在: %s", taskID)
	}
	workDir := snap.WorkDir
	if workDir == "" {
		workDir = taskWorkDir(taskID)
		a.mutate(taskID, nil, func(t *Task) { t.WorkDir = workDir })
	}

	runID := newRunID()
	logPath := filepath.Join(workDir, "logs", "run-"+runID+".log")
	ctx, cancel := context.WithCancel(a.rootCtx)
	rs := &runState{
		runID:   runID,
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		logPath: logPath,
		procs:   map[int]*os.Process{},
	}

	a.mu.Lock()
	if a.runs[taskID] != nil {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("任务已有运行实例，不能重复启动")
	}
	t, ok := a.tasks[taskID]
	if !ok {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("任务不存在: %s", taskID)
	}
	if !activeStatus(t.Status) {
		a.mu.Unlock()
		cancel()
		return fmt.Errorf("任务当前状态（%s）不允许启动", t.Status)
	}
	t.RunID = runID
	a.runs[taskID] = rs
	a.mu.Unlock()

	go func() {
		// defer 顺序（LIFO）：runLoop 返回 → clearRun → cancel → close(done)
		defer close(rs.done)
		defer cancel()
		defer a.clearRun(taskID, rs)
		a.runLoop(rs, taskID)
	}()
	return nil
}

// runLoop 串起「下载 → 可选 HEVC 转码 → 发布成品」的完整流程。
func (a *App) runLoop(rs *runState, taskID string) {
	task := a.taskSnapshot(taskID)
	if task == nil {
		return
	}
	workDir := task.WorkDir
	if workDir == "" {
		workDir = taskWorkDir(taskID)
	}
	// 工作目录在 run 内创建（子目录：download/tmp/transcode/logs）
	if err := ensureTaskDirs(workDir); err != nil {
		msg := err.Error()
		a.finishTask(taskID, rs.runID, statusError, msg, msg)
		return
	}

	// 进度 tail：整轮 run 共用一个日志文件，run 取消即退出。
	tailCtx, stopTail := context.WithCancel(rs.ctx)
	defer stopTail()
	go a.tailRunLog(tailCtx, taskID, rs.runID, rs.logPath, func(text string) {
		a.applyREProgress(taskID, rs.runID, text)
		a.applyYtdlpProgress(taskID, rs.runID, text)
	})

	// ---- 下载阶段（并发上限 2；没拿到槽位的任务保持 queued）----
	if !acquireSlot(rs.ctx, a.downloadSem) {
		return
	}
	started := a.setStatusForRun(taskID, rs.runID, statusRunning, func(t *Task) {
		t.Speed = ""
		t.Segments = ""
		t.Message = ""
	})
	if !started {
		releaseSlot(a.downloadSem)
		return
	}

	product, downloadErr := a.doDownload(rs, taskID)
	releaseSlot(a.downloadSem)

	if downloadErr != nil {
		if rs.ctx.Err() != nil || errors.Is(downloadErr, errCancelled) {
			return // 暂停/删除/退出导致的中断，交给控制方写状态
		}
		msg := downloadErr.Error()
		a.finishTask(taskID, rs.runID, statusError, msg, msg)
		return
	}
	if product == "" {
		msg := "下载完成但未找到产物文件"
		a.finishTask(taskID, rs.runID, statusError, msg, msg)
		return
	}
	a.mutateRun(taskID, rs.runID, func(t *Task) { t.DownloadPath = product })

	// ---- 转码阶段（并发上限 1，转码期间不支持暂停）----
	warn := ""
	finalPath := product
	transcoded := false
	if task.transcode {
		if !acquireSlot(rs.ctx, a.transcodeSem) {
			return
		}
		if !a.setStatusForRun(taskID, rs.runID, statusTranscoding, func(t *Task) { t.Speed = "" }) {
			releaseSlot(a.transcodeSem)
			return
		}
		out, terr := a.doTranscode(rs, task, product)
		releaseSlot(a.transcodeSem)
		if rs.ctx.Err() != nil || errors.Is(terr, errCancelled) {
			return
		}
		if terr == nil {
			finalPath = out
			transcoded = true
		} else {
			// 转码失败保留原画，仍然算完成（带警告）
			warn = "转码失败，已保留原画: " + terr.Error()
			a.mutateRun(taskID, rs.runID, func(t *Task) { t.Message = warn })
		}
	}

	if rs.ctx.Err() != nil {
		return // 删除/退出来得及，不再发布
	}

	// ---- 发布成品到任务的保存目录快照 ----
	latest := a.taskSnapshot(taskID)
	if latest == nil {
		return
	}
	base := safeBaseName(latest.Filename, latest.Title, taskID)
	dest, perr := publishFile(finalPath, latest.SaveDir, base)
	if perr != nil {
		msg := perr.Error()
		a.finishTask(taskID, rs.runID, statusError, msg, msg)
		return
	}

	// 立刻记录成品路径：即使随后被暂停/删除接管，删除流程也能找到这个文件。
	a.mutate(taskID, func(t *Task) bool { return t.RunID == rs.runID }, func(t *Task) {
		t.OutputPath = dest
	})

	msg := dest
	if warn != "" {
		msg = warn + " · 已保存到 " + dest
	}
	if !a.finishDone(taskID, rs.runID, dest, product, msg) {
		// 状态已被暂停/删除接管：撤销这次发布，绝不留下未同步的产物。
		_ = os.Remove(dest)
		return
	}

	// 发布成功后才删 staging 原片：只把 HEVC 成品留在用户目录。
	if transcoded && !samePath(product, dest) {
		_ = os.Remove(product)
	}

	// 成功收尾清 staging，避免每部片子留几 GB 中间文件（日志保留）。
	cleanupStaging(workDir)
}

// doDownload 下载到任务 staging，返回精确的 staged 产物路径。
// RE 优先；仅当本次日志出现明确 Cloudflare 特征时降级 yt-dlp。
func (a *App) doDownload(rs *runState, taskID string) (string, error) {
	snap := a.taskSnapshot(taskID)
	if snap == nil {
		return "", errCancelled
	}
	workDir := snap.WorkDir
	if workDir == "" {
		workDir = taskWorkDir(taskID)
	}
	downloadDir := filepath.Join(workDir, "download")
	tmpDir := filepath.Join(workDir, "tmp")
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return "", fmt.Errorf("创建任务下载目录失败: %w", err)
	}
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("创建任务分片目录失败: %w", err)
	}

	kind := normalizeStreamKind(snap.Kind)
	if !isKnownStreamKind(kind) {
		kind = detectStreamKind(snap.URL)
	}

	// 网页地址（或非已知流地址）：RE 只吃清单文件，交给 yt-dlp 自行解析。
	if kind == kindPage {
		a.mutateRun(taskID, rs.runID, func(t *Task) {
			t.Engine = engineYtdlp
			t.Message = "正在解析网页地址"
		})
		if err := a.runYtdlp(rs, snap, downloadDir); err != nil {
			if rs.ctx.Err() != nil {
				return "", errCancelled
			}
			return "", withLogTail(err, rs.logPath)
		}
		if rs.ctx.Err() != nil {
			return "", errCancelled
		}
		p, err := findStaged(downloadDir)
		if err != nil {
			return "", withLogTail(err, rs.logPath)
		}
		return p, nil
	}

	a.mutateRun(taskID, rs.runID, func(t *Task) { t.Engine = engineRE })
	reErr := a.runRE(rs, snap, downloadDir, tmpDir)
	if reErr == nil {
		if p, ferr := findStaged(downloadDir); ferr == nil {
			return p, nil
		} else {
			reErr = ferr
		}
	}
	if rs.ctx.Err() != nil {
		return "", errCancelled
	}

	// 降级判据：明确的 Cloudflare 特征；DASH 站点 RE 失败时也给 yt-dlp 一次机会。
	blocked := logShowsCloudflare(rs.logPath)
	if !blocked && kind != kindDASH {
		return "", withLogTail(reErr, rs.logPath)
	}

	msg := "被 Cloudflare 拦截，切换 yt-dlp 引擎"
	if !blocked {
		msg = "RE 下载失败，切换 yt-dlp 引擎"
	}
	a.mutateRun(taskID, rs.runID, func(t *Task) {
		t.Engine = engineYtdlp
		t.Message = msg
	})
	// 先清掉上一轮的半成品，避免与 yt-dlp 的输出名冲突（只在任务 staging 内清理）。
	clearStaged(downloadDir)
	if ytErr := a.runYtdlp(rs, snap, downloadDir); ytErr != nil {
		if rs.ctx.Err() != nil {
			return "", errCancelled
		}
		return "", withLogTail(ytErr, rs.logPath)
	}
	if rs.ctx.Err() != nil {
		return "", errCancelled
	}
	p, err := findStaged(downloadDir)
	if err != nil {
		return "", withLogTail(err, rs.logPath)
	}
	return p, nil
}

// runRE 用 N_m3u8DL-RE 下载到任务 staging（支持 HLS / DASH）。
func (a *App) runRE(rs *runState, t *Task, downloadDir, tmpDir string) error {
	reExe := a.toolRE()
	if reExe == "" {
		return fmt.Errorf("未找到 N_m3u8DL-RE，请在设置里指定它的路径")
	}
	if st, err := os.Stat(reExe); err != nil || st.IsDir() {
		return fmt.Errorf("N_m3u8DL-RE 不在指定位置: %s", reExe)
	}
	threads := strconv.Itoa(normalizeThreadCount(t.threadCount))
	kind := normalizeStreamKind(t.Kind)
	if !isKnownStreamKind(kind) {
		kind = detectStreamKind(t.URL)
	}
	// DASH 的 fMP4 分片交给 ffmpeg 正常混流；HLS 沿用二进制合并（TS 直拼更稳更快）。
	muxArgs := []string{"-M", "format=mp4:mux importer=raw"}
	if kind == kindDASH {
		muxArgs = []string{"-M", "format=mp4"}
	}
	args := []string{
		t.URL,
		"--save-dir", downloadDir,
		"--tmp-dir", tmpDir,
		"--save-name", stagedName,
		"--thread-count", threads, // 分片并发；默认 32，替代 RE 自带的 12
		"-mt", // 音视频轨道并行下载，避免先视频后音频
		"--disable-update-check",
		"--auto-select", // 无交互环境必须自动选轨
		"--no-log",
	}
	// 只有 ffmpeg 确实存在才传：传一个不存在的路径，RE 会在混流阶段
	// 直接退出（exit status 1），报错信息还完全看不出是 ffmpeg 的问题。
	if ff := a.toolFFmpeg(); ff != "" {
		if st, err := os.Stat(ff); err == nil && !st.IsDir() {
			args = append(args, "--ffmpeg-binary-path", ff)
		}
	}
	args = append(args, muxArgs...)
	headerArgs := headerLines(t.Headers)
	// Referer 是单独字段；若合并后的 Headers 没带它，仍需显式补上防盗链头。
	if ref := cleanHeaderValue(t.Referer); ref != "" {
		hasReferer := false
		for k := range t.Headers {
			if strings.EqualFold(k, "Referer") {
				hasReferer = true
				break
			}
		}
		if !hasReferer {
			headerArgs = append(headerArgs, "Referer: "+ref)
		}
	}
	for _, h := range headerArgs {
		args = append(args, "-H", h)
	}
	return a.runExternal(rs, reExe, args, rs.logPath)
}

// runYtdlp 用 yt-dlp --impersonate chrome 下载到任务 staging。
// 处理两类：过 CF 盾的降级下载、网页地址（由 yt-dlp 自行解析站点视频）。
func (a *App) runYtdlp(rs *runState, t *Task, downloadDir string) error {
	yt := a.toolYtdlp()
	if yt == "" {
		return fmt.Errorf("未找到 yt-dlp，请在设置里指定它的路径")
	}
	if st, err := os.Stat(yt); err != nil || st.IsDir() {
		return fmt.Errorf("yt-dlp 不在指定位置: %s", yt)
	}
	threads := strconv.Itoa(normalizeThreadCount(t.threadCount))
	args := []string{
		t.URL,
		"--impersonate", "chrome", // curl_cffi Chrome TLS 指纹
		"--merge-output-format", "mp4",
		"-P", downloadDir,
		"-o", stagedName + ".%(ext)s",
		"--newline", // 进度逐行输出，便于解析
		"--no-playlist",
		"-N", threads, // 分片并发与 RE 使用同一设置
	}
	// 显式指定自带 ffmpeg，避免依赖系统 PATH 或别的软件装的那份。
	if dir := a.toolFFmpegDir(); dir != "" {
		args = append(args, "--ffmpeg-location", dir)
	}
	// Referer 为空就不伪造，交给站点默认行为。
	if ref := cleanHeaderValue(t.Referer); ref != "" {
		args = append(args, "--referer", ref)
	}
	ua, extra := splitHeadersForYtdlp(t.Headers)
	if ua != "" {
		args = append(args, "--user-agent", ua)
	}
	for _, h := range extra {
		args = append(args, "--add-header", h)
	}
	return a.runExternal(rs, yt, args, rs.logPath)
}

// doTranscode 用 ffmpeg 转 HEVC（NVENC），返回成品路径。
func (a *App) doTranscode(rs *runState, t *Task, src string) (string, error) {
	ff := a.toolFFmpeg()
	if ff == "" {
		return "", fmt.Errorf("未找到 ffmpeg，请在设置里指定它的路径")
	}
	if st, err := os.Stat(ff); err != nil || st.IsDir() {
		return "", fmt.Errorf("ffmpeg 不在指定位置: %s", ff)
	}
	outDir := filepath.Join(t.WorkDir, "transcode")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("创建转码目录失败: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	out := filepath.Join(outDir, base+"_HEVC.mp4")

	args := []string{
		"-y", "-hide_banner", "-nostdin",
		"-i", src,
		"-c:v", "hevc_nvenc", "-preset", "p5", "-cq", "23",
		"-c:a", "copy",
		"-movflags", "+faststart",
		out,
	}
	logPath := filepath.Join(t.WorkDir, "logs", "run-"+rs.runID+"-ffmpeg.log")
	if err := a.runExternal(rs, ff, args, logPath); err != nil {
		return "", err
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		return "", fmt.Errorf("转码未生成有效文件")
	}
	return out, nil
}

// runExternal 启动外部进程，输出追加写入日志；进程登记到 runState 以便被杀。
func (a *App) runExternal(rs *runState, exe string, args []string, logPath string) error {
	if rs.ctx.Err() != nil {
		return errCancelled
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(exe, args...)
	hideWindow(cmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", filepath.Base(exe), err)
	}
	rs.track(cmd)
	waitErr := cmd.Wait()
	rs.untrack(cmd)

	if waitErr != nil {
		if rs.ctx.Err() != nil {
			return errCancelled
		}
		return fmt.Errorf("%s 退出异常: %w", filepath.Base(exe), waitErr)
	}
	return nil
}

// ---- 请求头处理 ----

// headerLines 生成 RE 使用的 "-H Key: Value" 行（按 key 排序保证可复现）。
func headerLines(headers map[string]string) []string {
	if len(headers) == 0 {
		return nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		v := cleanHeaderValue(headers[k])
		if v == "" {
			continue
		}
		out = append(out, k+": "+v)
	}
	return out
}

// splitHeadersForYtdlp 拆出 User-Agent（单独传）与需要通过 --add-header 传的头。
// Referer 由 --referer 传，避免与站点默认行为冲突。
func splitHeadersForYtdlp(headers map[string]string) (ua string, extra []string) {
	if len(headers) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := cleanHeaderValue(headers[k])
		if v == "" {
			continue
		}
		switch {
		case strings.EqualFold(k, "User-Agent"):
			ua = v
		case strings.EqualFold(k, "Referer"):
			// 交给 --referer
		default:
			extra = append(extra, k+": "+v)
		}
	}
	return ua, extra
}

// cleanHeaderValue 过滤换行/控制字符，防止 header injection。
func cleanHeaderValue(v string) string {
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r == '\r' || r == '\n' || r == '\t':
			b.WriteRune(' ')
		case r < 0x20 || r == 0x7f:
			continue
		default:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// ---- 日志进度解析 ----

// tailRunLog 轮询日志增量并交给 parse 解析。
//
// 必须处理 size < lastSize（文件被截断/重建），禁止用负长度 make，
// 并保留未写完的半行到下次再解析。
func (a *App) tailRunLog(ctx context.Context, taskID, runID, path string, parse func(text string)) {
	var offset int64
	var pending string

	ticker := time.NewTicker(tailInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		size := st.Size()
		if size < offset {
			// 文件被截断或重建，从头再来
			offset = 0
			pending = ""
		}
		if size == offset {
			continue
		}

		const maxChunk = 1 << 20
		if size-offset > maxChunk {
			offset = size - maxChunk
			pending = ""
		}
		delta := size - offset
		if delta <= 0 {
			continue
		}

		buf := make([]byte, delta)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		n, _ := f.ReadAt(buf, offset)
		_ = f.Close()
		if n <= 0 {
			continue
		}
		offset += int64(n)

		text := pending + string(buf[:n])
		cut := strings.LastIndexAny(text, "\r\n")
		if cut < 0 {
			// 还没有完整行，留作缓冲（防超长垃圾行吃内存）
			if len(text) > 64<<10 {
				text = ""
			}
			pending = text
			continue
		}
		pending = text[cut+1:]
		if len(pending) > 64<<10 {
			pending = ""
		}
		text = text[:cut]
		if text != "" {
			parse(text)
		}
	}
}

// applyREProgress 解析 RE 的进度行（\r 原地刷写，取最后一段有效行）。
func (a *App) applyREProgress(taskID, runID, text string) {
	parts := strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' })
	for i := len(parts) - 1; i >= 0; i-- {
		line := strings.TrimSpace(parts[i])
		if line == "" {
			continue
		}
		p, speed, seg, ok := parseRELine(line)
		if !ok {
			continue
		}
		a.mutateRun(taskID, runID, func(t *Task) {
			t.Progress = p
			t.Speed = speed
			if seg != "" {
				t.Segments = seg
			}
		})
		return
	}
}

// parseRELine 从 RE 进度行里抓百分比、速度、分片数。
func parseRELine(line string) (float64, string, string, bool) {
	idx := strings.Index(line, "%")
	if idx < 0 || idx > 12 {
		return 0, "", "", false
	}
	var p float64
	if _, err := fmt.Sscanf(strings.TrimSpace(line[:idx]), "%f", &p); err != nil {
		return 0, "", "", false
	}
	if p < 0 || p > 100 {
		return 0, "", "", false
	}
	speed := ""
	if i := strings.Index(line, "Bps"); i > 0 {
		if start := strings.LastIndex(line[:i], " "); start >= 0 {
			speed = strings.TrimSpace(line[start : i+3])
		}
	}
	seg := ""
	if m := reFrag.FindStringSubmatch(line); m != nil {
		seg = m[1] + "/" + m[2]
	}
	return p, speed, seg, true
}

// applyYtdlpProgress 解析 yt-dlp --newline 的进度行：
// "[download]  42.3% of 123.45MiB at 2.5MiB/s ETA 00:42"
func (a *App) applyYtdlpProgress(taskID, runID, text string) {
	const prefix = "[download]"
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		idx := strings.Index(line, "%")
		if idx <= len(prefix) {
			continue
		}
		var p float64
		if _, err := fmt.Sscanf(strings.TrimSpace(line[len(prefix):idx]), "%f", &p); err != nil {
			continue
		}
		if p < 0 || p > 100 {
			continue
		}
		speed := ""
		if i := strings.Index(line, " at "); i > 0 {
			speed = strings.TrimSpace(line[i+4:])
			if j := strings.Index(speed, " ETA"); j > 0 {
				speed = strings.TrimSpace(speed[:j])
			}
		}
		seg := ""
		if m := reFrag2.FindStringSubmatch(line); m != nil {
			seg = m[1] + "/" + m[2]
		}
		a.mutateRun(taskID, runID, func(t *Task) {
			t.Progress = p
			t.Speed = speed
			if seg != "" {
				t.Segments = seg
			}
		})
	}
}

// ---- 日志诊断 ----

// readLogTail 读日志末尾 maxBytes 字节（文件不存在/为空返回 nil）。
func readLogTail(path string, maxBytes int64) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil || st.Size() == 0 {
		return nil
	}
	size := st.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	n, _ := f.ReadAt(buf, start)
	if n <= 0 {
		return nil
	}
	return buf[:n]
}

// logShowsCloudflare 只在本次日志内容里出现明确 CF 特征时返回 true。
func logShowsCloudflare(logPath string) bool {
	buf := readLogTail(logPath, 16<<10)
	if len(buf) == 0 {
		return false
	}
	text := strings.ToLower(string(buf))
	for _, marker := range []string{"403", "forbidden", "cloudflare", "just a moment", "attention required", "cf-ray"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// logTailSummary 提取日志末尾摘要，过滤掉可能含 Cookie/Authorization 的行，
// 避免把凭据原样吐到 UI。
func logTailSummary(logPath string) string {
	buf := readLogTail(logPath, 8<<10)
	if len(buf) == 0 {
		return ""
	}
	normalized := strings.ReplaceAll(string(buf), "\r", "\n")
	var kept []string
	for _, line := range strings.Split(normalized, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.Contains(lower, "cookie") || strings.Contains(lower, "authorization") {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	if len(kept) > 3 {
		kept = kept[len(kept)-3:]
	}
	out := strings.Join(kept, " | ")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

// withLogTail 给错误追加日志末尾摘要（便于定位，但不泄露 cookie）。
func withLogTail(err error, logPath string) error {
	if err == nil {
		err = fmt.Errorf("下载失败")
	}
	tail := logTailSummary(logPath)
	if tail == "" {
		return err
	}
	return fmt.Errorf("%w（日志末尾: %s）", err, tail)
}
