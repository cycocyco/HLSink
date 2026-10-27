// Package engine 收敛所有「外部命令行工具」的调用边界。
//
// 三条设计约束（刻意为之，别破坏）：
//
//  1. 本包不 import 主程序。它不认识 Task / 窗口 / 配置 / Wails，
//     只接收 Job 这种纯数据，因此可以被单独测试，也不会出现循环依赖。
//  2. 新增一个工具 = 新建一个文件 + init() 注册，不改任何既有文件。
//     这就是「热插拔」的实际含义：改一个工具的参数不会碰到另一个工具。
//  3. 进程执行、停滞看门狗、工具定位、日志脱敏等公共能力集中在本包，
//     各实现只负责「我该拼什么参数」和「我这行输出是什么意思」。
//
// 分工：
//
//	Downloader  —— 输入 URL、输出文件的引擎（RE / yt-dlp / 以后的 tdl）
//	Transcoder  —— 后处理工具（目前只有 ffmpeg），没有 URL、没有进度
package engine

import (
	"errors"
	"time"
)

// Kind 是下载源的类型。决定由哪个 Downloader 接手。
//
// 新增来源类型时在这里加一个常量，并在相应实现的 CanHandle 里认领即可。
type Kind string

const (
	KindHLS  Kind = "hls"  // HLS 清单（.m3u8）
	KindDASH Kind = "dash" // DASH 清单（.mpd）
	KindPage Kind = "page" // 网页地址或未知后缀，交给站点解析器

	// KindTelegram 是 Telegram 消息链接（t.me/...）。它不是流媒体，
	// 产物可能是一张图、一个压缩包、一段视频，扩展名事先不可知。
	KindTelegram Kind = "telegram"
)

// 引擎 key 的规范值。配置项、日志、前端都引用这些常量，避免各处手写字符串拼错。
const (
	KeyRE     = "re"
	KeyYtdlp  = "yt-dlp"
	KeyTdl    = "tdl"
	KeyFFmpeg = "ffmpeg"
)

// Errors 可被调用方用 errors.Is 判定，从而区分「用户取消」和「真的失败」。
var (
	// ErrCancelled 表示主动取消（暂停/删除/退出），绝不能当失败处理。
	ErrCancelled = errors.New("任务已取消")
	// ErrStalled 表示引擎在守门狗时限内没有任何字节落地，被主动终止。
	ErrStalled = errors.New("引擎无下载进展")
)

// Job 是一次下载的纯数据描述。引擎从这里读取它需要的一切，不再回查任务状态。
//
// 字段全部是值或只读切片/映射：引擎实现不允许修改它们。
type Job struct {
	URL     string
	Kind    Kind
	OutDir  string // 成品输出目录
	TmpDir  string // 分片/临时目录（HLS 类引擎需要，其它可为空）
	Threads int
	Headers map[string]string
	Referer string // 单独字段，防盗链头
	Proxy   string // 形如 http://127.0.0.1:7890；空串表示不走代理
	Name    string // 输出文件名前缀（不含扩展名）
}

// Tools 是外部依赖的定位结果，由调用方解析后传入。
//
// 引擎不自己去找 ffmpeg：定位逻辑只有一份（Resolve），避免每个引擎各写一套。
type Tools struct {
	FFmpeg    string // ffmpeg 可执行文件绝对路径，空串表示未找到
	FFmpegDir string // ffmpeg 所在目录（yt-dlp 的 --ffmpeg-location 需要）
}

// Progress 是引擎输出的一行解析结果。
type Progress struct {
	Percent  float64 // 0~100
	Speed    string  // 已格式化的速度文本，如 "2.5MiB/s"
	Segments string  // 分片进度，如 "12/1760"；无此概念时留空
}

// Command 是一条待执行的命令。
type Command struct {
	Exe  string
	Args []string
	// Notes 是需要写进运行日志的诊断行（不参与执行）。
	// 引擎借此说明「我为了什么改了行为」，例如走了代理、跳过了某项自动处理，
	// 而不必自己持有日志文件路径。由调用方统一写，格式保持一致。
	Notes []string
}

// Downloader 是「输入 URL、输出文件」的引擎。
//
// 实现方只需关心四件事：认不认这种源、参数怎么拼、输出怎么读、多久算卡死。
type Downloader interface {
	// Key 是稳定标识，会下发到前端（如 "re" / "yt-dlp"）。
	Key() string
	// Label 是给人看的名字（设置页、错误信息）。
	Label() string
	// Binary 是可执行文件名，用于路径解析（如 "N_m3u8DL-RE.exe"）。
	Binary() string
	// Priority 决定「列表里的展示顺序」，小的在前。
	// 它不决定降级链——链是 Fallback() 串起来的，两者没有关系。
	Priority() int
	// CanHandle 认领某种源类型。
	CanHandle(Kind) bool
	// Build 构造命令行。返回的错误会直接展示给用户，措辞要能指导操作。
	Build(Job, Tools) (Command, error)
	// ParseLine 解析一行输出。ok=false 表示这行不是进度。
	ParseLine(line string) (Progress, bool)
	// Stall 返回「多久没有字节落地就判定停滞」，0 表示不监控。
	Stall() time.Duration
	// Probes 返回用于判断是否有进展的目录（按真实落盘字节统计）。
	Probes(Job) []string
	// Hint 是引擎启动时给用户看的一句话（如「正在解析网页地址」），可为空。
	// 只在「首次选中的引擎」上显示，降级时的提示由编排层生成，不会被它覆盖。
	Hint() string
	// Fallback 返回本引擎失败后建议换用的引擎 key；空串表示没有下一级。
	//
	// 降级关系由引擎自己声明，编排层不需要写 "RE 失败就去 yt-dlp" 这种硬编码。
	Fallback() string
}

// Transcoder 是后处理工具。与 Downloader 的区别：没有 URL、没有进度、不参与降级。
type Transcoder interface {
	Key() string
	Label() string
	Binary() string
	Build(src, dst string) (Command, error)
}
