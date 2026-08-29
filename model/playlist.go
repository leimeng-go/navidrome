package model

import (
	"slices"
	"strconv"
	"time"

	"github.com/navidrome/navidrome/model/criteria"
)

// Playlist 代表一个播放列表，有两种形态：
//   - 普通列表：曲目由用户显式添加，存放在 playlist_tracks 关联表中
//   - 智能列表：Rules 非空，曲目由查询规则动态求值得出（见 criteria 包）
//
// 若 Path 非空，说明该列表由磁盘上的 M3U/NSP 文件导入而来。
type Playlist struct {
	ID        string  `structs:"id" json:"id"`
	Name      string  `structs:"name" json:"name"`
	Comment   string  `structs:"comment" json:"comment"`
	Duration  float32 `structs:"duration" json:"duration"` // 全部曲目时长之和，见 refreshStats
	Size      int64   `structs:"size" json:"size"`
	SongCount int     `structs:"song_count" json:"songCount"`
	OwnerName string  `structs:"-" json:"ownerName"` // 由 JOIN 得到，不入库
	OwnerID   string  `structs:"owner_id" json:"ownerId"`
	Public    bool    `structs:"public" json:"public"` // 为 true 时其他用户也可见
	// Tracks 为按需加载的曲目明细，普通查询不会填充
	Tracks PlaylistTracks `structs:"-" json:"tracks,omitempty"`
	// Path 为来源文件路径（由扫描器 Phase 4 导入时写入），手工创建的列表为空
	Path string `structs:"path" json:"path"`
	// Sync 为 true 时，源文件变更会在下次扫描同步回该列表
	Sync      bool      `structs:"sync" json:"sync"`
	CreatedAt time.Time `structs:"created_at" json:"createdAt"`
	UpdatedAt time.Time `structs:"updated_at" json:"updatedAt"`

	// SmartPlaylist attributes
	// 智能播放列表专有字段
	Rules *criteria.Criteria `structs:"rules" json:"rules"` // 查询规则，会被翻译成 SQL
	// EvaluatedAt 上次求值时间，用于按 SmartPlaylistRefreshDelay 控制重算频率
	EvaluatedAt *time.Time `structs:"evaluated_at" json:"evaluatedAt"`
}

// IsSmartPlaylist 判断是否为智能播放列表（存在有效的规则表达式）。
func (pls Playlist) IsSmartPlaylist() bool {
	return pls.Rules != nil && pls.Rules.Expression != nil
}

// MediaFiles 提取列表中的曲目对象；未加载 Tracks 时返回 nil。
func (pls Playlist) MediaFiles() MediaFiles {
	if len(pls.Tracks) == 0 {
		return nil
	}
	return pls.Tracks.MediaFiles()
}

// refreshStats 依据当前 Tracks 重算曲目数、总时长与总体积。
// 所有会改动 Tracks 的方法都必须调用它，以保持统计字段一致。
func (pls *Playlist) refreshStats() {
	pls.SongCount = len(pls.Tracks)
	pls.Duration = 0
	pls.Size = 0
	for _, t := range pls.Tracks {
		pls.Duration += t.MediaFile.Duration
		pls.Size += t.MediaFile.Size
	}
}

// SetTracks 整体替换列表曲目并刷新统计。
func (pls *Playlist) SetTracks(tracks PlaylistTracks) {
	pls.Tracks = tracks
	pls.refreshStats()
}

// RemoveTracks 按下标（而非 ID）批量移除曲目，因为同一首歌允许在列表中重复出现，
// 只能靠位置精确定位。
func (pls *Playlist) RemoveTracks(idxToRemove []int) {
	var newTracks PlaylistTracks
	for i, t := range pls.Tracks {
		if slices.Contains(idxToRemove, i) {
			continue
		}
		newTracks = append(newTracks, t)
	}
	pls.Tracks = newTracks
	pls.refreshStats()
}

// ToM3U8 exports the playlist to the Extended M3U8 format
// ToM3U8 导出为扩展 M3U8 格式，使用绝对路径以便本地播放器直接打开。
func (pls *Playlist) ToM3U8() string {
	return pls.MediaFiles().ToM3U8(pls.Name, true)
}

// AddMediaFilesByID 按 ID 追加曲目，只填充 ID 占位而不载入完整曲目信息。
// 适用于只需写库的场景；注意此时统计字段（时长/体积）无法正确累加。
func (pls *Playlist) AddMediaFilesByID(mediaFileIds []string) {
	// 曲目 ID 为列表内的顺序位置，从当前长度继续递增
	pos := len(pls.Tracks)
	for _, mfId := range mediaFileIds {
		pos++
		t := PlaylistTrack{
			ID:          strconv.Itoa(pos),
			MediaFileID: mfId,
			MediaFile:   MediaFile{ID: mfId},
			PlaylistID:  pls.ID,
		}
		pls.Tracks = append(pls.Tracks, t)
	}
	pls.refreshStats()
}

// AddMediaFiles 追加完整曲目对象，统计字段可正确累加。
func (pls *Playlist) AddMediaFiles(mfs MediaFiles) {
	pos := len(pls.Tracks)
	for _, mf := range mfs {
		pos++
		t := PlaylistTrack{
			ID:          strconv.Itoa(pos),
			MediaFileID: mf.ID,
			MediaFile:   mf,
			PlaylistID:  pls.ID,
		}
		pls.Tracks = append(pls.Tracks, t)
	}
	pls.refreshStats()
}

// CoverArtID 返回播放列表封面标识（通常取首曲目所属专辑的封面）。
func (pls Playlist) CoverArtID() ArtworkID {
	return artworkIDFromPlaylist(pls)
}

type Playlists []Playlist

// PlaylistRepository 是播放列表仓储接口。
type PlaylistRepository interface {
	ResourceRepository
	CountAll(options ...QueryOptions) (int64, error)
	Exists(id string) (bool, error)
	Put(pls *Playlist) error
	Get(id string) (*Playlist, error)
	// GetWithTracks 连带曲目一起返回。
	// refreshSmartPlaylist 为 true 时会重新求值智能列表规则；
	// includeMissing 控制是否包含已丢失文件的曲目
	GetWithTracks(id string, refreshSmartPlaylist, includeMissing bool) (*Playlist, error)
	GetAll(options ...QueryOptions) (Playlists, error)
	// FindByPath 按来源文件路径查找，供扫描器判断该文件是否已导入
	FindByPath(path string) (*Playlist, error)
	Delete(id string) error
	// Tracks 返回该列表的曲目子仓储，用于增删与重排
	Tracks(playlistId string, refreshSmartPlaylist bool) PlaylistTrackRepository
	// GetPlaylists 反查包含指定曲目的所有播放列表
	GetPlaylists(mediaFileId string) (Playlists, error)
}

// PlaylistTrack 是播放列表中的一个条目。它内嵌 MediaFile，
// 因此既携带曲目全部信息，又带有列表内的位置（ID）。
// 同一首曲目可在同一列表中多次出现，靠 ID（位置）区分。
type PlaylistTrack struct {
	ID          string `json:"id"` // 列表内的顺序位置
	MediaFileID string `json:"mediaFileId"`
	PlaylistID  string `json:"playlistId"`
	MediaFile
}

type PlaylistTracks []PlaylistTrack

// MediaFiles 剥离列表位置信息，只保留曲目对象。
func (plt PlaylistTracks) MediaFiles() MediaFiles {
	mfs := make(MediaFiles, len(plt))
	for i, t := range plt {
		mfs[i] = t.MediaFile
	}
	return mfs
}

// PlaylistTrackRepository 管理某个播放列表内的曲目。
// Add* 系列方法返回实际新增的条目数。
type PlaylistTrackRepository interface {
	ResourceRepository
	GetAll(options ...QueryOptions) (PlaylistTracks, error)
	GetAlbumIDs(options ...QueryOptions) ([]string, error)
	Add(mediaFileIds []string) (int, error)
	// AddAlbums/AddArtists/AddDiscs 为批量添加的便捷入口，
	// 会自动展开为对应的曲目集合
	AddAlbums(albumIds []string) (int, error)
	AddArtists(artistIds []string) (int, error)
	AddDiscs(discs []DiscID) (int, error)
	Delete(id ...string) error
	DeleteAll() error
	// Reorder 把 pos 位置的曲目移动到 newPos
	Reorder(pos int, newPos int) error
}
