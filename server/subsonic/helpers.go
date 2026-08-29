package subsonic

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/public"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/number"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
)

// newResponse 构造一个成功状态的空响应骨架。
func newResponse() *responses.Subsonic {
	return &responses.Subsonic{
		Status:        responses.StatusOK,
		Version:       Version,
		Type:          consts.AppName,
		ServerVersion: consts.Version,
		OpenSubsonic:  true,
	}
}

// subError 是带 Subsonic 错误码的错误。
type subError struct {
	code     int32
	messages []interface{}
}

// newError 构造 Subsonic 错误，message 支持 fmt 风格的格式化参数。
func newError(code int32, message ...interface{}) error {
	return subError{
		code:     code,
		messages: message,
	}
}

// errSubsonic and Unwrap are used to allow `errors.Is(err, errSubsonic)` to work
var errSubsonic = errors.New("subsonic API error")

func (e subError) Unwrap() error {
	return fmt.Errorf("%w: %d", errSubsonic, e.code)
}

func (e subError) Error() string {
	var msg string
	if len(e.messages) == 0 {
		msg = responses.ErrorMsg(e.code)
	} else {
		msg = fmt.Sprintf(e.messages[0].(string), e.messages[1:]...)
	}
	return msg
}

// getUser 从上下文取当前用户，不存在时返回零值。
func getUser(ctx context.Context) model.User {
	user, ok := request.UserFrom(ctx)
	if ok {
		return user
	}
	return model.User{}
}

// sortName 依据 PreferSortTags 配置在排序标签与推导出的排序名之间取舍。
func sortName(sortName, orderName string) string {
	if conf.Server.PreferSortTags {
		return cmp.Or(
			sortName,
			orderName,
		)
	}
	return orderName
}

func getArtistAlbumCount(a *model.Artist) int32 {
	// If ArtistParticipations are set, then `getArtist` will return albums
	// where the artist is an album artist OR artist. Use the custom stat
	// main credit for this calculation.
	// Otherwise, return just the roles as album artist (precise)
	if conf.Server.Subsonic.ArtistParticipations {
		mainCreditStats := a.Stats[model.RoleMainCredit]
		return int32(mainCreditStats.AlbumCount)
	} else {
		albumStats := a.Stats[model.RoleAlbumArtist]
		return int32(albumStats.AlbumCount)
	}
}

// toArtist 转换为旧版 Artist 响应结构。
func toArtist(r *http.Request, a model.Artist) responses.Artist {
	artist := responses.Artist{
		Id:             a.ID,
		Name:           a.Name,
		UserRating:     int32(a.Rating),
		CoverArt:       a.CoverArtID().String(),
		ArtistImageUrl: public.ImageURL(r, a.CoverArtID(), 600),
	}
	if a.Starred {
		artist.Starred = a.StarredAt
	}
	return artist
}

// toArtistID3 转换为 ID3 风格的艺人响应结构。
func toArtistID3(r *http.Request, a model.Artist) responses.ArtistID3 {
	artist := responses.ArtistID3{
		Id:             a.ID,
		Name:           a.Name,
		AlbumCount:     getArtistAlbumCount(&a),
		CoverArt:       a.CoverArtID().String(),
		ArtistImageUrl: public.ImageURL(r, a.CoverArtID(), 600),
		UserRating:     int32(a.Rating),
	}
	if a.Starred {
		artist.Starred = a.StarredAt
	}
	artist.OpenSubsonicArtistID3 = toOSArtistID3(r.Context(), a)
	return artist
}

// toOSArtistID3 构造 OpenSubsonic 扩展的艺人字段。
// 已知不兼容的老客户端会解析失败，故对其返回 nil 以退回纯 Subsonic 响应。
func toOSArtistID3(ctx context.Context, a model.Artist) *responses.OpenSubsonicArtistID3 {
	player, _ := request.PlayerFrom(ctx)
	if strings.Contains(conf.Server.Subsonic.LegacyClients, player.Client) {
		return nil
	}
	artist := responses.OpenSubsonicArtistID3{
		MusicBrainzId: a.MbzArtistID,
		SortName:      sortName(a.SortArtistName, a.OrderArtistName),
	}
	artist.Roles = slice.Map(a.Roles(), func(r model.Role) string { return r.String() })
	return &artist
}

// toGenres 转换风格列表。
func toGenres(genres model.Genres) *responses.Genres {
	response := make([]responses.Genre, len(genres))
	for i, g := range genres {
		response[i] = responses.Genre{
			Name:       g.Name,
			SongCount:  int32(g.SongCount),
			AlbumCount: int32(g.AlbumCount),
		}
	}
	return &responses.Genres{Genre: response}
}

// toItemGenres 转换为 OpenSubsonic 的风格条目。
func toItemGenres(genres model.Genres) []responses.ItemGenre {
	itemGenres := make([]responses.ItemGenre, len(genres))
	for i, g := range genres {
		itemGenres[i] = responses.ItemGenre{Name: g.Name}
	}
	return itemGenres
}

// getTranscoding 从上下文取当前播放器的转码格式与码率上限。
func getTranscoding(ctx context.Context) (format string, bitRate int) {
	if trc, ok := request.TranscodingFrom(ctx); ok {
		format = trc.TargetFormat
	}
	if plr, ok := request.PlayerFrom(ctx); ok {
		bitRate = plr.MaxBitRate
	}
	return
}

// childFromMediaFile 把媒体文件转换为 Subsonic 的 Child 结构。
// 转码格式与原格式不同时额外声明转码后的类型，供客户端预判。
func childFromMediaFile(ctx context.Context, mf model.MediaFile) responses.Child {
	child := responses.Child{}
	child.Id = mf.ID
	child.Title = mf.FullTitle()
	child.IsDir = false
	child.Parent = mf.AlbumID
	child.Album = mf.Album
	child.Year = int32(mf.Year)
	child.Artist = mf.Artist
	child.Genre = mf.Genre
	child.Track = int32(mf.TrackNumber)
	child.Duration = int32(mf.Duration)
	child.Size = mf.Size
	child.Suffix = mf.Suffix
	child.BitRate = int32(mf.BitRate)
	child.CoverArt = mf.CoverArtID().String()
	child.ContentType = mf.ContentType()
	player, ok := request.PlayerFrom(ctx)
	if ok && player.ReportRealPath {
		child.Path = mf.AbsolutePath()
	} else {
		child.Path = fakePath(mf)
	}
	child.DiscNumber = int32(mf.DiscNumber)
	child.Created = &mf.BirthTime
	child.AlbumId = mf.AlbumID
	child.ArtistId = mf.ArtistID
	child.Type = "music"
	child.PlayCount = mf.PlayCount
	if mf.Starred {
		child.Starred = mf.StarredAt
	}
	child.UserRating = int32(mf.Rating)

	format, _ := getTranscoding(ctx)
	if mf.Suffix != "" && format != "" && mf.Suffix != format {
		child.TranscodedSuffix = format
		child.TranscodedContentType = mime.TypeByExtension("." + format)
	}
	child.BookmarkPosition = mf.BookmarkPosition
	child.OpenSubsonicChild = osChildFromMediaFile(ctx, mf)
	return child
}

// osChildFromMediaFile 构造 OpenSubsonic 扩展的曲目字段。
// 对配置为 LegacyClients 的客户端返回 nil，避免其解析扩展字段出错。
func osChildFromMediaFile(ctx context.Context, mf model.MediaFile) *responses.OpenSubsonicChild {
	player, _ := request.PlayerFrom(ctx)
	if strings.Contains(conf.Server.Subsonic.LegacyClients, player.Client) {
		return nil
	}
	child := responses.OpenSubsonicChild{}
	if mf.PlayCount > 0 {
		child.Played = mf.PlayDate
	}
	child.Comment = mf.Comment
	child.SortName = sortName(mf.SortTitle, mf.OrderTitle)
	child.BPM = int32(mf.BPM)
	child.MediaType = responses.MediaTypeSong
	child.MusicBrainzId = mf.MbzRecordingID
	child.Isrc = mf.Tags.Values(model.TagISRC)
	child.ReplayGain = responses.ReplayGain{
		TrackGain: mf.RGTrackGain,
		AlbumGain: mf.RGAlbumGain,
		TrackPeak: mf.RGTrackPeak,
		AlbumPeak: mf.RGAlbumPeak,
	}
	child.ChannelCount = int32(mf.Channels)
	child.SamplingRate = int32(mf.SampleRate)
	child.BitDepth = int32(mf.BitDepth)
	child.Genres = toItemGenres(mf.Genres)
	child.Moods = mf.Tags.Values(model.TagMood)
	child.DisplayArtist = mf.Artist
	child.Artists = artistRefs(mf.Participants[model.RoleArtist])
	child.DisplayAlbumArtist = mf.AlbumArtist
	child.AlbumArtists = artistRefs(mf.Participants[model.RoleAlbumArtist])
	var contributors []responses.Contributor
	child.DisplayComposer = mf.Participants[model.RoleComposer].Join(consts.ArtistJoiner)
	for role, participants := range mf.Participants {
		if role == model.RoleArtist || role == model.RoleAlbumArtist {
			continue
		}
		for _, participant := range participants {
			contributors = append(contributors, responses.Contributor{
				Role:    role.String(),
				SubRole: participant.SubRole,
				Artist: responses.ArtistID3Ref{
					Id:   participant.ID,
					Name: participant.Name,
				},
			})
		}
	}
	child.Contributors = contributors
	child.ExplicitStatus = mapExplicitStatus(mf.ExplicitStatus)
	return &child
}

// artistRefs 把参与者列表转换为艺人引用。
func artistRefs(participants model.ParticipantList) []responses.ArtistID3Ref {
	return slice.Map(participants, func(p model.Participant) responses.ArtistID3Ref {
		return responses.ArtistID3Ref{
			Id:   p.ID,
			Name: p.Name,
		}
	})
}

// fakePath 依据元数据合成一个虚拟路径。
//
// 不直接暴露真实路径以免泄露服务器目录结构；
// 但许多客户端依赖 path 字段做展示与分组，故需构造一个形似的路径。
// 仅当播放器显式配置 ReportRealPath 时才返回真实路径。
func fakePath(mf model.MediaFile) string {
	builder := strings.Builder{}

	builder.WriteString(fmt.Sprintf("%s/%s/", sanitizeSlashes(mf.AlbumArtist), sanitizeSlashes(mf.Album)))
	if mf.DiscNumber != 0 {
		builder.WriteString(fmt.Sprintf("%02d-", mf.DiscNumber))
	}
	if mf.TrackNumber != 0 {
		builder.WriteString(fmt.Sprintf("%02d - ", mf.TrackNumber))
	}
	builder.WriteString(fmt.Sprintf("%s.%s", sanitizeSlashes(mf.FullTitle()), mf.Suffix))
	return builder.String()
}

// sanitizeSlashes 把名称中的斜杠替换掉，避免破坏虚拟路径的层级结构。
func sanitizeSlashes(target string) string {
	return strings.ReplaceAll(target, "/", "_")
}

// childFromAlbum 把专辑转换为 Child 结构（在目录浏览中专辑表现为一个「目录」）。
func childFromAlbum(ctx context.Context, al model.Album) responses.Child {
	child := responses.Child{}
	child.Id = al.ID
	child.IsDir = true
	child.Title = al.Name
	child.Name = al.Name
	child.Album = al.Name
	child.Artist = al.AlbumArtist
	child.Year = int32(cmp.Or(al.MaxOriginalYear, al.MaxYear))
	child.Genre = al.Genre
	child.CoverArt = al.CoverArtID().String()
	child.Created = &al.CreatedAt
	child.Parent = al.AlbumArtistID
	child.ArtistId = al.AlbumArtistID
	child.Duration = int32(al.Duration)
	child.SongCount = int32(al.SongCount)
	if al.Starred {
		child.Starred = al.StarredAt
	}
	child.PlayCount = al.PlayCount
	child.UserRating = int32(al.Rating)
	child.OpenSubsonicChild = osChildFromAlbum(ctx, al)
	return child
}

// osChildFromAlbum 构造 OpenSubsonic 扩展的专辑字段，老客户端同样跳过。
func osChildFromAlbum(ctx context.Context, al model.Album) *responses.OpenSubsonicChild {
	player, _ := request.PlayerFrom(ctx)
	if strings.Contains(conf.Server.Subsonic.LegacyClients, player.Client) {
		return nil
	}
	child := responses.OpenSubsonicChild{}
	if al.PlayCount > 0 {
		child.Played = al.PlayDate
	}
	child.MediaType = responses.MediaTypeAlbum
	child.MusicBrainzId = al.MbzAlbumID
	child.Genres = toItemGenres(al.Genres)
	child.Moods = al.Tags.Values(model.TagMood)
	child.DisplayArtist = al.AlbumArtist
	child.Artists = artistRefs(al.Participants[model.RoleAlbumArtist])
	child.DisplayAlbumArtist = al.AlbumArtist
	child.AlbumArtists = artistRefs(al.Participants[model.RoleAlbumArtist])
	child.ExplicitStatus = mapExplicitStatus(al.ExplicitStatus)
	child.SortName = sortName(al.SortAlbumName, al.OrderAlbumName)
	return &child
}

// toItemDate converts a string date in the formats 'YYYY-MM-DD', 'YYYY-MM' or 'YYYY' to an OS ItemDate
// toItemDate 解析部分精度的日期：标签中的日期常只有年或年月，需分段处理。
func toItemDate(date string) responses.ItemDate {
	itemDate := responses.ItemDate{}
	if date == "" {
		return itemDate
	}
	parts := strings.Split(date, "-")
	if len(parts) > 2 {
		itemDate.Day = number.ParseInt[int32](parts[2])
	}
	if len(parts) > 1 {
		itemDate.Month = number.ParseInt[int32](parts[1])
	}
	itemDate.Year = number.ParseInt[int32](parts[0])

	return itemDate
}

// buildDiscSubtitles 构造碟片副标题列表并按碟号排序。
// 只有单碟且无标题时视为无意义，返回空。
func buildDiscSubtitles(a model.Album) []responses.DiscTitle {
	if len(a.Discs) == 0 {
		return nil
	}
	var discTitles []responses.DiscTitle
	for num, title := range a.Discs {
		discTitles = append(discTitles, responses.DiscTitle{Disc: int32(num), Title: title})
	}
	if len(discTitles) == 1 && discTitles[0].Title == "" {
		return nil
	}
	sort.Slice(discTitles, func(i, j int) bool {
		return discTitles[i].Disc < discTitles[j].Disc
	})
	return discTitles
}

// buildAlbumID3 转换为 ID3 风格的专辑响应结构。
func buildAlbumID3(ctx context.Context, album model.Album) responses.AlbumID3 {
	dir := responses.AlbumID3{}
	dir.Id = album.ID
	dir.Name = album.Name
	dir.Artist = album.AlbumArtist
	dir.ArtistId = album.AlbumArtistID
	dir.CoverArt = album.CoverArtID().String()
	dir.SongCount = int32(album.SongCount)
	dir.Duration = int32(album.Duration)
	dir.PlayCount = album.PlayCount
	dir.Year = int32(cmp.Or(album.MaxOriginalYear, album.MaxYear))
	dir.Genre = album.Genre
	if !album.CreatedAt.IsZero() {
		dir.Created = &album.CreatedAt
	}
	if album.Starred {
		dir.Starred = album.StarredAt
	}
	dir.OpenSubsonicAlbumID3 = buildOSAlbumID3(ctx, album)
	return dir
}

// buildOSAlbumID3 构造 OpenSubsonic 扩展的专辑 ID3 字段，老客户端跳过。
func buildOSAlbumID3(ctx context.Context, album model.Album) *responses.OpenSubsonicAlbumID3 {
	player, _ := request.PlayerFrom(ctx)
	if strings.Contains(conf.Server.Subsonic.LegacyClients, player.Client) {
		return nil
	}
	dir := responses.OpenSubsonicAlbumID3{}
	if album.PlayCount > 0 {
		dir.Played = album.PlayDate
	}
	dir.UserRating = int32(album.Rating)
	dir.RecordLabels = slice.Map(album.Tags.Values(model.TagRecordLabel), func(s string) responses.RecordLabel {
		return responses.RecordLabel{Name: s}
	})
	dir.MusicBrainzId = album.MbzAlbumID
	dir.Genres = toItemGenres(album.Genres)
	dir.Artists = artistRefs(album.Participants[model.RoleAlbumArtist])
	dir.DisplayArtist = album.AlbumArtist
	dir.ReleaseTypes = album.Tags.Values(model.TagReleaseType)
	dir.Moods = album.Tags.Values(model.TagMood)
	dir.SortName = sortName(album.SortAlbumName, album.OrderAlbumName)
	dir.OriginalReleaseDate = toItemDate(album.OriginalDate)
	dir.ReleaseDate = toItemDate(album.ReleaseDate)
	dir.IsCompilation = album.Compilation
	dir.DiscTitles = buildDiscSubtitles(album)
	dir.ExplicitStatus = mapExplicitStatus(album.ExplicitStatus)
	if len(album.Tags.Values(model.TagAlbumVersion)) > 0 {
		dir.Version = album.Tags.Values(model.TagAlbumVersion)[0]
	}

	return &dir
}

// mapExplicitStatus 把内部的单字母标记转换为 OpenSubsonic 规定的字面量。
func mapExplicitStatus(explicitStatus string) string {
	switch explicitStatus {
	case "c":
		return "clean"
	case "e":
		return "explicit"
	}
	return ""
}

// buildStructuredLyric 构造结构化歌词。歌词自身未带艺人/标题时回退用曲目信息填充。
func buildStructuredLyric(mf *model.MediaFile, lyrics model.Lyrics) responses.StructuredLyric {
	lines := make([]responses.Line, len(lyrics.Line))

	for i, line := range lyrics.Line {
		lines[i] = responses.Line{
			Start: line.Start,
			Value: line.Value,
		}
	}

	structured := responses.StructuredLyric{
		DisplayArtist: lyrics.DisplayArtist,
		DisplayTitle:  lyrics.DisplayTitle,
		Lang:          lyrics.Lang,
		Line:          lines,
		Offset:        lyrics.Offset,
		Synced:        lyrics.Synced,
	}

	if structured.DisplayArtist == "" {
		structured.DisplayArtist = mf.Artist
	}
	if structured.DisplayTitle == "" {
		structured.DisplayTitle = mf.Title
	}

	return structured
}

// buildLyricsList 构造歌词列表响应。
func buildLyricsList(mf *model.MediaFile, lyricsList model.LyricList) *responses.LyricsList {
	lyricList := make(responses.StructuredLyrics, len(lyricsList))

	for i, lyrics := range lyricsList {
		lyricList[i] = buildStructuredLyric(mf, lyrics)
	}

	res := &responses.LyricsList{
		StructuredLyrics: lyricList,
	}
	return res
}

// getUserAccessibleLibraries returns the list of libraries the current user has access to.
// getUserAccessibleLibraries 返回当前用户可访问的音乐库。
func getUserAccessibleLibraries(ctx context.Context) []model.Library {
	user := getUser(ctx)
	return user.Libraries
}

// selectedMusicFolderIds retrieves the music folder IDs from the request parameters.
// If no IDs are provided, it returns all libraries the user has access to (based on the user found in the context).
// If the parameter is required and not present, it returns an error.
// If any of the provided library IDs are invalid (don't exist or user doesn't have access), returns ErrorDataNotFound.
//
// selectedMusicFolderIds 解析请求中的音乐库 ID。
// 逐个校验访问权限，任一不可访问即整体报错，防止越权读取其他库的内容。
// 未指定时默认为用户全部可访问的库。
func selectedMusicFolderIds(r *http.Request, required bool) ([]int, error) {
	p := req.Params(r)
	musicFolderIds, err := p.Ints("musicFolderId")

	// If the parameter is not present, it returns an error if it is required.
	if errors.Is(err, req.ErrMissingParam) && required {
		return nil, err
	}

	// Get user's accessible libraries for validation
	libraries := getUserAccessibleLibraries(r.Context())
	accessibleLibraryIds := slice.Map(libraries, func(lib model.Library) int { return lib.ID })

	if len(musicFolderIds) > 0 {
		// Validate all provided library IDs - if any are invalid, return an error
		for _, id := range musicFolderIds {
			if !slices.Contains(accessibleLibraryIds, id) {
				return nil, newError(responses.ErrorDataNotFound, "Library %d not found or not accessible", id)
			}
		}
		return musicFolderIds, nil
	}

	// If no musicFolderId is provided, return all libraries the user has access to.
	return accessibleLibraryIds, nil
}
