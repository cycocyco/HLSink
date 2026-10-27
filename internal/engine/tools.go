package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// subdirs 是目录里找不到可执行文件时的下探顺序。
//
// ffmpeg 官方 Windows 包是 <root>\bin\ffmpeg.exe 这种结构，用户填的大多是
// 解压根目录，所以必须往下探一层，否则会拼出一个不存在的路径。
var subdirs = []string{"bin", "tools", "deps"}

// findInDir 在 dir 本身及其常见子目录里查找 binary，找不到返回空串。
func findInDir(dir, binary string) string {
	if st, err := os.Stat(filepath.Join(dir, binary)); err == nil && !st.IsDir() {
		return filepath.Join(dir, binary)
	}
	for _, sub := range subdirs {
		cand := filepath.Join(dir, sub, binary)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand
		}
	}
	return ""
}

// Resolve 把用户设置解析成可执行文件的绝对路径。
//
// raw 支持：完整 exe 路径 / 所在目录 / PATH 中的命令名；留空则自动在
// exe 旁及其 tools|deps|bin 子目录、系统 PATH 中查找。找不到返回空字符串。
//
// 注意：FindInDir 与 Resolve 都不认识具体是哪个工具，binary 由调用方给。
// 加一个新工具不需要动这里。
func Resolve(raw, binary string) string {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		if st, err := os.Stat(raw); err == nil {
			if st.IsDir() {
				if p := findInDir(raw, binary); p != "" {
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
		if p := findInDir(filepath.Dir(exe), binary); p != "" {
			return p
		}
	}
	if p, err := exec.LookPath(binary); err == nil {
		return p
	}
	return ""
}

// Status 是单个工具的解析结果，供设置页回显「到底认没认出来」。
type Status struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Found bool   `json:"found"`
	Note  string `json:"note"`
}

// Check 解析一个工具并给出可读结论。
//
// 用户填目录、填 exe、留空自动找，最终都走 Resolve，
// 这里直接把命中结果报出来——填完不用猜，也不用等到下载失败才发现。
func Check(key, name, raw, binary string) Status {
	p := Resolve(raw, binary)
	out := Status{Key: key, Name: name, Path: p}
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

// Executable 返回可执行文件的可用路径；不可用时返回带指导的 error。
//
// 三个引擎都要做这套校验，抽出来一处维护，报错文案也统一。
func Executable(raw, binary, label string) (string, error) {
	p := Resolve(raw, binary)
	if p == "" {
		return "", &MissingError{Label: label, Binary: binary}
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() {
		return "", &WrongPathError{Label: label, Path: p}
	}
	return p, nil
}

// MissingError 表示工具完全没找到（多半是没设置路径）。
type MissingError struct {
	Label  string
	Binary string
}

func (e *MissingError) Error() string {
	return "未找到 " + e.Label + "，请在设置里指定它的路径"
}

// WrongPathError 表示路径有值但指向的位置没有这个文件。
type WrongPathError struct {
	Label string
	Path  string
}

func (e *WrongPathError) Error() string {
	return e.Label + " 不在指定位置: " + e.Path
}
