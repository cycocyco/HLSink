package engine

// ffmpeg：后处理工具，目前只用于 HEVC（NVENC）转码。
//
// 与 Downloader 的区别：没有 URL、没有进度、不参与降级链。
// 它只在下载完成后跑一次，输入一个文件、输出一个文件。

func init() { RegisterTranscoder(ffmpegTranscoder{}) }

type ffmpegTranscoder struct{}

func (ffmpegTranscoder) Key() string    { return KeyFFmpeg }
func (ffmpegTranscoder) Label() string  { return "ffmpeg" }
func (ffmpegTranscoder) Binary() string { return "ffmpeg.exe" }

// Build 拼装 HEVC 转码参数。
//
// 用 NVENC 硬编而非 libx265：同样的画质档位下速度快一个量级，
// 代价是没有 NVIDIA 显卡时会直接失败（调用方按"转码失败保留原画"处理）。
func (ffmpegTranscoder) Build(src, dst string) (Command, error) {
	return Command{Args: []string{
		"-y", "-hide_banner", "-nostdin",
		"-i", src,
		"-c:v", "hevc_nvenc", "-preset", "p5", "-cq", "23",
		"-c:a", "copy",
		"-movflags", "+faststart",
		dst,
	}}, nil
}
