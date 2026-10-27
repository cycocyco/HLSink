package engine

import (
	"sort"
	"strings"
)

// CleanHeaderValue 过滤换行/控制字符，防止 header injection。
//
// 导出是因为入参校验（HTTP 接口）也要用同一套规则：清洗逻辑只有一份，
// 不会出现「接口放行、执行时被截断」这种不一致。
func CleanHeaderValue(v string) string {
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

// sortedHeaderKeys 返回排序后的键，保证参数可复现（便于对比日志、复现问题）。
func sortedHeaderKeys(headers map[string]string) []string {
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// headerLines 生成 "Key: Value" 行，供 -H / --add-header 使用。
// skip 返回 true 的键会被跳过（由各引擎声明自己的例外，如 yt-dlp 的 UA/Referer）。
func headerLines(headers map[string]string, skip func(string) bool) []string {
	if len(headers) == 0 {
		return nil
	}
	out := make([]string, 0, len(headers))
	for _, k := range sortedHeaderKeys(headers) {
		if skip != nil && skip(k) {
			continue
		}
		v := CleanHeaderValue(headers[k])
		if v == "" {
			continue
		}
		out = append(out, k+": "+v)
	}
	return out
}

// hasHeader 判断 headers 里是否已有某个头（大小写不敏感）。
func hasHeader(headers map[string]string, name string) bool {
	for k := range headers {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}
