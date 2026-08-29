package model

import (
	"time"
)

// Player 代表一个播放客户端实例。同一用户使用不同客户端（网页、手机 App）
// 会各自登记一条记录，从而支持按客户端分别配置转码与码率上限。
type Player struct {
	Username string `structs:"-" json:"userName"` // 由 JOIN 得到，不入库

	ID   string `structs:"id" json:"id"`
	Name string `structs:"name" json:"name"`
	// UserAgent/Client 用于识别客户端；Client 来自 Subsonic 请求的 c 参数
	UserAgent string    `structs:"user_agent" json:"userAgent"`
	UserId    string    `structs:"user_id" json:"userId"`
	Client    string    `structs:"client" json:"client"`
	IP        string    `structs:"ip" json:"ip"`
	LastSeen  time.Time `structs:"last_seen" json:"lastSeen"`
	// TranscodingId/MaxBitRate 为该客户端的转码配置，
	// 可针对带宽受限的设备单独降码率
	TranscodingId string `structs:"transcoding_id" json:"transcodingId"`
	MaxBitRate    int    `structs:"max_bit_rate" json:"maxBitRate"`
	// ReportRealPath 为 true 时向客户端暴露文件真实路径，
	// 供能直接读取文件系统的客户端使用
	ReportRealPath bool `structs:"report_real_path" json:"reportRealPath"`
	// ScrobbleEnabled 控制该客户端的播放是否上报到 Last.fm 等外部服务
	ScrobbleEnabled bool `structs:"scrobble_enabled" json:"scrobbleEnabled"`
}

type Players []Player

// PlayerRepository 是播放客户端仓储接口。
type PlayerRepository interface {
	Get(id string) (*Player, error)
	// FindMatch 按「用户 + 客户端 + UA」查找已登记的客户端，
	// 使同一设备的重复请求复用同一条记录而非不断新建
	FindMatch(userId, client, userAgent string) (*Player, error)
	Put(p *Player) error
	CountAll(...QueryOptions) (int64, error)
	// CountByClient 按客户端类型统计数量，供 insights 指标上报使用
	CountByClient(...QueryOptions) (map[string]int64, error)
}
