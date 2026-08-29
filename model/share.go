package model

import (
	"cmp"
	"strings"
	"time"

	"github.com/navidrome/navidrome/utils/random"
)

// Share 是一个公开分享链接，允许未登录用户访问指定的专辑/曲目/播放列表。
// 由 server/public 路由提供服务，可设置有效期、是否允许下载与转码参数。
type Share struct {
	ID          string `structs:"id" json:"id,omitempty"`
	UserID      string `structs:"user_id" json:"userId,omitempty"`
	Username    string `structs:"-" json:"username,omitempty"` // 由 JOIN 得到，不入库
	Description string `structs:"description" json:"description,omitempty"`
	// Downloadable 为 true 时允许访客下载原始文件
	Downloadable bool `structs:"downloadable" json:"downloadable"`
	// ExpiresAt 为空表示永不过期
	ExpiresAt     *time.Time `structs:"expires_at" json:"expiresAt,omitempty"`
	LastVisitedAt *time.Time `structs:"last_visited_at" json:"lastVisitedAt,omitempty"`
	// ResourceIDs 是逗号分隔的资源 ID 列表，ResourceType 指明类型
	// （album/playlist/artist/media_file）
	ResourceIDs  string `structs:"resource_ids" json:"resourceIds,omitempty"`
	ResourceType string `structs:"resource_type" json:"resourceType,omitempty"`
	// Contents 是资源名称的文字摘要，用于列表展示，避免每次都去查关联实体
	Contents string `structs:"contents" json:"contents,omitempty"`
	// Format/MaxBitRate 强制访客侧的转码参数，可限制分享带宽
	Format     string     `structs:"format" json:"format,omitempty"`
	MaxBitRate int        `structs:"max_bit_rate" json:"maxBitRate,omitempty"`
	VisitCount int        `structs:"visit_count" json:"visitCount,omitempty"`
	CreatedAt  time.Time  `structs:"created_at" json:"createdAt,omitempty"`
	UpdatedAt  time.Time  `structs:"updated_at" json:"updatedAt,omitempty"`
	Tracks     MediaFiles `structs:"-" json:"tracks,omitempty"` // 展开后的曲目，运行时填充
	Albums     Albums     `structs:"-" json:"albums,omitempty"`
	URL        string     `structs:"-" json:"-"` // 运行时拼装的公开访问地址
	ImageURL   string     `structs:"-" json:"-"`
}

// CoverArtID 返回分享的封面标识：
// 专辑/播放列表/艺人类型取首个资源的封面；其他类型（多曲目分享）随机取一首的封面。
func (s Share) CoverArtID() ArtworkID {
	ids := strings.SplitN(s.ResourceIDs, ",", 2)
	if len(ids) == 0 {
		return ArtworkID{}
	}
	switch s.ResourceType {
	case "album":
		return Album{ID: ids[0]}.CoverArtID()
	case "playlist":
		return Playlist{ID: ids[0]}.CoverArtID()
	case "artist":
		return Artist{ID: ids[0]}.CoverArtID()
	}
	rnd := random.Int64N(len(s.Tracks))
	return s.Tracks[rnd].CoverArtID()
}

type Shares []Share

// ToM3U8 exports the share to the Extended M3U8 format.
// ToM3U8 导出为扩展 M3U8 格式。标题优先用描述、缺失时退回 ID；
// 使用相对路径，因为访客无法访问服务端的绝对路径。
func (s Share) ToM3U8() string {
	return s.Tracks.ToM3U8(cmp.Or(s.Description, s.ID), false)
}

// ShareRepository 是分享仓储接口（只读；创建与更新走 core.Share 服务）。
type ShareRepository interface {
	Exists(id string) (bool, error)
	Get(id string) (*Share, error)
	GetAll(options ...QueryOptions) (Shares, error)
	CountAll(options ...QueryOptions) (int64, error)
}
