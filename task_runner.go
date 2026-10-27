package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"hlsink/internal/engine"
)

// 本文件只做「编排」：任务怎么排队、用哪个引擎、失败了要不要降级、成品怎么发布。
//
// 具体某个外部工具的参数怎么拼、输出怎么读，全部在 internal/engine 里。
// 除了 toolMap() 那几行「配置字段 → 引擎 key」的映射，本文件不出现任何具体工具。
// 加新工具时：新实现的文件在自己的 init() 里登记，toolMap 补一行，其余不用动。

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
	// stagedName 所有引擎在 staging 内统一使用的文件名前缀。
	// findStaged 靠它认产物，所以这里是全局约定，不属于某个引擎。
	stagedName = "download"
	// maxEngineAttempts 一条降级链最多尝试几个引擎。
	// 正常链条是 RE→yt-dlp 两级；设上限是为了兜住「A→B→A」这类配置错误，
	// 否则会无限重试同一个坏链接，既烧流量又让任务卡在 0%。
	maxEngineAttempts = 3
)

// runState 表示一次运行（下载 + 可选转码）的全部可取消资源。
type runState struct {
	runID   string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	logPath string
	procs   *engine.Procs

	// engKey 当前正在跑的引擎 key。进度解析要用它，因为不同引擎的输出格式不同，
	// 只能让「正在跑的那个」来解释自己的输出，不能拿所有引擎挨个试。
	engMu  sync.RWMutex
	engKey string
}

// setEngine 记录当前引擎（含降级切换）。
func (rs *runState) setEngine(key string) {
	if rs == nil {
		return
	}
	rs.engMu.Lock()
	rs.engKey = key
	rs.engMu.Unlock()
}

// currentEngine 返回当前引擎实现，未设置时返回 nil。
func (rs *runState) currentEngine() engine.Downloader {
	if rs == nil {
		return nil
	}
	rs.engMu.RLock()
	key := rs.engKey
	rs.engMu.RUnlock()
	if key == "" {
		return nil
	}
	d, _ := engine.Get(key)
	return d
}

// killAll 杀掉本次运行登记的所有进程树。
func (rs *runState) killAll() {
	if rs == nil || rs.procs == nil {
		return
	}
	rs.procs.KillAll()
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

// ---- 外部工具解析（配置字段 → 引擎）----

// toolMap 把设置里的路径字段映射成「引擎 key → 用户填的原始值」。
//
// 这是唯一知道「配置字段叫什么」的地方：配置是 main 的概念，引擎只认自己的 key。
// 加一个工具时，这里补一行、Settings 补一个字段即可，引擎实现不用动。
func (a *App) toolMap() map[string]string {
	s := a.currentSettings()
	return map[string]string{
		engine.KeyRE:     s.REPath,
		engine.KeyYtdlp:  s.YtdlpPath,
		engine.KeyFFmpeg: s.FFmpegPath,
	}
}

// tools 解析出本次运行可用的依赖（ffmpeg 等）。
//
// 只有文件确实存在才填：给引擎一个不存在的路径，比不填更糟
// （RE 会拿它去混流然后 exit 1，报错完全看不出是 ffmpeg 的问题）。
func (a *App) tools() engine.Tools {
	var t engine.Tools
	tr := engine.GetTranscoder()
	if tr == nil {
		return t
	}
	if ff := engine.Resolve(a.toolMap()[tr.Key()], tr.Binary()); ff != "" {
		if st, err := os.Stat(ff); err == nil && !st.IsDir() {
			t.FFmpeg = ff
			t.FFmpegDir = filepath.Dir(ff)
		}
	}
	return t
}

// CheckTools 把所有外部工具的解析结果回显给设置页。
//
// 遍历注册表而不是写死三个名字：以后加工具，设置页会自动多出一行状态，
// 后端零改动（前端补一个占位 DOM 即可显示）。
func (a *App) CheckTools() []engine.Status {
	paths := a.toolMap()
	out := make([]engine.Status, 0, 4)
	for _, d := range engine.List() {
		out = append(out, engine.Check(d.Key(), d.Label(), paths[d.Key()], d.Binary()))
	}
	if t := engine.GetTranscoder(); t != nil {
		out = append(out, engine.Check(t.Key(), t.Label(), paths[t.Key()], t.Binary()))
	}
	return out
}

// ---- 运行调度 ----

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
		procs:   engine.NewProcs(),
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
		if d := rs.currentEngine(); d != nil {
			a.applyProgress(taskID, rs.runID, d, text)
		}
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
		if rs.ctx.Err() != nil || errors.Is(downloadErr, engine.ErrCancelled) {
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
		if rs.ctx.Err() != nil || errors.Is(terr, engine.ErrCancelled) {
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
//
// 引擎选择完全由注册表决定：先问「谁认领这种源」，失败后按引擎自己声明的
// Fallback 往下走。本函数不认识具体工具。
func (a *App) doDownload(rs *runState, taskID string) (string, error) {
	snap := a.taskSnapshot(taskID)
	if snap == nil {
		return "", engine.ErrCancelled
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

	kind := engine.NormalizeKind(snap.Kind)
	if !kind.Known() {
		kind = engine.DetectKind(snap.URL)
	}

	job := engine.Job{
		URL:     snap.URL,
		Kind:    kind,
		OutDir:  downloadDir,
		TmpDir:  tmpDir,
		Threads: normalizeThreadCount(snap.threadCount),
		Headers: snap.Headers,
		Referer: snap.Referer,
		Proxy:   systemProxy(),
		Name:    stagedName,
	}
	// 依赖在循环内按次解析（见下），这里只声明。
	var tools engine.Tools

	current := engine.Pick(kind)
	if current == nil {
		return "", fmt.Errorf("没有可用的引擎处理 %s 链接", kind.Label())
	}

	first := true
	for attempt := 1; ; attempt++ {
		// 每次尝试前重新解析依赖：原实现里每个引擎启动前都会重读设置与系统代理，
		// 用户在降级间隙改了代理或工具路径，下一次尝试应当用新值。
		job.Proxy = systemProxy()
		tools = a.tools()

		hint := ""
		if first {
			hint = current.Hint()
			first = false
		}
		a.mutateRun(taskID, rs.runID, func(t *Task) {
			t.Engine = current.Key()
			if hint != "" {
				t.Message = hint
			}
		})

		runErr := a.runEngine(rs, current, job, tools)

		// 先看有没有产物，再看是否被取消：引擎跑完的瞬间用户点了暂停时，
		// 产物是完整的，交给上层按自己的节奏处理即可。
		if runErr == nil {
			if p, ferr := findStaged(downloadDir); ferr == nil {
				return p, nil
			} else {
				// 引擎说成功了但没产物，也当失败处理（可能是站点返回了空清单）。
				runErr = ferr
			}
		}
		if rs.ctx.Err() != nil {
			return "", engine.ErrCancelled
		}

		// 降级链深度保护：兜住「A→B→A」这种配置错误，绝不允许无限重试。
		if attempt >= maxEngineAttempts {
			return "", withLogTail(a.explainEngineError(current, runErr), rs.logPath)
		}
		next, ok := a.fallbackFor(kind, current, runErr, rs.logPath)
		if !ok {
			return "", withLogTail(a.explainEngineError(current, runErr), rs.logPath)
		}

		msg := a.fallbackMessage(current, next, runErr, rs.logPath)
		a.mutateRun(taskID, rs.runID, func(t *Task) {
			t.Engine = next.Key()
			t.Message = msg
		})
		// 先清掉上一轮的半成品，避免与下一个引擎的输出名冲突（只在 staging 内清理）。
		clearStaged(downloadDir)
		current = next
	}
}

// runEngine 用指定引擎执行一次下载。
func (a *App) runEngine(rs *runState, d engine.Downloader, job engine.Job, tools engine.Tools) error {
	exe, err := engine.Executable(a.toolMap()[d.Key()], d.Binary(), d.Label())
	if err != nil {
		return err
	}
	cmd, err := d.Build(job, tools)
	if err != nil {
		return err
	}
	cmd.Exe = exe
	for _, note := range cmd.Notes {
		engine.AppendLog(rs.logPath, note)
	}

	// 先登记再启动：tail 可能在同一瞬间收到第一行输出，必须能查到是谁写的。
	rs.setEngine(d.Key())
	return engine.Run(rs.ctx, engine.RunOpts{
		Command: cmd,
		LogPath: rs.logPath,
		Idle:    d.Stall(),
		Probes:  d.Probes(job),
		Procs:   rs.procs,
	})
}

// fallbackFor 判断当前引擎失败后是否值得换引擎再试，值得则返回下一个引擎。
//
// 判断条件（任一成立就换）：
//   - 看门狗认定它连续无进展（有下一级就早点换，别等它把每片都试到超时）；
//   - 日志里出现明确的 Cloudflare 特征；
//   - DASH 站点失败（分片格式兼容性问题居多）。
//
// 最后一条是历史经验：DASH 的 fMP4 各家实现差异大，RE 报错时换 yt-dlp
// 往往能过，所以对 DASH 放宽到「只要失败就再试一次」。
func (a *App) fallbackFor(kind engine.Kind, from engine.Downloader, err error, logPath string) (engine.Downloader, bool) {
	key := from.Fallback()
	if key == "" || err == nil {
		return nil, false
	}
	stalled := errors.Is(err, engine.ErrStalled)
	blocked := logShowsCloudflare(logPath)
	if !stalled && !blocked && kind != engine.KindDASH {
		return nil, false
	}
	next, ok := engine.Get(key)
	if !ok {
		return nil, false
	}
	return next, true
}

// fallbackMessage 生成切换引擎时卡片上的一句话。
func (a *App) fallbackMessage(from, to engine.Downloader, err error, logPath string) string {
	switch {
	case errors.Is(err, engine.ErrStalled):
		return fmt.Sprintf("%s 超过 %d 秒没有下载进展，切换 %s 引擎",
			from.Label(), int(from.Stall().Seconds()), to.Label())
	case logShowsCloudflare(logPath):
		return fmt.Sprintf("被 Cloudflare 拦截，切换 %s 引擎", to.Label())
	default:
		return fmt.Sprintf("%s 下载失败，切换 %s 引擎", from.Label(), to.Label())
	}
}

// explainEngineError 把引擎内部错误翻译成用户能理解的一句话。
func (a *App) explainEngineError(d engine.Downloader, err error) error {
	if errors.Is(err, engine.ErrStalled) {
		return fmt.Errorf("%s 超过 %d 秒没有下载进展", d.Label(), int(d.Stall().Seconds()))
	}
	return err
}

// doTranscode 用注册的后处理工具转码，返回成品路径。
func (a *App) doTranscode(rs *runState, t *Task, src string) (string, error) {
	tr := engine.GetTranscoder()
	if tr == nil {
		return "", fmt.Errorf("没有可用的转码工具")
	}
	exe, err := engine.Executable(a.toolMap()[tr.Key()], tr.Binary(), tr.Label())
	if err != nil {
		return "", err
	}
	outDir := filepath.Join(t.WorkDir, "transcode")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("创建转码目录失败: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	out := filepath.Join(outDir, base+"_HEVC.mp4")

	cmd, err := tr.Build(src, out)
	if err != nil {
		return "", err
	}
	cmd.Exe = exe

	logPath := filepath.Join(t.WorkDir, "logs", "run-"+rs.runID+"-"+tr.Key()+".log")
	// 不设 Idle：转码没有「字节落盘增长」这种可观测的中间进度，
	// 硬加看门狗会把正常的慢速转码误杀。
	if err := engine.Run(rs.ctx, engine.RunOpts{
		Command: cmd,
		LogPath: logPath,
		Procs:   rs.procs,
	}); err != nil {
		return "", err
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		return "", fmt.Errorf("转码未生成有效文件")
	}
	return out, nil
}

// ---- 日志进度解析 ----

// applyProgress 解析一段进度文本并写回任务。
//
// RE 用 \r 原地刷写，一段文本里可能挤着十几条进度，最后一条才是最新的，
// 所以倒序找「第一条能解析的行」，命中即停——避免同一批数据触发多次状态更新。
func (a *App) applyProgress(taskID, runID string, d engine.Downloader, text string) {
	lines := strings.FieldsFunc(text, func(r rune) bool { return r == '\r' || r == '\n' })
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		p, ok := d.ParseLine(line)
		if !ok {
			continue
		}
		a.mutateRun(taskID, runID, func(t *Task) {
			t.Progress = p.Percent
			t.Speed = p.Speed
			if p.Segments != "" {
				t.Segments = p.Segments
			}
		})
		return
	}
}

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
