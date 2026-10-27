package engine

import (
	"sort"
	"sync"
)

// 引擎注册表。
//
// 热插拔的关键：每个实现文件在自己的 init() 里登记，注册表不写死任何名字。
// 加一个新工具 = 新建 downloader_xxx.go + init() 登记，不改本文件。
//
// 顺序由 Priority 决定，而不是 init() 的执行顺序（它依赖文件名字母序，
// 靠它排降级链迟早出事）。
var (
	regMu          sync.Mutex
	registry       []Downloader
	transcoderImpl Transcoder
)

// Register 登记一个下载引擎。由各实现文件的 init() 调用。
func Register(d Downloader) {
	if d == nil {
		return
	}
	regMu.Lock()
	for _, x := range registry {
		if x.Key() == d.Key() {
			regMu.Unlock()
			return // 同一个 key 重复登记：以第一次为准
		}
	}
	registry = append(registry, d)
	sort.Slice(registry, func(i, j int) bool {
		return registry[i].Priority() < registry[j].Priority()
	})
	regMu.Unlock()
}

// RegisterTranscoder 登记后处理工具（目前只有 ffmpeg）。
func RegisterTranscoder(t Transcoder) {
	regMu.Lock()
	transcoderImpl = t
	regMu.Unlock()
}

// List 返回全部已注册的下载引擎，按 Priority 排序。
//
// 注意：这个顺序只用于「设置页逐行展示」这类遍历，**不是降级链**。
// 降级链完全由每个引擎的 Fallback() 决定：tdl 只认 Telegram 链接且
// Fallback 为空，所以它永远不在 HLS / 网页那条链上。
func List() []Downloader {
	regMu.Lock()
	defer regMu.Unlock()
	out := make([]Downloader, len(registry))
	copy(out, registry)
	return out
}

// Get 按 key 取引擎。
func Get(key string) (Downloader, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, d := range registry {
		if d.Key() == key {
			return d, true
		}
	}
	return nil, false
}

// Pick 返回第一个认领该类型的引擎；没有则返回 nil。
//
// 调用方拿到 nil 时应直接报错，而不是静默换一个能跑的——那会掩盖配置问题。
func Pick(kind Kind) Downloader {
	regMu.Lock()
	defer regMu.Unlock()
	for _, d := range registry {
		if d.CanHandle(kind) {
			return d
		}
	}
	return nil
}

// GetTranscoder 返回后处理工具，未登记时返回 nil。
func GetTranscoder() Transcoder {
	regMu.Lock()
	defer regMu.Unlock()
	return transcoderImpl
}
