package metadata

import (
	"cmp"
	"encoding/json"
	"maps"
	"math"
	"strconv"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/str"
)

// ToMediaFile 把规范化后的元数据映射为 model.MediaFile 实体。
// libID 与 folderID 由扫描器传入，标签本身不含这些归属信息。
//
// 映射顺序有依赖：先填基础字段，再解析参与者，最后计算持久化 ID
// （PID 配置可引用任意字段，故必须最后算）。
func (md Metadata) ToMediaFile(libID int, folderID string) model.MediaFile {
	mf := model.MediaFile{
		LibraryID: libID,
		FolderID:  folderID,
		// 克隆标签：mf.Tags 后续会被删减，不能影响 Metadata 自身
		Tags: maps.Clone(md.tags),
	}

	// Title and Album
	// Order* 是用于排序的规整形式（统一大小写、去音调符号）
	mf.Title = md.mapTrackTitle()
	mf.Album = md.mapAlbumName()
	mf.SortTitle = md.String(model.TagTitleSort)
	mf.SortAlbumName = md.String(model.TagAlbumSort)
	mf.OrderTitle = str.SanitizeFieldForSorting(mf.Title)
	// 专辑名排序时忽略冠词（"The Wall" 归入 W），曲目标题不忽略
	mf.OrderAlbumName = str.SanitizeFieldForSortingNoArticle(mf.Album)
	mf.Compilation = md.Bool(model.TagCompilation)

	// Disc and Track info
	// 只取序号丢弃总数：总数由扫描时统计的实际曲目数更可靠
	mf.TrackNumber, _ = md.NumAndTotal(model.TagTrackNumber)
	mf.DiscNumber, _ = md.NumAndTotal(model.TagDiscNumber)
	mf.DiscSubtitle = md.String(model.TagDiscSubtitle)
	mf.CatalogNum = md.String(model.TagCatalogNumber)
	mf.Comment = md.String(model.TagComment)
	mf.BPM = int(math.Round(md.Float(model.TagBPM)))
	mf.Lyrics = md.mapLyrics()
	mf.ExplicitStatus = md.mapExplicitStatusTag()

	// Dates
	// 三种日期需联合推断，见 mapDates
	date, origDate, relDate := md.mapDates()
	mf.OriginalYear, mf.OriginalDate = origDate.Year(), string(origDate)
	mf.ReleaseYear, mf.ReleaseDate = relDate.Year(), string(relDate)
	mf.Year, mf.Date = date.Year(), string(date)

	// MBIDs
	// MusicBrainz 标识符，用于与外部元数据服务对接
	mf.MbzRecordingID = md.String(model.TagMusicBrainzRecordingID)
	mf.MbzReleaseTrackID = md.String(model.TagMusicBrainzTrackID)
	mf.MbzAlbumID = md.String(model.TagMusicBrainzAlbumID)
	mf.MbzReleaseGroupID = md.String(model.TagMusicBrainzReleaseGroupID)
	mf.MbzAlbumType = md.String(model.TagReleaseType)

	// ReplayGain
	// 音量归一化数据。增益需兼容 Opus 的 R128 格式，见 mapGain
	mf.RGAlbumPeak = md.NullableFloat(model.TagReplayGainAlbumPeak)
	mf.RGAlbumGain = md.mapGain(model.TagReplayGainAlbumGain, model.TagR128AlbumGain)
	mf.RGTrackPeak = md.NullableFloat(model.TagReplayGainTrackPeak)
	mf.RGTrackGain = md.mapGain(model.TagReplayGainTrackGain, model.TagR128TrackGain)

	// General properties
	// 文件与音频流属性，均不来自标签
	mf.HasCoverArt = md.HasPicture()
	mf.Duration = md.Length()
	mf.BitRate = md.AudioProperties().BitRate
	mf.SampleRate = md.AudioProperties().SampleRate
	mf.BitDepth = md.AudioProperties().BitDepth
	mf.Channels = md.AudioProperties().Channels
	mf.Path = md.FilePath()
	mf.Suffix = md.Suffix()
	mf.Size = md.Size()
	mf.BirthTime = md.BirthTime()
	// 用文件修改时间作为 UpdatedAt，使内容未变的文件重扫后时间戳保持稳定
	mf.UpdatedAt = md.ModTime()

	mf.Participants = md.mapParticipants()
	mf.Artist = md.mapDisplayArtist()
	mf.AlbumArtist = md.mapDisplayAlbumArtist(mf)

	// Persistent IDs
	// 持久化 ID 依赖上面已填充的字段，必须最后计算
	mf.PID = md.trackPID(mf)
	mf.AlbumID = md.albumID(mf, conf.Server.PID.Album)

	// BFR These IDs will go away once the UI handle multiple participants.
	// BFR For Legacy Subsonic compatibility, we will set them in the API handlers
	// 单值艺人 ID 是历史遗留字段：取参与者列表首位，
	// 供尚不支持多参与者的 UI 与 Subsonic API 使用
	mf.ArtistID = mf.Participants.First(model.RoleArtist).ID
	mf.AlbumArtistID = mf.Participants.First(model.RoleAlbumArtist).ID

	// BFR What to do with sort/order artist names?
	mf.OrderArtistName = mf.Participants.First(model.RoleArtist).OrderArtistName
	mf.OrderAlbumArtistName = mf.Participants.First(model.RoleAlbumArtist).OrderArtistName
	mf.SortArtistName = mf.Participants.First(model.RoleArtist).SortArtistName
	mf.SortAlbumArtistName = mf.Participants.First(model.RoleAlbumArtist).SortArtistName

	// Don't store tags that are first-class fields (and are not album-level tags) in the
	// MediaFile struct. This is to avoid redundancy in the DB
	//
	// Remove all tags from the main section that are not flagged as album tags
	// 已提升为独立字段的标签不再重复存进 Tags 列，避免冗余。
	// 但标记为 Album 的需保留：专辑聚合时要从曲目 Tags 中汇总
	for tag, conf := range model.TagMainMappings() {
		if !conf.Album {
			delete(mf.Tags, tag)
		}
	}

	return mf
}

// AlbumID 对外暴露专辑 ID 计算，供需按不同 PID 配置重算的场景使用。
func (md Metadata) AlbumID(mf model.MediaFile, pidConf string) string {
	return md.albumID(mf, pidConf)
}

// mapGain 解析音量增益：优先 ReplayGain 标签，缺失时回退到 Opus 的 R128 标签。
//
// R128 值是 Q7.8 定点数，需除以 256 转为 dB。
// 又因 R128 以 -23 LUFS 为参考电平、ReplayGain 以 -18 LUFS 为参考，
// 两者相差 5 dB，故加 5 使数值可统一比较。
func (md Metadata) mapGain(rg, r128 model.TagName) *float64 {
	v := md.Gain(rg)
	if v != nil {
		return v
	}
	r128value := md.String(r128)
	if r128value != "" {
		var v, err = strconv.Atoi(r128value)
		if err != nil {
			return nil
		}
		// Convert Q7.8 to float
		// Q7.8 定点数转浮点
		value := float64(v) / 256.0
		// Adding 5 dB to normalize with ReplayGain level
		// 补偿 R128 与 ReplayGain 参考电平的 5 dB 差异
		value += 5
		return &value
	}
	return nil
}

// mapLyrics 把歌词标签解析为结构化列表并序列化为 JSON 存储。
// 歌词是键值对标签（键为语言代码），单条解析失败只跳过该条，
// 不影响其他语言；空歌词也一并丢弃。
func (md Metadata) mapLyrics() string {
	rawLyrics := md.Pairs(model.TagLyrics)

	lyricList := make(model.LyricList, 0, len(rawLyrics))

	for _, raw := range rawLyrics {
		lang := raw.Key()
		text := raw.Value()

		lyrics, err := model.ToLyrics(lang, text)
		if err != nil {
			log.Warn("Unexpected failure occurred when parsing lyrics", "file", md.filePath, err)
			continue
		}
		if !lyrics.IsEmpty() {
			lyricList = append(lyricList, *lyrics)
		}
	}

	res, err := json.Marshal(lyricList)
	if err != nil {
		log.Warn("Unexpected error occurred when serializing lyrics", "file", md.filePath, err)
		return ""
	}
	return string(res)
}

// mapExplicitStatusTag 把 iTunes 的分级数字映射为 Navidrome 的单字母标识：
// 1/4 表示露骨内容（e），2 表示已净化版本（c），其余为未标注。
func (md Metadata) mapExplicitStatusTag() string {
	switch md.first(model.TagExplicitStatus) {
	case "1", "4":
		return "e"
	case "2":
		return "c"
	default:
		return ""
	}
}

// mapDates 推断三种日期：录制日期、原始（首次）发行日期、本版本发行日期。
//
// 需要特殊处理是因为历史上打标工具的写法不一致：
// 许多工具把专辑的发行日期写进 Date 标签，而把 ReleaseDate 留空。
// 判据是「有 OriginalDate、无 ReleaseDate、且 Date 不早于 OriginalDate」，
// 此时把 Date 的值挪到 ReleaseDate，并让 Date 取 OriginalDate。
//
// 非该情形时，Date 缺失则依次回退到 OriginalDate、ReleaseDate，
// 保证至少有一个可用日期用于年份展示与排序。
func (md Metadata) mapDates() (date Date, originalDate Date, releaseDate Date) {
	// Start with defaults
	date = md.Date(model.TagRecordingDate)
	originalDate = md.Date(model.TagOriginalDate)
	releaseDate = md.Date(model.TagReleaseDate)

	// For some historic reason, taggers have been writing the Release Date of an album to the Date tag,
	// and leave the Release Date tag empty.
	// 识别上述历史写法并纠正字段归属
	legacyMappings := (originalDate != "") &&
		(releaseDate == "") &&
		(date >= originalDate)
	if legacyMappings {
		return originalDate, originalDate, date
	}
	// when there's no Date, first fall back to Original Date, then to Release Date.
	// Date 缺失时依次回退，保证总有可用日期
	date = cmp.Or(date, originalDate, releaseDate)
	return date, originalDate, releaseDate
}
