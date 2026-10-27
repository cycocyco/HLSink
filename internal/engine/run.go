package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// 停滞探测的采样间隔。
const stallProbeInterval = 800 * time.Millisecond

// Procs 登记一次运行启动的所有外部进程，供暂停/删除/退出时统一杀进程树。
//
// 每次 run 一个实例，由调用方持有；本类型不感知任务与状态。
type Procs struct {
	mu    sync.Mutex
	items map[int]*os.Process
}

// NewProcs 创建一个空的进程登记表。
func NewProcs() *Procs {
	return &Procs{items: map[int]*os.Process{}}
}

// Track 登记一个刚启动的进程。
func (p *Procs) Track(cmd *exec.Cmd) {
	if p == nil || cmd == nil || cmd.Process == nil {
		return
	}
	p.mu.Lock()
	p.items[cmd.Process.Pid] = cmd.Process
	p.mu.Unlock()
}

// Untrack 进程正常退出后取消登记。
func (p *Procs) Untrack(cmd *exec.Cmd) {
	if p == nil || cmd == nil || cmd.Process == nil {
		return
	}
	p.mu.Lock()
	delete(p.items, cmd.Process.Pid)
	p.mu.Unlock()
}

// KillAll 杀掉所有登记进程的进程树。
//
// 内部只发起 taskkill（不等待结果），因此可以在退出路径上同步调用而不卡顿。
func (p *Procs) KillAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	procs := make([]*os.Process, 0, len(p.items))
	for _, proc := range p.items {
		procs = append(procs, proc)
	}
	p.mu.Unlock()
	for _, proc := range procs {
		killTree(proc)
	}
}

// RunOpts 描述一次外部命令执行。
//
// 这里刻意不叫 Process：它同时承载「看门狗」策略，是运行器的入参而不是进程本身。
type RunOpts struct {
	Command
	LogPath string        // 输出追加写入的日志文件
	Idle    time.Duration // 连续这么久没有进展就杀掉进程；0 表示不监控
	// Probes 是用于判断进展的目录（按真实落盘字节统计）。
	//
	// 留空表示「这个引擎的中间文件不可观测」，改为监视日志是否还在增长。
	// tdl 就是这样：它把数据攒在内存里分块写，目录可能几十秒都是 0 字节，
	// 但进度行每 100ms 刷一次。拿磁盘当判据会把正常下载误杀。
	Probes []string
	Procs  *Procs // 进程登记表，可为 nil
}

// Run 执行一条命令，输出追加写入日志。
//
// 停滞看门狗每 stallProbeInterval 采样一次进展指标，连续 Idle 时长没有变化
// 就杀掉进程并返回 ErrStalled。指标有两种，由 Probes 决定：
//
//   - 给了 Probes：看这些目录的真实落盘字节。RE 在解析清单、重试分片时
//     都可能长时间不打印进度，只有磁盘上的字节数才说明它真的在下。
//   - 没给 Probes：看日志文件是否还在增长。适用于中间文件不可见的引擎。
func Run(ctx context.Context, o RunOpts) error {
	if err := ctx.Err(); err != nil {
		return ErrCancelled
	}
	if o.Exe == "" {
		return fmt.Errorf("未指定可执行文件")
	}
	if err := os.MkdirAll(filepath.Dir(o.LogPath), 0o755); err != nil {
		return fmt.Errorf("创建日志目录失败: %w", err)
	}
	logFile, err := os.OpenFile(o.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %w", err)
	}
	defer func() { _ = logFile.Close() }()

	cmd := exec.Command(o.Exe, o.Args...)
	hideWindow(cmd)
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 %s 失败: %w", filepath.Base(o.Exe), err)
	}
	o.Procs.Track(cmd)

	var (
		stopWatch chan struct{}
		watchDone chan struct{}
		stalled   bool
		stalledMu sync.Mutex
	)
	// 选一个进展指标：优先磁盘字节，其次日志增长。
	progress := func() int64 { return dirBytes(o.Probes...) }
	if len(o.Probes) == 0 {
		if o.LogPath == "" {
			progress = nil // 两者都没有，无法判断，干脆不监控
		} else {
			progress = func() int64 { return fileSize(o.LogPath) }
		}
	}
	if o.Idle > 0 && progress != nil {
		stopWatch = make(chan struct{})
		watchDone = make(chan struct{})
		go func() {
			defer close(watchDone)
			ticker := time.NewTicker(stallProbeInterval)
			defer ticker.Stop()

			last := progress()
			lastMove := time.Now()
			for {
				select {
				case <-stopWatch:
					return
				case <-ticker.C:
				}
				cur := progress()
				if cur != last {
					// 指标变化（含分片被清理导致的减少）都算在动。
					last, lastMove = cur, time.Now()
					continue
				}
				if time.Since(lastMove) >= o.Idle {
					stalledMu.Lock()
					stalled = true
					stalledMu.Unlock()
					killTree(cmd.Process)
					return
				}
			}
		}()
	}

	waitErr := cmd.Wait()
	o.Procs.Untrack(cmd)
	if stopWatch != nil {
		close(stopWatch)
		<-watchDone
	}

	stalledMu.Lock()
	wasStalled := stalled
	stalledMu.Unlock()

	if wasStalled && waitErr != nil && ctx.Err() == nil {
		return ErrStalled
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ErrCancelled
		}
		return fmt.Errorf("%s 退出异常: %w", filepath.Base(o.Exe), waitErr)
	}
	return nil
}

// hideWindow 压掉外部 exe 的控制台黑框。
func hideWindow(cmd *exec.Cmd) {
	if cmd == nil {
		return
	}
	// CREATE_NO_WINDOW = 0x08000000
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}

// killTree 杀掉进程及其子进程（Windows: taskkill /T /F）。
func killTree(p *os.Process) {
	if p == nil {
		return
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(p.Pid), "/T", "/F")
	hideWindow(cmd)
	// 只发起、不等待：taskkill 是独立进程，父进程退出后它照样会跑完。
	// 若这里等结果，多个任务叠加就会把点 X 拖成秒级。
	if err := cmd.Start(); err == nil {
		go func() { _ = cmd.Wait() }()
	}
	_ = p.Kill()
}

// dirBytes 统计若干目录下所有文件的字节总和（目录不存在按 0 计）。
//
// 注意：这里必须对每个文件调用 os.Stat，不能直接用 WalkDir 拿到的
// DirEntry.Info()。后者走的是目录枚举（FindFirstFile），Windows 上正在被
// 写入的文件，其目录项大小可能长时间停留在旧值甚至 0，会把「正在下载」
// 误判成「停滞」。os.Stat 走文件句柄查询，拿到的是实时大小。
func dirBytes(dirs ...string) int64 {
	var total int64
	for _, dir := range dirs {
		_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d == nil || d.IsDir() {
				return nil
			}
			if info, ierr := os.Stat(p); ierr == nil {
				total += info.Size()
			}
			return nil
		})
	}
	return total
}

// fileSize 返回文件大小（不存在按 0 计）。
func fileSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}

// AppendLog 往日志追加一行诊断信息。
//
// 解析器只认各家自己的进度行，认不出的行会被忽略，不会干扰进度。
func AppendLog(path, line string) {
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line + "\n")
	_ = f.Close()
}
