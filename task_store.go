package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"
)

// idFallbackCounter 兜底 ID 的自增序列，仅在 crypto/rand 不可用时使用。
var idFallbackCounter uint64

// randomHexID 用 crypto/rand 生成 n 字节的十六进制标识。
// crypto/rand 理论上不会失败，兜底时用时间戳 + 自增计数保证不冲突。
func randomHexID(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err == nil {
		return hex.EncodeToString(buf)
	}
	seq := atomic.AddUint64(&idFallbackCounter, 1)
	return fmt.Sprintf("%x%08x", time.Now().UnixNano(), seq)
}

// newRunID 生成一次运行的唯一标识。
func newRunID() string {
	return "r" + randomHexID(8)
}

// newTaskIDLocked 生成不与现有任务冲突的任务 ID。调用方必须持有 a.mu。
func (a *App) newTaskIDLocked() (string, error) {
	for i := 0; i < 16; i++ {
		id := "t" + randomHexID(8)
		if _, exists := a.tasks[id]; !exists {
			return id, nil
		}
	}
	return "", fmt.Errorf("生成任务 ID 失败")
}

// emit 广播事件（应用句柄未就绪时静默丢弃，避免启动早期 panic）。
func (a *App) emit(name string, payload interface{}) {
	a.mu.Lock()
	app := a.app
	a.mu.Unlock()
	if app == nil {
		return
	}
	app.Event.Emit(name, payload)
}

// mutate 在锁内修改任务并广播快照。guard 返回 false 表示放弃本次修改。
func (a *App) mutate(id string, guard func(*Task) bool, fn func(*Task)) bool {
	a.mu.Lock()
	t, ok := a.tasks[id]
	if !ok || (guard != nil && !guard(t)) {
		a.mu.Unlock()
		return false
	}
	fn(t)
	snap := t.clone()
	a.mu.Unlock()
	a.emit("task-updated", snap)
	return true
}

// activeStatus 判断状态是否属于「可被 run 写回」的阶段。
func activeStatus(status string) bool {
	switch status {
	case statusQueued, statusRunning, statusTranscoding:
		return true
	default:
		return false
	}
}

// mutateRun 只在「当前 run 且状态活跃」时写回，丢弃过期 run 的写回。
// 这是暂停/删除与下载进程退出之间竞态的核心防线。
func (a *App) mutateRun(id, runID string, fn func(*Task)) bool {
	return a.mutate(id, func(t *Task) bool {
		return t.RunID == runID && activeStatus(t.Status)
	}, fn)
}

// setStatusForRun 把状态从活跃态切到指定状态（如 queued → running）。
func (a *App) setStatusForRun(id, runID, status string, fn func(*Task)) bool {
	return a.mutate(id, func(t *Task) bool {
		return t.RunID == runID && activeStatus(t.Status)
	}, func(t *Task) {
		t.Status = status
		if fn != nil {
			fn(t)
		}
	})
}

// finishTask 写终态（done/error）。只有当前 run 且状态活跃时才生效，
// 所以被暂停/删除的任务不会被迟到的进程退出覆盖。
func (a *App) finishTask(id, runID, status, errText, message string) bool {
	return a.mutate(id, func(t *Task) bool {
		return t.RunID == runID && activeStatus(t.Status)
	}, func(t *Task) {
		t.Status = status
		t.Error = errText
		t.Message = message
		t.Speed = ""
		t.Segments = ""
		if status == statusDone {
			t.Progress = 100
		}
	})
}

// finishDone 写成功终态，同时记录精确成品路径。
func (a *App) finishDone(id, runID, outputPath, downloadPath, message string) bool {
	return a.mutate(id, func(t *Task) bool {
		return t.RunID == runID && activeStatus(t.Status)
	}, func(t *Task) {
		t.Status = statusDone
		t.Progress = 100
		t.Speed = ""
		t.Segments = ""
		t.Error = ""
		t.OutputPath = outputPath
		if downloadPath != "" {
			t.DownloadPath = downloadPath
		}
		t.Message = message
	})
}

// taskSnapshot 返回任务深拷贝，任务不存在时返回 nil。
func (a *App) taskSnapshot(id string) *Task {
	a.mu.Lock()
	defer a.mu.Unlock()
	if t, ok := a.tasks[id]; ok {
		return t.clone()
	}
	return nil
}

// activeCountLocked 统计「真正占用资源」的任务数。调用方必须持有 a.mu。
//
// 只算会推进流程的活跃态：paused 不占并发槽位、cleanup_failed 只是等待重试清理，
// 若把它们也算进队列上限，清理失败的任务会白占名额，把新任务挡在门外。
func (a *App) activeCountLocked() int {
	n := 0
	for _, t := range a.tasks {
		if activeStatus(t.Status) {
			n++
		}
	}
	return n
}

// findActiveByURLLocked 找同 URL 且未终态的任务（用于去重）。调用方必须持有 a.mu。
func (a *App) findActiveByURLLocked(rawURL string) *Task {
	for _, id := range a.order {
		t, ok := a.tasks[id]
		if !ok || t.URL != rawURL {
			continue
		}
		switch t.Status {
		case statusDone, statusError:
			continue
		}
		return t
	}
	return nil
}

// clearRun 移除 taskID 的 run 记录（只移除自己，避免误删新 run）。
func (a *App) clearRun(id string, rs *runState) {
	a.mu.Lock()
	if cur, ok := a.runs[id]; ok && cur == rs {
		delete(a.runs, id)
	}
	a.mu.Unlock()
}
