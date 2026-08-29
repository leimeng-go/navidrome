package model

import (
	"cmp"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"iter"
	"mime"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gohugoio/hashstructure"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/utils"
	"github.com/navidrome/navidrome/utils/slice"
)

// MediaFile 代表音乐库中的一个音频文件（一首曲目），是整个系统最核心的实体。
// 结构体标签含义：
//   - structs: 映射到数据库列名，"-" 表示不入库（运行时计算或来自 JOIN）
//   - json:    Native API 输出的字段名
//   - hash:    是否参与 Hash() 计算，"ignore" 表示排除。被排除的多为标识符与
//     时间戳，因此 Equals() 只比较"内容"变化，这是扫描器判断是否需要更新的依据。
type MediaFile struct {
	Annotations  `structs:"-" hash:"ignore"` // 用户维度标注：播放次数、评分、收藏
	Bookmarkable `structs:"-" hash:"ignore"` // 书签能力：记录播放位置

	ID string `structs:"id"  json:"id" hash:"ignore"`
	// PID 是持久 ID，跨扫描识别同一曲目，用于检测文件移动（见扫描器 Phase 2）
	PID       string `structs:"pid" json:"-" hash:"ignore"`
	LibraryID int    `structs:"library_id" json:"libraryId" hash:"ignore"`
	// LibraryPath 是所属库的绝对根路径，与 Path 拼接得到文件真实位置
	LibraryPath string `structs:"-" json:"libraryPath" hash:"ignore"`
	LibraryName string `structs:"-" json:"libraryName" hash:"ignore"`
	FolderID    string `structs:"folder_id" json:"folderId" hash:"ignore"`
	// Path 是相对于库根目录的路径
	Path     string `structs:"path" json:"path" hash:"ignore"`
	Title    string `structs:"title" json:"title"`
	Album    string `structs:"album" json:"album"`
	ArtistID string `structs:"artist_id" json:"artistId"` // Deprecated: Use Participants instead
	// Artist is the display name used for the artist.
	// Artist 为展示用艺人名，多艺人时由 Scanner.ArtistJoiner 连接
	Artist        string `structs:"artist" json:"artist"`
	AlbumArtistID string `structs:"album_artist_id" json:"albumArtistId"` // Deprecated: Use Participants instead
	// AlbumArtist is the display name used for the album artist.
	// AlbumArtist 为展示用专辑艺人名
	AlbumArtist          string   `structs:"album_artist" json:"albumArtist"`
	AlbumID              string   `structs:"album_id" json:"albumId"`
	HasCoverArt          bool     `structs:"has_cover_art" json:"hasCoverArt"` // 文件是否内嵌封面图
	TrackNumber          int      `structs:"track_number" json:"trackNumber"`
	DiscNumber           int      `structs:"disc_number" json:"discNumber"`
	DiscSubtitle         string   `structs:"disc_subtitle" json:"discSubtitle,omitempty"`
	Year                 int      `structs:"year" json:"year"`
	Date                 string   `structs:"date" json:"date,omitempty"`
	OriginalYear         int      `structs:"original_year" json:"originalYear"` // 原始发行年份，再版专辑据此保持排序
	OriginalDate         string   `structs:"original_date" json:"originalDate,omitempty"`
	ReleaseYear          int      `structs:"release_year" json:"releaseYear"` // 当前版本的发行年份
	ReleaseDate          string   `structs:"release_date" json:"releaseDate,omitempty"`
	Size                 int64    `structs:"size" json:"size"`
	Suffix               string   `structs:"suffix" json:"suffix"` // 不含点的扩展名，用于推导 MIME 与转码决策
	Duration             float32  `structs:"duration" json:"duration"`
	BitRate              int      `structs:"bit_rate" json:"bitRate"`
	SampleRate           int      `structs:"sample_rate" json:"sampleRate"`
	BitDepth             int      `structs:"bit_depth" json:"bitDepth"`
	Channels             int      `structs:"channels" json:"channels"`
	Genre                string   `structs:"genre" json:"genre"`        // 主流派，兼容旧客户端
	Genres               Genres   `structs:"-" json:"genres,omitempty"` // 全部流派，由 Tags 派生
	SortTitle            string   `structs:"sort_title" json:"sortTitle,omitempty"`
	SortAlbumName        string   `structs:"sort_album_name" json:"sortAlbumName,omitempty"`
	SortArtistName       string   `structs:"sort_artist_name" json:"sortArtistName,omitempty"`            // Deprecated: Use Participants instead
	SortAlbumArtistName  string   `structs:"sort_album_artist_name" json:"sortAlbumArtistName,omitempty"` // Deprecated: Use Participants instead
	OrderTitle           string   `structs:"order_title" json:"orderTitle,omitempty"`                     // Order* 为系统归一化生成的排序键
	OrderAlbumName       string   `structs:"order_album_name" json:"orderAlbumName"`
	OrderArtistName      string   `structs:"order_artist_name" json:"orderArtistName"`            // Deprecated: Use Participants instead
	OrderAlbumArtistName string   `structs:"order_album_artist_name" json:"orderAlbumArtistName"` // Deprecated: Use Participants instead
	Compilation          bool     `structs:"compilation" json:"compilation"`                      // 是否属于合辑（Various Artists）
	Comment              string   `structs:"comment" json:"comment,omitempty"`
	Lyrics               string   `structs:"lyrics" json:"lyrics"` // 歌词的 JSON 序列化结果，见 StructuredLyrics
	BPM                  int      `structs:"bpm" json:"bpm,omitempty"`
	ExplicitStatus       string   `structs:"explicit_status" json:"explicitStatus"` // 分级标记："e"=explicit，"c"=clean
	CatalogNum           string   `structs:"catalog_num" json:"catalogNum,omitempty"`
	MbzRecordingID       string   `structs:"mbz_recording_id" json:"mbzRecordingID,omitempty"` // 以下 Mbz* 为 MusicBrainz 标识
	MbzReleaseTrackID    string   `structs:"mbz_release_track_id" json:"mbzReleaseTrackId,omitempty"`
	MbzAlbumID           string   `structs:"mbz_album_id" json:"mbzAlbumId,omitempty"`
	MbzReleaseGroupID    string   `structs:"mbz_release_group_id" json:"mbzReleaseGroupId,omitempty"`
	MbzArtistID          string   `structs:"mbz_artist_id" json:"mbzArtistId,omitempty"`            // Deprecated: Use Participants instead
	MbzAlbumArtistID     string   `structs:"mbz_album_artist_id" json:"mbzAlbumArtistId,omitempty"` // Deprecated: Use Participants instead
	MbzAlbumType         string   `structs:"mbz_album_type" json:"mbzAlbumType,omitempty"`
	MbzAlbumComment      string   `structs:"mbz_album_comment" json:"mbzAlbumComment,omitempty"`
	RGAlbumGain          *float64 `structs:"rg_album_gain" json:"rgAlbumGain"` // 以下 RG* 为 ReplayGain 音量归一化数据
	RGAlbumPeak          *float64 `structs:"rg_album_peak" json:"rgAlbumPeak"`
	RGTrackGain          *float64 `structs:"rg_track_gain" json:"rgTrackGain"`
	RGTrackPeak          *float64 `structs:"rg_track_peak" json:"rgTrackPeak"`

	Tags         Tags         `structs:"tags" json:"tags,omitempty" hash:"ignore"`       // All imported tags from the original file
	Participants Participants `structs:"participants" json:"participants" hash:"ignore"` // All artists that participated in this track
	// Tags：文件原始标签的全量键值；Participants：按角色（艺人/作曲/指挥等）组织的参与者

	Missing   bool      `structs:"missing" json:"missing" hash:"ignore"`      // If the file is not found in the library's FS
	BirthTime time.Time `structs:"birth_time" json:"birthTime" hash:"ignore"` // Time of file creation (ctime)
	CreatedAt time.Time `structs:"created_at" json:"createdAt" hash:"ignore"` // Time this entry was created in the DB
	UpdatedAt time.Time `structs:"updated_at" json:"updatedAt" hash:"ignore"` // Time of file last update (mtime)
	// Missing 是软删除标记：文件在库中已找不到，等待 Phase 2 匹配移动或最终被 GC 清理
}

// FullTitle 返回展示用完整标题。开启 Subsonic.AppendSubtitle 时把副标题以括号追加。
func (mf MediaFile) FullTitle() string {
	if conf.Server.Subsonic.AppendSubtitle && mf.Tags[TagSubtitle] != nil {
		return fmt.Sprintf("%s (%s)", mf.Title, mf.Tags[TagSubtitle][0])
	}
	return mf.Title
}

// ContentType 根据文件后缀推导 HTTP Content-Type，用于流媒体响应头。
func (mf MediaFile) ContentType() string {
	return mime.TypeByExtension("." + mf.Suffix)
}

// CoverArtID 返回该曲目应使用的封面标识：优先内嵌封面，否则回退到专辑封面。
func (mf MediaFile) CoverArtID() ArtworkID {
	// If it has a cover art, return it (if feature is disabled, skip)
	if mf.HasCoverArt && conf.Server.EnableMediaFileCoverArt {
		return artworkIDFromMediaFile(mf)
	}
	// if it does not have a coverArt, fallback to the album cover
	return mf.AlbumCoverArtID()
}

// AlbumCoverArtID 返回所属专辑的封面标识。
func (mf MediaFile) AlbumCoverArtID() ArtworkID {
	return artworkIDFromAlbum(Album{ID: mf.AlbumID})
}

// StructuredLyrics 把存库的 Lyrics JSON 反序列化为结构化歌词列表，
// 支持多语言与带时间轴的同步歌词。
func (mf MediaFile) StructuredLyrics() (LyricList, error) {
	lyrics := LyricList{}
	err := json.Unmarshal([]byte(mf.Lyrics), &lyrics)
	if err != nil {
		return nil, err
	}
	return lyrics, nil
}

// String is mainly used for debugging
// String 主要用于调试输出，直接返回文件路径
func (mf MediaFile) String() string {
	return mf.Path
}

// Hash returns a hash of the MediaFile based on its tags and audio properties
// Hash 基于标签与音频属性计算指纹：先对带 hash 标签的结构体字段求哈希，
// 再叠加 Tags 与 Participants 的哈希，最后统一取 MD5。
// 标记 hash:"ignore" 的字段（ID/路径/时间戳等）不参与，因此指纹只反映内容变化。
func (mf MediaFile) Hash() string {
	opts := &hashstructure.HashOptions{
		IgnoreZeroValue: true,
		ZeroNil:         true,
	}
	hash, _ := hashstructure.Hash(mf, opts)
	sum := md5.New()
	sum.Write([]byte(fmt.Sprintf("%d", hash)))
	sum.Write(mf.Tags.Hash())
	sum.Write(mf.Participants.Hash())
	return fmt.Sprintf("%x", sum.Sum(nil))
}

// Equals compares two MediaFiles by their hash. It does not consider the ID, PID, Path and other identifier fields.
// Check the structure for the fields that are marked with `hash:"ignore"`.
// Equals 通过 Hash 比较两个 MediaFile 的内容是否等价，忽略 ID/PID/Path 等标识字段。
// 扫描器用它判断已入库的记录是否需要更新。
func (mf MediaFile) Equals(other MediaFile) bool {
	return mf.Hash() == other.Hash()
}

// IsEquivalent compares two MediaFiles by path only. Used for matching missing tracks.
// IsEquivalent 只比较去掉扩展名后的文件名，用于 Phase 2 中为丢失曲目寻找候选匹配
// （例如同一首歌从 mp3 换成了 flac）。
func (mf MediaFile) IsEquivalent(other MediaFile) bool {
	return utils.BaseName(mf.Path) == utils.BaseName(other.Path)
}

// AbsolutePath 返回文件在磁盘上的绝对路径（库根路径 + 相对路径）。
func (mf MediaFile) AbsolutePath() string {
	return filepath.Join(mf.LibraryPath, mf.Path)
}

// MediaFiles 是曲目集合，提供聚合为专辑、导出播放列表等批量能力。
type MediaFiles []MediaFile

// ToAlbum creates an Album object based on the attributes of this MediaFiles collection.
// It assumes all mediafiles have the same Album (same ID), or else results are unpredictable.
// ToAlbum 把一组同属一张专辑的曲目聚合成 Album 对象，是扫描器 Phase 3 刷新专辑的核心。
// 聚合分三类：
//  1. 直接取值——假定专辑内所有曲目一致的字段（专辑名、专辑艺人等），循环中反复覆盖
//  2. 累加/求极值——时长、体积、年份区间
//  3. 一致性归并——allOrNothing（全部相同才保留）、MostFrequent（取众数）
//
// 调用方必须保证入参属于同一 AlbumID，否则结果不可预期。
func (mfs MediaFiles) ToAlbum() Album {
	if len(mfs) == 0 {
		return Album{}
	}
	a := Album{SongCount: len(mfs), Tags: make(Tags), Participants: make(Participants), Discs: Discs{1: ""}}

	// Sorting the mediafiles ensure the results will be consistent
	// 先排序，保证多次聚合（如封面选取、众数计算）结果稳定可复现
	slices.SortFunc(mfs, func(a, b MediaFile) int { return cmp.Compare(a.Path, b.Path) })

	// 收集各曲目的取值，循环结束后统一做一致性归并
	mbzAlbumIds := make([]string, 0, len(mfs))
	mbzReleaseGroupIds := make([]string, 0, len(mfs))
	comments := make([]string, 0, len(mfs))
	years := make([]int, 0, len(mfs))
	dates := make([]string, 0, len(mfs))
	originalYears := make([]int, 0, len(mfs))
	originalDates := make([]string, 0, len(mfs))
	releaseDates := make([]string, 0, len(mfs))
	tags := make(TagList, 0, len(mfs[0].Tags)*len(mfs))

	// Missing 初始为 true，循环中与每首曲目做与运算：只有全部曲目都丢失，专辑才算丢失
	a.Missing = true
	embedArtPath := ""
	embedArtDisc := 0
	for _, m := range mfs {
		// We assume these attributes are all the same for all songs in an album
		a.ID = m.AlbumID
		a.LibraryID = m.LibraryID
		a.Name = m.Album
		a.AlbumArtist = m.AlbumArtist
		a.AlbumArtistID = m.AlbumArtistID
		a.SortAlbumName = m.SortAlbumName
		a.SortAlbumArtistName = m.SortAlbumArtistName
		a.OrderAlbumName = m.OrderAlbumName
		a.OrderAlbumArtistName = m.OrderAlbumArtistName
		a.MbzAlbumArtistID = m.MbzAlbumArtistID
		a.MbzAlbumType = m.MbzAlbumType
		a.MbzAlbumComment = m.MbzAlbumComment
		a.CatalogNum = m.CatalogNum
		a.Compilation = a.Compilation || m.Compilation

		// Calculated attributes based on aggregations
		a.Duration += m.Duration
		a.Size += m.Size
		years = append(years, m.Year)
		dates = append(dates, m.Date)
		originalYears = append(originalYears, m.OriginalYear)
		originalDates = append(originalDates, m.OriginalDate)
		releaseDates = append(releaseDates, m.ReleaseDate)
		comments = append(comments, m.Comment)
		mbzAlbumIds = append(mbzAlbumIds, m.MbzAlbumID)
		mbzReleaseGroupIds = append(mbzReleaseGroupIds, m.MbzReleaseGroupID)
		if m.DiscNumber > 0 {
			a.Discs.Add(m.DiscNumber, m.DiscSubtitle)
		}
		tags = append(tags, m.Tags.FlattenAll()...)
		a.Participants.Merge(m.Participants)

		// Find the MediaFile with cover art and the lowest disc number to use for album cover
		// 选取专辑封面：优先碟号最小、其次路径字典序最小的内嵌封面
		embedArtPath, embedArtDisc = firstArtPath(embedArtPath, embedArtDisc, m)

		// 分级标记按严格程度取最高：只要有一首 explicit，整张即 explicit
		if m.ExplicitStatus == "c" && a.ExplicitStatus != "e" {
			a.ExplicitStatus = "c"
		} else if m.ExplicitStatus == "e" {
			a.ExplicitStatus = "e"
		}

		// UpdatedAt 取最新，CreatedAt 取最早，使专辑时间戳覆盖全部曲目的时间跨度
		a.UpdatedAt = newer(a.UpdatedAt, m.UpdatedAt)
		a.CreatedAt = older(a.CreatedAt, m.BirthTime)
		a.Missing = a.Missing && m.Missing
	}

	a.EmbedArtPath = embedArtPath
	a.SetTags(tags)
	a.FolderIDs = slice.Unique(slice.Map(mfs, func(m MediaFile) string { return m.FolderID }))
	// 日期类字段要求全专辑一致，否则留空，避免展示误导性信息
	a.Date, _ = allOrNothing(dates)
	a.OriginalDate, _ = allOrNothing(originalDates)
	a.ReleaseDate, _ = allOrNothing(releaseDates)
	a.MinYear, a.MaxYear = minMax(years)
	a.MinOriginalYear, a.MaxOriginalYear = minMax(originalYears)
	a.Comment, _ = allOrNothing(comments)
	// MusicBrainz ID 允许个别曲目标签有误，因此取众数而非要求完全一致
	a.MbzAlbumID = slice.MostFrequent(mbzAlbumIds)
	a.MbzReleaseGroupID = slice.MostFrequent(mbzReleaseGroupIds)
	fixAlbumArtist(&a)

	return a
}

// allOrNothing 仅当所有元素相同时返回该值，否则返回空串。
// 第二个返回值是去重后的元素个数，供调用方判断分歧程度。
func allOrNothing(items []string) (string, int) {
	if len(items) == 0 {
		return "", 0
	}
	items = slice.Unique(items)
	if len(items) != 1 {
		return "", len(items)
	}
	return items[0], 1
}

// minMax 返回最小值与最大值。特殊之处在于求最小值时跳过 0，
// 因为年份 0 表示"未知"而非真实年份，不应拉低专辑的起始年份。
func minMax(items []int) (int, int) {
	var mn, mx = items[0], items[0]
	for _, value := range items {
		mx = max(mx, value)
		if mn == 0 {
			mn = value
		} else if value > 0 {
			mn = min(mn, value)
		}
	}
	return mn, mx
}

// newer 返回两个时间中较晚的一个。
func newer(t1, t2 time.Time) time.Time {
	if t1.After(t2) {
		return t1
	}
	return t2
}

// older 返回两个时间中较早的一个；t1 为零值时视为"尚未赋值"，直接取 t2。
func older(t1, t2 time.Time) time.Time {
	if t1.IsZero() {
		return t2
	}
	if t1.After(t2) {
		return t2
	}
	return t1
}

// fixAlbumArtist sets the AlbumArtist to "Various Artists" if the album has more than one artist
// or if it is a compilation
// fixAlbumArtist 修正专辑艺人：
//   - 非合辑且缺失专辑艺人时，用第一位参与艺人补上
//   - 合辑且存在多个不同专辑艺人时，统一归为 "Various Artists"
func fixAlbumArtist(a *Album) {
	if !a.Compilation {
		if a.AlbumArtistID == "" {
			artist := a.Participants.First(RoleArtist)
			a.AlbumArtistID = artist.ID
			a.AlbumArtist = artist.Name
		}
		return
	}
	albumArtistIds := slice.Map(a.Participants[RoleAlbumArtist], func(p Participant) string { return p.ID })
	if len(slice.Unique(albumArtistIds)) > 1 {
		a.AlbumArtist = consts.VariousArtists
		a.AlbumArtistID = consts.VariousArtistsID
	}
}

// firstArtPath determines which media file path should be used for album artwork
// based on disc number (preferring lower disc numbers) and path (for consistency)
// firstArtPath 在遍历过程中逐步选出用作专辑封面的文件：
// 优先碟号更小者（多碟套装取第一碟），碟号相同时取路径字典序更小者以保证结果稳定。
func firstArtPath(currentPath string, currentDisc int, m MediaFile) (string, int) {
	if !m.HasCoverArt {
		return currentPath, currentDisc
	}

	// If current has no disc number (currentDisc == 0) or new file has lower disc number
	if currentDisc == 0 || (m.DiscNumber < currentDisc && m.DiscNumber > 0) {
		return m.Path, m.DiscNumber
	}

	// If disc numbers are equal, use path for ordering
	if m.DiscNumber == currentDisc {
		if m.Path < currentPath || currentPath == "" {
			return m.Path, m.DiscNumber
		}
	}

	return currentPath, currentDisc
}

// ToM3U8 exports the playlist to the Extended M3U8 format, as specified in
// https://docs.fileformat.com/audio/m3u/#extended-m3u
// ToM3U8 把曲目集合导出为扩展 M3U8 播放列表。
// absolutePaths 为 true 时写入磁盘绝对路径（供本地播放器使用），
// 否则写入库内相对路径（供分发或跨机器使用）。
func (mfs MediaFiles) ToM3U8(title string, absolutePaths bool) string {
	buf := strings.Builder{}
	buf.WriteString("#EXTM3U\n")
	buf.WriteString(fmt.Sprintf("#PLAYLIST:%s\n", title))
	for _, t := range mfs {
		buf.WriteString(fmt.Sprintf("#EXTINF:%.f,%s - %s\n", t.Duration, t.Artist, t.Title))
		if absolutePaths {
			buf.WriteString(t.AbsolutePath() + "\n")
		} else {
			buf.WriteString(t.Path + "\n")
		}
	}
	return buf.String()
}

// MediaFileCursor 是曲目的流式游标，用于逐条处理海量数据而不全量载入内存。
type MediaFileCursor iter.Seq2[MediaFile, error]

// MediaFileRepository 是曲目仓储接口。
type MediaFileRepository interface {
	CountAll(options ...QueryOptions) (int64, error)
	Exists(id string) (bool, error)
	// Put 新增或更新曲目（按 ID upsert）
	Put(m *MediaFile) error
	Get(id string) (*MediaFile, error)
	// GetWithParticipants 额外载入参与艺人信息，代价高于 Get
	GetWithParticipants(id string) (*MediaFile, error)
	GetAll(options ...QueryOptions) (MediaFiles, error)
	// GetCursor 返回流式游标，适合大结果集
	GetCursor(options ...QueryOptions) (MediaFileCursor, error)
	Delete(id string) error
	// DeleteMissing 按 ID 删除已标记丢失的曲目
	DeleteMissing(ids []string) error
	// DeleteAllMissing 清空所有丢失曲目，返回删除条数
	DeleteAllMissing() (int64, error)
	FindByPaths(paths []string) (MediaFiles, error)

	// The following methods are used exclusively by the scanner:
	// 以下方法仅供扫描器使用：
	// MarkMissing 批量设置/取消丢失标记
	MarkMissing(bool, ...*MediaFile) error
	// MarkMissingByFolder 按文件夹批量标记（整个目录消失时使用）
	MarkMissingByFolder(missing bool, folderIDs ...string) error
	// GetMissingAndMatching 返回丢失曲目及其潜在匹配项，供 Phase 2 做移动检测
	GetMissingAndMatching(libId int) (MediaFileCursor, error)
	// FindRecentFilesByMBZTrackID 按 MusicBrainz 音轨 ID 查找近期新增文件（移动匹配的强证据）
	FindRecentFilesByMBZTrackID(missing MediaFile, since time.Time) (MediaFiles, error)
	// FindRecentFilesByProperties 按音频属性（时长、体积等）查找近期新增文件（弱证据兜底）
	FindRecentFilesByProperties(missing MediaFile, since time.Time) (MediaFiles, error)

	AnnotatedRepository
	BookmarkableRepository
	SearchableRepository[MediaFiles]
}
