package model

import (
	"maps"
	"slices"
	"time"
)

// Artist 代表一位艺人。艺人记录由曲目标签派生，本身不对应磁盘实体。
// 同一位艺人可以在不同曲目中承担不同角色（见 Stats 字段）。
type Artist struct {
	Annotations `structs:"-"` // 用户维度标注：播放次数、评分、收藏

	ID string `structs:"id" json:"id"`

	// Data based on tags
	// 来自文件标签的数据
	Name            string `structs:"name" json:"name"`
	SortArtistName  string `structs:"sort_artist_name" json:"sortArtistName,omitempty"`   // 标签中显式给出的排序名
	OrderArtistName string `structs:"order_artist_name" json:"orderArtistName,omitempty"` // 系统归一化生成的排序键
	MbzArtistID     string `structs:"mbz_artist_id" json:"mbzArtistId,omitempty"`         // MusicBrainz 艺人 ID

	// Data calculated from files
	// 由文件统计得出，structs:"-" 表示不直接入库，而是扫描后由 RefreshStats 重算
	Stats      map[Role]ArtistStats `structs:"-" json:"stats,omitempty"` // 按角色分别统计
	Size       int64                `structs:"-" json:"size,omitempty"`
	AlbumCount int                  `structs:"-" json:"albumCount,omitempty"`
	SongCount  int                  `structs:"-" json:"songCount,omitempty"`

	// Data imported from external sources
	// 从外部元数据源（Last.fm / Spotify 等）导入的数据
	Biography             string     `structs:"biography" json:"biography,omitempty"`
	SmallImageUrl         string     `structs:"small_image_url" json:"smallImageUrl,omitempty"`
	MediumImageUrl        string     `structs:"medium_image_url" json:"mediumImageUrl,omitempty"`
	LargeImageUrl         string     `structs:"large_image_url" json:"largeImageUrl,omitempty"`
	ExternalUrl           string     `structs:"external_url" json:"externalUrl,omitempty"`
	SimilarArtists        Artists    `structs:"similar_artists"  json:"-"`
	ExternalInfoUpdatedAt *time.Time `structs:"external_info_updated_at" json:"externalInfoUpdatedAt,omitempty"` // 外部信息拉取时间，用于判断缓存是否过期

	// Missing 表示该艺人名下已无可用文件（等待 GC 清理）
	Missing bool `structs:"missing" json:"missing"`

	CreatedAt *time.Time `structs:"created_at" json:"createdAt,omitempty"`
	UpdatedAt *time.Time `structs:"updated_at" json:"updatedAt,omitempty"`
}

// ArtistStats 是艺人在某一个角色下的统计数据。
type ArtistStats struct {
	SongCount  int   `json:"songCount"`
	AlbumCount int   `json:"albumCount"`
	Size       int64 `json:"size"`
}

// ArtistImageUrl 按大图→中图→小图的顺序返回可用的艺人图片地址。
func (a Artist) ArtistImageUrl() string {
	if a.LargeImageUrl != "" {
		return a.LargeImageUrl
	}
	if a.MediumImageUrl != "" {
		return a.MediumImageUrl
	}
	return a.SmallImageUrl
}

// CoverArtID 返回艺人头像的标识。
func (a Artist) CoverArtID() ArtworkID {
	return artworkIDFromArtist(a)
}

// Roles returns the roles this artist has participated in., based on the Stats field
// Roles 基于 Stats 返回该艺人参与过的全部角色（艺人、作曲、指挥等）。
func (a Artist) Roles() []Role {
	return slices.Collect(maps.Keys(a.Stats))
}

type Artists []Artist

// ArtistIndex 是按首字母分组的艺人索引项，ID 为分组键（如 "A"、"#"）。
// 供 Subsonic 的 getIndexes 与 Web UI 的字母导航使用。
type ArtistIndex struct {
	ID      string
	Artists Artists
}
type ArtistIndexes []ArtistIndex

// ArtistRepository 是艺人仓储接口。
type ArtistRepository interface {
	CountAll(options ...QueryOptions) (int64, error)
	Exists(id string) (bool, error)
	// Put 新增或更新艺人；传入 colsToUpdate 可限定只更新指定列
	Put(m *Artist, colsToUpdate ...string) error
	// UpdateExternalInfo 只更新外部元数据字段（简介、图片链接等）
	UpdateExternalInfo(a *Artist) error
	Get(id string) (*Artist, error)
	GetAll(options ...QueryOptions) (Artists, error)
	// GetIndex 返回按首字母分组的艺人索引，可按库与角色过滤
	GetIndex(includeMissing bool, libraryIds []int, roles ...Role) (ArtistIndexes, error)

	// The following methods are used exclusively by the scanner:
	// 以下方法仅供扫描器使用：
	// RefreshPlayCounts 由曲目播放次数重算艺人播放次数
	RefreshPlayCounts() (int64, error)
	// RefreshStats 重算艺人的曲目数/专辑数/体积统计。
	// allArtists 为 false 时只处理本次扫描受影响的艺人，以加快增量扫描
	RefreshStats(allArtists bool) (int64, error)

	AnnotatedRepository
	SearchableRepository[Artists]
}
