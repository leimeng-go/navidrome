package model

import (
	"iter"
	"math"
	"sync"
	"time"

	"github.com/gohugoio/hashstructure"
)

// Album 代表一张专辑。它不直接对应磁盘上的实体，而是由所属曲目聚合而成
// （见 MediaFiles.ToAlbum），由扫描器 Phase 3 负责刷新。
type Album struct {
	Annotations `structs:"-" hash:"ignore"` // 用户维度标注：播放次数、评分、收藏

	ID          string `structs:"id" json:"id"`
	LibraryID   int    `structs:"library_id" json:"libraryId"`
	LibraryPath string `structs:"-" json:"libraryPath" hash:"ignore"`
	LibraryName string `structs:"-" json:"libraryName" hash:"ignore"`
	Name        string `structs:"name" json:"name"`
	// EmbedArtPath 指向被选作专辑封面的那个音频文件（内嵌图片来源）
	EmbedArtPath  string `structs:"embed_art_path" json:"-"`
	AlbumArtistID string `structs:"album_artist_id" json:"albumArtistId"` // Deprecated, use Participants
	// AlbumArtist is the display name used for the album artist.
	// AlbumArtist 为展示用专辑艺人名，合辑场景下可能是 "Various Artists"
	AlbumArtist string `structs:"album_artist" json:"albumArtist"`
	// MaxYear/MinYear 为专辑内曲目年份区间（求最小值时忽略表示未知的 0）
	MaxYear int    `structs:"max_year" json:"maxYear"`
	MinYear int    `structs:"min_year" json:"minYear"`
	Date    string `structs:"date" json:"date,omitempty"`
	// MaxOriginalYear/MinOriginalYear 为原始发行年份区间，用于再版专辑排序
	MaxOriginalYear int    `structs:"max_original_year" json:"maxOriginalYear"`
	MinOriginalYear int    `structs:"min_original_year" json:"minOriginalYear"`
	OriginalDate    string `structs:"original_date" json:"originalDate,omitempty"`
	ReleaseDate     string `structs:"release_date" json:"releaseDate,omitempty"`
	Compilation     bool   `structs:"compilation" json:"compilation"` // 是否为合辑
	Comment         string `structs:"comment" json:"comment,omitempty"`
	SongCount       int    `structs:"song_count" json:"songCount"`
	// Duration/Size 为全部曲目的时长与体积之和
	Duration             float32  `structs:"duration" json:"duration"`
	Size                 int64    `structs:"size" json:"size"`
	Discs                Discs    `structs:"discs" json:"discs,omitempty"`
	SortAlbumName        string   `structs:"sort_album_name" json:"sortAlbumName,omitempty"`
	SortAlbumArtistName  string   `structs:"sort_album_artist_name" json:"sortAlbumArtistName,omitempty"`
	OrderAlbumName       string   `structs:"order_album_name" json:"orderAlbumName"` // 归一化后的排序键
	OrderAlbumArtistName string   `structs:"order_album_artist_name" json:"orderAlbumArtistName"`
	CatalogNum           string   `structs:"catalog_num" json:"catalogNum,omitempty"`
	MbzAlbumID           string   `structs:"mbz_album_id" json:"mbzAlbumId,omitempty"` // 以下 Mbz* 为 MusicBrainz 标识
	MbzAlbumArtistID     string   `structs:"mbz_album_artist_id" json:"mbzAlbumArtistId,omitempty"`
	MbzAlbumType         string   `structs:"mbz_album_type" json:"mbzAlbumType,omitempty"`
	MbzAlbumComment      string   `structs:"mbz_album_comment" json:"mbzAlbumComment,omitempty"`
	MbzReleaseGroupID    string   `structs:"mbz_release_group_id" json:"mbzReleaseGroupId,omitempty"`
	FolderIDs            []string `structs:"folder_ids" json:"-" hash:"set"` // All folders that contain media_files for this album
	ExplicitStatus       string   `structs:"explicit_status" json:"explicitStatus"`
	// FolderIDs 标记 hash:"set"，比较时忽略顺序（一张专辑可能分散在多个目录）

	// External metadata fields
	// 以下字段来自外部元数据源（Last.fm / Spotify 等），全部标记 hash:"ignore"：
	// 它们不属于本地文件内容，变化不应触发专辑更新
	Description           string     `structs:"description" json:"description,omitempty" hash:"ignore"`
	SmallImageUrl         string     `structs:"small_image_url" json:"smallImageUrl,omitempty" hash:"ignore"`
	MediumImageUrl        string     `structs:"medium_image_url" json:"mediumImageUrl,omitempty" hash:"ignore"`
	LargeImageUrl         string     `structs:"large_image_url" json:"largeImageUrl,omitempty" hash:"ignore"`
	ExternalUrl           string     `structs:"external_url" json:"externalUrl,omitempty" hash:"ignore"`
	ExternalInfoUpdatedAt *time.Time `structs:"external_info_updated_at" json:"externalInfoUpdatedAt" hash:"ignore"`

	Genre        string       `structs:"genre" json:"genre" hash:"ignore"`               // Easy access to the most common genre
	Genres       Genres       `structs:"-" json:"genres" hash:"ignore"`                  // Easy access to all genres for this album
	Tags         Tags         `structs:"tags" json:"tags,omitempty" hash:"ignore"`       // All imported tags for this album
	Participants Participants `structs:"participants" json:"participants" hash:"ignore"` // All artists that participated in this album

	Missing    bool      `structs:"missing" json:"missing"`                      // If all file of the album ar missing
	ImportedAt time.Time `structs:"imported_at" json:"importedAt" hash:"ignore"` // When this album was imported/updated
	CreatedAt  time.Time `structs:"created_at" json:"createdAt"`                 // Oldest CreatedAt for all songs in this album
	UpdatedAt  time.Time `structs:"updated_at" json:"updatedAt"`                 // Newest UpdatedAt for all songs in this album
	// Missing 仅当专辑内所有曲目均丢失时才为 true
}

// CoverArtID 返回专辑封面的标识。
func (a Album) CoverArtID() ArtworkID {
	return artworkIDFromAlbum(a)
}

// Equals compares two Album structs, ignoring calculated fields
// Equals 比较两张专辑是否等价，忽略标记 hash:"ignore" 的字段（外部元数据、标注等）。
// 时长先向下取整再比较，避免浮点累加误差被误判为"内容有变化"。
func (a Album) Equals(other Album) bool {
	// Normalize float32 values to avoid false negatives
	a.Duration = float32(math.Floor(float64(a.Duration)))
	other.Duration = float32(math.Floor(float64(other.Duration)))

	opts := &hashstructure.HashOptions{
		IgnoreZeroValue: true,
		ZeroNil:         true,
	}
	hash1, _ := hashstructure.Hash(a, opts)
	hash2, _ := hashstructure.Hash(other, opts)

	return hash1 == hash2
}

// AlbumLevelTags contains all Tags marked as `album: true` in the mappings.yml file. They are not
// "first-class citizens" in the Album struct, but are still stored in the album table, in the `tags` column.
// AlbumLevelTags 返回 mappings.yaml 中标记 album: true 的标签集合。
// 这些标签没有独立的结构体字段，统一存放在 album 表的 tags 列中。
// 用 sync.OnceValue 包装，保证只解析一次并可安全并发读取。
var AlbumLevelTags = sync.OnceValue(func() map[TagName]struct{} {
	tags := make(map[TagName]struct{})
	m := TagMappings()
	for t, conf := range m {
		if conf.Album {
			tags[t] = struct{}{}
		}
	}
	return tags
})

// SetTags 按出现频率归组曲目标签，并剔除非专辑级标签，
// 使 Album.Tags 只保留适合在专辑维度展示的信息。
func (a *Album) SetTags(tags TagList) {
	a.Tags = tags.GroupByFrequency()
	for k := range a.Tags {
		if _, ok := AlbumLevelTags()[k]; !ok {
			delete(a.Tags, k)
		}
	}
}

// Discs 是碟号到碟片副标题的映射，用于多碟套装。
type Discs map[int]string

// Add 登记一张碟片及其副标题。
func (d Discs) Add(discNumber int, discSubtitle string) {
	d[discNumber] = discSubtitle
}

// DiscID 唯一标识专辑中的某一张碟片，供 Subsonic API 的碟片级接口使用。
type DiscID struct {
	AlbumID     string `json:"albumId"`
	ReleaseDate string `json:"releaseDate"`
	DiscNumber  int    `json:"discNumber"`
}

type Albums []Album

// AlbumCursor 是专辑的流式游标，用于逐条处理海量数据而不全量载入内存。
type AlbumCursor iter.Seq2[Album, error]

// AlbumRepository 是专辑仓储接口。
type AlbumRepository interface {
	CountAll(...QueryOptions) (int64, error)
	Exists(id string) (bool, error)
	// Put 新增或更新专辑（按 ID upsert）
	Put(*Album) error
	// UpdateExternalInfo 只更新外部元数据字段（简介、图片链接等）
	UpdateExternalInfo(*Album) error
	Get(id string) (*Album, error)
	GetAll(...QueryOptions) (Albums, error)

	// The following methods are used exclusively by the scanner:
	// 以下方法仅供扫描器使用：
	// Touch 把专辑标记为"本次扫描已触及"，Phase 3 只处理被触及的专辑
	Touch(ids ...string) error
	// TouchByMissingFolder 触及包含丢失文件夹的专辑，返回受影响条数
	TouchByMissingFolder() (int64, error)
	// GetTouchedAlbums 返回本次扫描被触及的专辑游标，供 Phase 3 消费
	GetTouchedAlbums(libID int) (AlbumCursor, error)
	// RefreshPlayCounts 由曲目播放次数重算专辑播放次数
	RefreshPlayCounts() (int64, error)
	// CopyAttributes 在专辑 ID 变化时把指定列从旧记录迁移到新记录，
	// 以保留创建时间等不应因重新计算 ID 而丢失的信息
	CopyAttributes(fromID, toID string, columns ...string) error

	AnnotatedRepository
	SearchableRepository[Albums]
}
