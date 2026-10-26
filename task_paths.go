package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// appName 应用私有目录名（回退用户目录时使用）。
const appName = "HLSink"

// videoExts 视为成品视频的扩展名集合（小写）。
var videoExts = map[string]bool{
	".mp4":  true,
	".mkv":  true,
	".ts":   true,
	".m4v":  true,
	".mov":  true,
	".webm": true,
	".flv":  true,
	".avi":  true,
}

// publishMu 串行化「查重名 + 移动文件」。同名任务并发发布时，
// 如果没有覆盖整个窗口的锁，两个任务可能同时挑中同一个目标名并互相覆盖。
var publishMu sync.Mutex

var (
	dataDirOnce sync.Once
	dataDirPath string
)

// appDataDir 返回应用数据根目录：优先 exe 同级的 data 目录（绿色便携），
// exe 目录不可写时回退到用户配置目录。结果进程内只计算一次。
func appDataDir() string {
	dataDirOnce.Do(func() {
		if exe, err := os.Executable(); err == nil {
			dir := filepath.Join(filepath.Dir(exe), "data")
			if err := os.MkdirAll(dir, 0o755); err == nil {
				// 写探测：目录存在但无写权限时也能正确回退。
				if f, ferr := os.CreateTemp(dir, ".probe-*"); ferr == nil {
					name := f.Name()
					_ = f.Close()
					_ = os.Remove(name)
					dataDirPath = dir
				}
			}
		}
		if dataDirPath == "" {
			base, err := os.UserConfigDir()
			if err != nil || strings.TrimSpace(base) == "" {
				base = os.TempDir()
			}
			dataDirPath = filepath.Join(base, appName)
			_ = os.MkdirAll(dataDirPath, 0o755)
		}
	})
	return dataDirPath
}

// workRoot 返回应用工作区根目录：<data>/work。
// 所有 RE / yt-dlp / ffmpeg 的中间产物与日志都只写在这里。
func workRoot() string {
	return filepath.Join(appDataDir(), "work")
}

// taskWorkDir 返回某个任务的私有工作目录。
//
// 任务 ID 正常由服务端随机生成；这里仍做边界校验，确保即使内部误传
// "." / ".." / 路径分隔符等异常值，也绝不可能拼出 workRoot 之外的路径。
// 非法 ID 被映射到 workRoot/_invalid/<hash>，嵌套目录不会被 removeWorkDir
// 当作任务级目录删除，属于安全的失败路径。
func taskWorkDir(id string) string {
	if !validWorkID(id) {
		sum := sha256.Sum256([]byte(id))
		return filepath.Join(workRoot(), "_invalid", fmt.Sprintf("%x", sum[:8]))
	}
	return filepath.Join(workRoot(), id)
}

// validWorkID 只允许字母、数字、连字符和下划线，拒绝空值、点目录和路径分隔符。
func validWorkID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return false
		}
	}
	return true
}

// ensureTaskDirs 创建任务工作目录下的全部子目录。
func ensureTaskDirs(workDir string) error {
	for _, sub := range []string{"download", "tmp", "transcode", "logs"} {
		if err := os.MkdirAll(filepath.Join(workDir, sub), 0o755); err != nil {
			return fmt.Errorf("创建工作目录 %s 失败: %w", sub, err)
		}
	}
	return nil
}

// sanitizeName 生成安全的文件/stem 名称。
//
// 拒绝：空、"."、".."、Windows 保留名、控制字符、路径分隔符、结尾点/空格。
// 非法字符替换为 "_"，连续空白折叠为单个空格，超长截断到 120 个字符。
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == unicode.ReplacementChar:
			continue
		case r < 0x20 || r == 0x7f:
			continue
		case strings.ContainsRune(`\/:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	out = strings.Trim(out, " .")
	if out == "" || out == "." || out == ".." {
		return ""
	}
	if runes := []rune(out); len(runes) > 120 {
		out = strings.Trim(string(runes[:120]), " .")
	}
	if out == "" || isReservedName(out) {
		return ""
	}
	return out
}

// isReservedName 判断是否是 Windows 设备保留名（CON/PRN/AUX/NUL/COM1-9/LPT1-9）。
func isReservedName(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSpace(base)
	switch base {
	case "CON", "PRN", "AUX", "NUL":
		return true
	}
	if len(base) == 4 {
		head := base[:3]
		digit := base[3]
		if (head == "COM" || head == "LPT") && digit >= '1' && digit <= '9' {
			return true
		}
	}
	return false
}

// safeBaseName 用任务的文件名/标题生成用户友好的成品名（不含扩展名）。
// 全部不可用时回退 video_<shortID>。
func safeBaseName(filename, title, taskID string) string {
	for _, cand := range []string{filename, title} {
		name := sanitizeName(cand)
		if name == "" {
			continue
		}
		if ext := strings.ToLower(filepath.Ext(name)); videoExts[ext] {
			name = sanitizeName(strings.TrimSuffix(name, filepath.Ext(name)))
			if name == "" {
				continue
			}
		}
		return name
	}
	short := taskID
	if len(short) > 8 {
		short = short[:8]
	}
	short = sanitizeName(short)
	if short == "" {
		short = "unknown"
	}
	return "video_" + short
}

// exists 判断路径是否存在。
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// stillExists 判断路径是否仍然存在（用于删除后的残留检查）。
func stillExists(path string) bool {
	if path == "" {
		return false
	}
	return exists(path)
}

// uniqueName 在目录内生成不冲突的文件名：base.ext / base (1).ext / base (2).ext ...
// 绝不覆盖同名文件。
func uniqueName(dir, base, ext string) string {
	candidate := base + ext
	if !exists(filepath.Join(dir, candidate)) {
		return candidate
	}
	for i := 1; i < 10000; i++ {
		candidate = fmt.Sprintf("%s (%d)%s", base, i, ext)
		if !exists(filepath.Join(dir, candidate)) {
			return candidate
		}
	}
	return fmt.Sprintf("%s (%d)%s", base, os.Getpid(), ext)
}

// moveFile 移动文件：优先 rename；跨盘失败时回退为「复制 + fsync + 删除源」。
func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	out, err := os.Create(dst)
	if err != nil {
		_ = in.Close()
		return fmt.Errorf("创建目标文件失败: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = in.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("复制文件失败: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = in.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("同步目标文件失败: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = in.Close()
		_ = os.Remove(dst)
		return fmt.Errorf("关闭目标文件失败: %w", err)
	}
	if err := in.Close(); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("关闭源文件失败: %w", err)
	}
	if err := os.Remove(src); err != nil {
		return fmt.Errorf("删除源文件失败: %w", err)
	}
	return nil
}

// publishFile 把 staged 产物发布到 saveDir，返回精确成品路径。
// 同名自动加 (1)(2) 后缀，绝不覆盖。
func publishFile(staged, saveDir, base string) (string, error) {
	if staged == "" {
		return "", fmt.Errorf("没有可发布的产物")
	}
	if _, err := os.Stat(staged); err != nil {
		return "", fmt.Errorf("产物不存在: %w", err)
	}
	if strings.TrimSpace(saveDir) == "" {
		return "", fmt.Errorf("保存目录为空")
	}
	if err := os.MkdirAll(saveDir, 0o755); err != nil {
		return "", fmt.Errorf("创建保存目录失败: %w", err)
	}
	// 同一进程内的并发发布必须串行：查重名与 rename 之间不能插入另一个任务，
	// 否则两个同名任务可能选中同一个目标并互相覆盖。
	publishMu.Lock()
	defer publishMu.Unlock()
	ext := strings.ToLower(filepath.Ext(staged))
	if !videoExts[ext] {
		ext = ".mp4"
	}
	name := uniqueName(saveDir, base, ext)
	dest := filepath.Join(saveDir, name)
	if err := moveFile(staged, dest); err != nil {
		return "", fmt.Errorf("发布成品失败: %w", err)
	}
	return dest, nil
}

// findStaged 在任务 staging 目录内查找下载产物。
// 只认「download.」前缀 + 视频扩展名的精确文件，且只在 staging 内找，绝不去保存目录扫描。
func findStaged(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("读取任务下载目录失败: %w", err)
	}
	best := ""
	var bestSize int64 = -1
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), "download.") {
			continue
		}
		ext := strings.ToLower(filepath.Ext(name))
		if !videoExts[ext] {
			continue
		}
		info, err := e.Info()
		if err != nil || info.Size() <= 0 {
			continue
		}
		if info.Size() > bestSize {
			best = filepath.Join(dir, name)
			bestSize = info.Size()
		}
	}
	if best == "" {
		return "", fmt.Errorf("下载完成但 staging 目录内没有找到视频产物")
	}
	return best, nil
}

// clearStaged 清掉 staging 目录内上一轮引擎的产物（仅前缀 download. 的文件）。
//
// 只在任务专属 staging 内操作，且只删「download.」前缀文件：
// 既不会碰用户文件，也不会误删日志目录。用于换引擎重试前避免输出名冲突。
func clearStaged(dir string) {
	if strings.TrimSpace(dir) == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(strings.ToLower(name), "download.") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// pathWithin 判断 p 是否位于 root 之内（含 root 本身）。
func pathWithin(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	cleanP := filepath.Clean(p)
	cleanRoot := filepath.Clean(root)
	rel, err := filepath.Rel(cleanRoot, cleanP)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	if filepath.IsAbs(rel) {
		return false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// isVolumeRoot 判断是否是盘符根（如 C:\）。
func isVolumeRoot(p string) bool {
	clean := filepath.Clean(p)
	return filepath.Dir(clean) == clean
}

// samePath 判断两个路径是否指向同一位置（Windows 大小写不敏感）。
func samePath(a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// removeWorkDir 物理删除任务工作目录。
//
// 严格校验：必须是 app work root 下的一级任务目录，且不等于 work root 本身。
// 这保证删除永远不可能波及用户的其他文件。
func removeWorkDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("工作目录为空，拒绝删除")
	}
	clean := filepath.Clean(dir)
	root := filepath.Clean(workRoot())
	if samePath(clean, root) {
		return fmt.Errorf("拒绝删除工作区根目录: %s", clean)
	}
	if !pathWithin(clean, root) {
		return fmt.Errorf("拒绝删除工作区之外的路径: %s", clean)
	}
	if !samePath(filepath.Dir(clean), root) {
		return fmt.Errorf("拒绝删除非任务级工作目录: %s", clean)
	}
	if err := os.RemoveAll(clean); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除工作目录失败(%s): %w", clean, err)
	}
	return nil
}

// removeOutputFile 删除一个「已记录」的文件。文件不存在视为成功。
//
// allowedRoot 限定删除范围（成品必须在任务保存目录内，中间产物必须在任务工作目录内），
// 且拒绝删除盘符根。调用方只传记录过的精确路径，绝不传目录。
func removeOutputFile(path, allowedRoot string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("路径不是绝对路径，拒绝删除: %s", clean)
	}
	if isVolumeRoot(clean) {
		return fmt.Errorf("拒绝删除盘符根目录: %s", clean)
	}
	if strings.TrimSpace(allowedRoot) != "" && !pathWithin(clean, allowedRoot) {
		return fmt.Errorf("路径不在允许的目录内，拒绝删除: %s", clean)
	}
	info, err := os.Stat(clean)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("检查文件失败(%s): %w", clean, err)
	}
	if info.IsDir() {
		return fmt.Errorf("路径是目录，拒绝删除: %s", clean)
	}
	if err := os.Remove(clean); err != nil {
		return fmt.Errorf("删除文件失败(%s): %w", clean, err)
	}
	return nil
}

// cleanupStaging 删除任务 staging 下的中间产物（下载、分片、转码临时文件）。
// 仅在成功发布成品后调用，用于不留几 GB 垃圾；日志目录保留。
func cleanupStaging(workDir string) {
	if strings.TrimSpace(workDir) == "" {
		return
	}
	for _, sub := range []string{"download", "tmp", "transcode"} {
		p := filepath.Join(workDir, sub)
		if pathWithin(p, workDir) && !samePath(p, workDir) {
			_ = os.RemoveAll(p)
		}
	}
}
