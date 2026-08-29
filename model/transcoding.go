package model

// Transcoding 是一条转码配置：把音频转成目标格式时使用的命令与默认码率。
// 播放客户端可各自绑定不同的转码配置（见 Player.TranscodingId）。
type Transcoding struct {
	ID           string `structs:"id" json:"id"`
	Name         string `structs:"name" json:"name"`
	TargetFormat string `structs:"target_format" json:"targetFormat"` // 目标格式，如 mp3、opus
	// Command 是 ffmpeg 命令模板，内含 %s（输入文件）与 %b（码率）等占位符
	Command        string `structs:"command" json:"command"`
	DefaultBitRate int    `structs:"default_bit_rate" json:"defaultBitRate"`
}

type Transcodings []Transcoding

// TranscodingRepository 是转码配置仓储接口。
type TranscodingRepository interface {
	Get(id string) (*Transcoding, error)
	CountAll(...QueryOptions) (int64, error)
	Put(*Transcoding) error
	// FindByFormat 按目标格式查找配置，供客户端只指定格式时使用
	FindByFormat(format string) (*Transcoding, error)
}
