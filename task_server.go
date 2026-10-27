package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"hlsink/internal/engine"
)

// listenPort 本地 HTTP 端口（扩展固定连这个端口）。
const listenPort = 18752

// 入参限制。
const (
	maxTaskBodyBytes   = 512 << 10 // 512 KiB
	maxURLLen          = 8192
	maxHeaders         = 32
	maxHeaderNameLen   = 128
	maxHeaderValueLen  = 8192
	maxHeaderTotalByte = 64 << 10
	maxTitleLen        = 300
)

// errQueueFull 队列已满（对外返回 429）。
var errQueueFull = errors.New("任务队列已满，请等待现有任务完成")

// TaskRequest 是 HTTP /task 的入参 DTO。
//
// 只接收这 6 个字段：服务端自己生成 ID/状态/路径等内部字段，
// 客户端无法注入 Message/Status/Progress/OutputPath/WorkDir。
// Kind 只是「格式提示」（hls / dash / page），缺省时由服务端按 URL 判定。
type TaskRequest struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Referer  string            `json:"referer"`
	Title    string            `json:"title"`
	Filename string            `json:"filename"`
	Kind     string            `json:"kind"`
}

// taskInput 是校验后的入参。
type taskInput struct {
	URL      string
	Headers  map[string]string
	Referer  string
	Title    string
	Filename string
	Kind     string
}

// httpServer 包一层 http.Server，便于统一关闭。
type httpServer struct {
	srv *http.Server
}

// close 优雅关闭 HTTP 服务，超时后强制关闭。
func (s *httpServer) close(timeout time.Duration) {
	if s == nil || s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
	_ = s.srv.Close()
}

// startTaskServer 常驻 HTTP：接收浏览器扩展发来的任务。
func (a *App) startTaskServer() {
	mux := http.NewServeMux()
	mux.HandleFunc("/task", a.handleTask)
	mux.HandleFunc("/ping", a.handlePing)

	srv := &http.Server{
		Addr:              fmt.Sprintf("127.0.0.1:%d", listenPort),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		// 上一个实例刚退出时端口可能处于 TIME_WAIT，重试一次再放弃。
		time.Sleep(600 * time.Millisecond)
		ln, err = net.Listen("tcp", srv.Addr)
		if err != nil {
			println("task server listen failed:", err.Error())
			return
		}
	}

	a.mu.Lock()
	a.server = &httpServer{srv: srv}
	a.mu.Unlock()

	a.emit("server-ready", listenPort)

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		println("task server stopped:", err.Error())
	}
}

// handlePing 健康检查。
func (a *App) handlePing(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && !isExtensionOrigin(origin) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"app": "HLSink", "version": "1.0.0"})
}

// handleTask 接收一个下载任务。
func (a *App) handleTask(w http.ResponseWriter, r *http.Request) {
	// 只允许浏览器扩展和无 Origin 的本机调用（防任意网页往本机端口塞任务）
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" && !isExtensionOrigin(origin) {
		http.Error(w, "forbidden origin", http.StatusForbidden)
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxTaskBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req TaskRequest
	if err := dec.Decode(&req); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad payload: "+err.Error(), http.StatusBadRequest)
		return
	}

	input, err := normalizeTaskRequest(&req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id, duplicate, err := a.createTask(input)
	if err != nil {
		if errors.Is(err, errQueueFull) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"id": id, "duplicate": duplicate})
}

// createTask 建任务并启动 run。同 URL 且未终态的任务直接复用（去重）。
func (a *App) createTask(in *taskInput) (id string, duplicate bool, err error) {
	a.mu.Lock()
	if exist := a.findActiveByURLLocked(in.URL); exist != nil {
		existingID := exist.ID
		a.mu.Unlock()
		return existingID, true, nil
	}
	if a.activeCountLocked() >= maxActiveTasks {
		a.mu.Unlock()
		return "", false, errQueueFull
	}
	newID, err := a.newTaskIDLocked()
	if err != nil {
		a.mu.Unlock()
		return "", false, err
	}
	saveDir := strings.TrimSpace(a.settings.DownloadDir)
	if saveDir == "" {
		saveDir = defaultDownloadDir()
	}
	t := &Task{
		ID:        newID,
		URL:       in.URL,
		Headers:   in.Headers,
		Referer:   in.Referer,
		Title:     in.Title,
		Filename:  in.Filename,
		Kind:      in.Kind,
		Status:    statusQueued,
		CreatedAt: time.Now(),
		WorkDir:   taskWorkDir(newID),
		SaveDir:   saveDir,
		transcode: a.settings.HEVCTranscode,
		threadCount: normalizeThreadCount(
			a.settings.ThreadCount,
		),
	}
	a.tasks[newID] = t
	a.order = append(a.order, newID)
	snap := t.clone()
	a.mu.Unlock()

	a.emit("task-added", snap)

	if err := a.launchRun(newID); err != nil {
		msg := "启动任务失败: " + err.Error()
		a.mutate(newID, nil, func(x *Task) {
			x.Status = statusError
			x.Error = msg
			x.Message = msg
		})
		return "", false, fmt.Errorf("%s", msg)
	}
	return newID, false, nil
}

// normalizeTaskRequest 严格校验并规整入参。
func normalizeTaskRequest(req *TaskRequest) (*taskInput, error) {
	if req == nil {
		return nil, fmt.Errorf("请求体为空")
	}
	rawURL := strings.TrimSpace(req.URL)
	if rawURL == "" {
		return nil, fmt.Errorf("url 不能为空")
	}
	if len(rawURL) > maxURLLen {
		return nil, fmt.Errorf("url 过长（上限 %d 字节）", maxURLLen)
	}
	if strings.ContainsAny(rawURL, "\r\n\t ") {
		return nil, fmt.Errorf("url 含非法空白字符")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("url 格式错误: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return nil, fmt.Errorf("只允许 http/https 链接")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("url 缺少主机名")
	}

	// 格式：优先用客户端提示，否则按 URL 后缀判定；显式传入非法值直接报错，避免静默降级。
	kind := ""
	if raw := strings.TrimSpace(req.Kind); raw != "" {
		k := engine.NormalizeKind(raw)
		if !k.Known() {
			return nil, fmt.Errorf("不支持的源格式: %s（仅支持 hls / dash / page / telegram）", raw)
		}
		kind = string(k)
	}
	if kind == "" {
		kind = string(engine.DetectKind(rawURL))
	}

	headers, err := sanitizeHeaders(req.Headers)
	if err != nil {
		return nil, err
	}

	referer := ""
	if ref := strings.TrimSpace(req.Referer); ref != "" {
		if len(ref) > maxURLLen {
			return nil, fmt.Errorf("referer 过长")
		}
		if strings.ContainsAny(ref, "\r\n") {
			return nil, fmt.Errorf("referer 含非法字符")
		}
		if ru, rerr := url.Parse(ref); rerr == nil && ru.Host != "" {
			switch strings.ToLower(ru.Scheme) {
			case "http", "https":
				referer = ref
			}
		}
	}

	return &taskInput{
		URL:      rawURL,
		Headers:  headers,
		Referer:  referer,
		Title:    clampText(req.Title, maxTitleLen),
		Filename: clampText(req.Filename, maxTitleLen),
		Kind:     kind,
	}, nil
}

// sanitizeHeaders 过滤非法头名/空值，并限制数量与总长度。
func sanitizeHeaders(in map[string]string) (map[string]string, error) {
	if len(in) == 0 {
		return map[string]string{}, nil
	}
	if len(in) > maxHeaders {
		return nil, fmt.Errorf("请求头数量过多（上限 %d）", maxHeaders)
	}
	out := make(map[string]string, len(in))
	total := 0
	for k, v := range in {
		key := strings.TrimSpace(k)
		if key == "" || len(key) > maxHeaderNameLen || !isHeaderToken(key) {
			continue // 静默丢弃非法头名，不因噪音导致整单失败
		}
		val := engine.CleanHeaderValue(v)
		if val == "" {
			continue
		}
		if len(val) > maxHeaderValueLen {
			return nil, fmt.Errorf("请求头 %s 过长（上限 %d 字节）", key, maxHeaderValueLen)
		}
		total += len(key) + len(val)
		if total > maxHeaderTotalByte {
			return nil, fmt.Errorf("请求头总长度超限")
		}
		out[key] = val
	}
	return out, nil
}

// isHeaderToken 判断是否是合法的 HTTP 头名（RFC 7230 token）。
func isHeaderToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

// clampText 去掉控制字符、首尾空白并限制长度。
func clampText(s string, maxRunes int) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if maxRunes > 0 {
		if runes := []rune(out); len(runes) > maxRunes {
			out = strings.TrimSpace(string(runes[:maxRunes]))
		}
	}
	return out
}

// isExtensionOrigin 判断 Origin 是否来自浏览器扩展。
func isExtensionOrigin(origin string) bool {
	lower := strings.ToLower(origin)
	for _, prefix := range []string{
		"chrome-extension://",
		"moz-extension://",
		"safari-web-extension://",
		"extension://",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// writeJSON 输出 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
