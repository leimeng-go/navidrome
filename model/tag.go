package model

import (
	"cmp"
	"crypto/md5"
	"fmt"
	"slices"
	"strings"

	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/utils/slice"
)

// Tag 是一个标签键值对（如 genre=Rock）。它在数据库中独立成表并被曲目/专辑引用，
// 因此相同的键值对全库只存一份，AlbumCount/SongCount 记录其被引用的次数。
type Tag struct {
	ID         string  `json:"id,omitempty"` // 由 TagName+TagValue 哈希生成，见 tagID
	TagName    TagName `json:"tagName,omitempty"`
	TagValue   string  `json:"tagValue,omitempty"`
	AlbumCount int     `json:"albumCount,omitempty"` // 引用计数，由 UpdateCounts 重算
	SongCount  int     `json:"songCount,omitempty"`
}

type TagList []Tag

// GroupByFrequency 把扁平标签列表按标签名归组，
// 每组内按「出现频次降序、频次相同则按值升序」排列。
// 专辑聚合时用它得出最具代表性的标签值（例如取首个即为最高频流派）；
// 值升序作为次级键是为了保证结果稳定可复现。
func (l TagList) GroupByFrequency() Tags {
	grouped := map[string]map[string]int{}
	values := map[string]string{}
	for _, t := range l {
		if m, ok := grouped[string(t.TagName)]; !ok {
			grouped[string(t.TagName)] = map[string]int{t.ID: 1}
		} else {
			m[t.ID]++
		}
		values[t.ID] = t.TagValue
	}

	tags := Tags{}
	for name, counts := range grouped {
		idList := make([]string, 0, len(counts))
		for tid := range counts {
			idList = append(idList, tid)
		}
		slices.SortFunc(idList, func(a, b string) int {
			return cmp.Or(
				cmp.Compare(counts[b], counts[a]),
				cmp.Compare(values[a], values[b]),
			)
		})
		tags[TagName(name)] = slice.Map(idList, func(id string) string { return values[id] })
	}
	return tags
}

// String 返回 "名=值" 形式，主要用于日志与调试。
func (t Tag) String() string {
	return fmt.Sprintf("%s=%s", t.TagName, t.TagValue)
}

// NewTag 构造标签，标签名统一转小写后再生成 ID，
// 使 "Genre" 与 "genre" 归一为同一条记录。
func NewTag(name TagName, value string) Tag {
	name = name.ToLower()
	hashID := tagID(name, value)
	return Tag{
		ID:       hashID,
		TagName:  name,
		TagValue: value,
	}
}

// tagID 由标签名与值哈希出稳定 ID，使相同键值对在全库共享同一行。
func tagID(name TagName, value string) string {
	return id.NewTagID(string(name), value)
}

// RawTags 是从文件中读出的原始标签，键为文件格式中的原始字段名，
// 尚未经过 mappings.yaml 归一化。
type RawTags map[string][]string

// Tags 是归一化后的标签集合：标签名 → 多个值（同一标签允许多值，如多个流派）。
type Tags map[TagName][]string

// Values 返回指定标签的全部取值。
func (t Tags) Values(name TagName) []string {
	return t[name]
}

// IDs 返回集合中所有标签的 ID，用于建立曲目/专辑与标签表的关联。
func (t Tags) IDs() []string {
	var ids []string
	for name, tag := range t {
		name = name.ToLower()
		for _, v := range tag {
			ids = append(ids, tagID(name, v))
		}
	}
	return ids
}

// Flatten 把指定标签的多个值展开为 Tag 列表。
func (t Tags) Flatten(name TagName) TagList {
	var tags TagList
	for _, v := range t[name] {
		tags = append(tags, NewTag(name, v))
	}
	return tags
}

// FlattenAll 把全部标签展开为扁平的 Tag 列表，供入库或频次统计使用。
func (t Tags) FlattenAll() TagList {
	var tags TagList
	for name, values := range t {
		for _, v := range values {
			tags = append(tags, NewTag(name, v))
		}
	}
	return tags
}

// Sort 对每个标签的取值排序，使输出顺序稳定。
func (t Tags) Sort() {
	for _, values := range t {
		slices.Sort(values)
	}
}

// Hash 计算标签集合的指纹，供 MediaFile.Hash 叠加使用。
// 先对 ID 排序再拼接，确保 map 遍历顺序不影响结果。
func (t Tags) Hash() []byte {
	if len(t) == 0 {
		return nil
	}
	ids := t.IDs()
	slices.Sort(ids)
	sum := md5.New()
	sum.Write([]byte(strings.Join(ids, "|")))
	return sum.Sum(nil)
}

// ToGenres 从标签中提取流派，返回「主流派 + 全部流派」。
// 主流派取第一个值，用于填充兼容旧客户端的 Genre 单值字段。
func (t Tags) ToGenres() (string, Genres) {
	values := t.Values("genre")
	if len(values) == 0 {
		return "", nil
	}
	genres := slice.Map(values, func(g string) Genre {
		t := NewTag("genre", g)
		return Genre{ID: t.ID, Name: g}
	})
	return genres[0].Name, genres
}

// Merge merges the tags from another Tags object into this one, removing any duplicates
// Merge 把另一组标签并入当前集合，重复值会被忽略。
func (t Tags) Merge(tags Tags) {
	for name, values := range tags {
		for _, v := range values {
			t.Add(name, v)
		}
	}
}

// Add 追加一个标签值，已存在则忽略。
// 采用线性查找而非 map 去重，因为单个标签的取值通常只有个位数。
func (t Tags) Add(name TagName, v string) {
	for _, existing := range t[name] {
		if existing == v {
			return
		}
	}
	t[name] = append(t[name], v)
}

// TagRepository 是标签仓储接口。
type TagRepository interface {
	// Add 批量登记标签并关联到指定库
	Add(libraryID int, tags ...Tag) error
	// UpdateCounts 重算所有标签的曲目数与专辑数引用计数，在扫描收尾阶段调用
	UpdateCounts() error
}

// TagName 是归一化后的标签名，取值见下方常量，来源于 mappings.yaml。
type TagName string

// ToLower 返回小写形式的标签名，标签名比较统一走小写。
func (t TagName) ToLower() TagName {
	return TagName(strings.ToLower(string(t)))
}

func (t TagName) String() string {
	return string(t)
}

// Tag names, as defined in the mappings.yaml file
// 标签名常量，与 resources/mappings.yaml 中的定义保持一致。
// mappings.yaml 负责把各种音频格式的原始字段名（ID3、Vorbis、MP4 等）
// 映射到这里的统一名称。
const (
	TagAlbum          TagName = "album"
	TagTitle          TagName = "title"
	TagTrackNumber    TagName = "track"
	TagDiscNumber     TagName = "disc"
	TagTotalTracks    TagName = "tracktotal"
	TagTotalDiscs     TagName = "disctotal"
	TagDiscSubtitle   TagName = "discsubtitle"
	TagSubtitle       TagName = "subtitle"
	TagGenre          TagName = "genre"
	TagMood           TagName = "mood"
	TagComment        TagName = "comment"
	TagAlbumSort      TagName = "albumsort"
	TagAlbumVersion   TagName = "albumversion"
	TagTitleSort      TagName = "titlesort"
	TagCompilation    TagName = "compilation"
	TagGrouping       TagName = "grouping"
	TagLyrics         TagName = "lyrics"
	TagRecordLabel    TagName = "recordlabel"
	TagReleaseType    TagName = "releasetype"
	TagReleaseCountry TagName = "releasecountry"
	TagMedia          TagName = "media"
	TagCatalogNumber  TagName = "catalognumber"
	TagISRC           TagName = "isrc"
	TagBPM            TagName = "bpm"
	TagExplicitStatus TagName = "explicitstatus"

	// Dates and years
	// 日期与年份

	TagOriginalDate  TagName = "originaldate"
	TagReleaseDate   TagName = "releasedate"
	TagRecordingDate TagName = "recordingdate"

	// Artists and roles
	// 艺人与角色。带 Sort 后缀的是排序名；复数形式（如 albumartists）
	// 用于承载多值标签，是单数形式的多艺人版本

	TagAlbumArtist      TagName = "albumartist"
	TagAlbumArtists     TagName = "albumartists"
	TagAlbumArtistSort  TagName = "albumartistsort"
	TagAlbumArtistsSort TagName = "albumartistssort"
	TagTrackArtist      TagName = "artist"
	TagTrackArtists     TagName = "artists"
	TagTrackArtistSort  TagName = "artistsort"
	TagTrackArtistsSort TagName = "artistssort"
	TagComposer         TagName = "composer"
	TagComposerSort     TagName = "composersort"
	TagLyricist         TagName = "lyricist"
	TagLyricistSort     TagName = "lyricistsort"
	TagDirector         TagName = "director"
	TagProducer         TagName = "producer"
	TagEngineer         TagName = "engineer"
	TagMixer            TagName = "mixer"
	TagRemixer          TagName = "remixer"
	TagDJMixer          TagName = "djmixer"
	TagConductor        TagName = "conductor"
	TagArranger         TagName = "arranger"
	TagPerformer        TagName = "performer"

	// ReplayGain
	// ReplayGain 音量归一化数据。R128 是广播级 EBU R128 标准的等价字段

	TagReplayGainAlbumGain TagName = "replaygain_album_gain"
	TagReplayGainAlbumPeak TagName = "replaygain_album_peak"
	TagReplayGainTrackGain TagName = "replaygain_track_gain"
	TagReplayGainTrackPeak TagName = "replaygain_track_peak"
	TagR128AlbumGain       TagName = "r128_album_gain"
	TagR128TrackGain       TagName = "r128_track_gain"

	// MusicBrainz
	// MusicBrainz 标识。这些 ID 是跨库匹配的强证据，
	// 也是持久 ID（PID）生成与丢失文件匹配的首选依据

	TagMusicBrainzArtistID       TagName = "musicbrainz_artistid"
	TagMusicBrainzRecordingID    TagName = "musicbrainz_recordingid"
	TagMusicBrainzTrackID        TagName = "musicbrainz_trackid"
	TagMusicBrainzAlbumArtistID  TagName = "musicbrainz_albumartistid"
	TagMusicBrainzAlbumID        TagName = "musicbrainz_albumid"
	TagMusicBrainzReleaseGroupID TagName = "musicbrainz_releasegroupid"

	TagMusicBrainzComposerID  TagName = "musicbrainz_composerid"
	TagMusicBrainzLyricistID  TagName = "musicbrainz_lyricistid"
	TagMusicBrainzDirectorID  TagName = "musicbrainz_directorid"
	TagMusicBrainzProducerID  TagName = "musicbrainz_producerid"
	TagMusicBrainzEngineerID  TagName = "musicbrainz_engineerid"
	TagMusicBrainzMixerID     TagName = "musicbrainz_mixerid"
	TagMusicBrainzRemixerID   TagName = "musicbrainz_remixerid"
	TagMusicBrainzDJMixerID   TagName = "musicbrainz_djmixerid"
	TagMusicBrainzConductorID TagName = "musicbrainz_conductorid"
	TagMusicBrainzArrangerID  TagName = "musicbrainz_arrangerid"
	TagMusicBrainzPerformerID TagName = "musicbrainz_performerid"
)
