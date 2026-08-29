package subsonic

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server/public"
	"github.com/navidrome/navidrome/server/subsonic/filter"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
)

// GetMusicFolders 返回当前用户可访问的音乐库列表。
func (api *Router) GetMusicFolders(r *http.Request) (*responses.Subsonic, error) {
	libraries := getUserAccessibleLibraries(r.Context())

	folders := make([]responses.MusicFolder, len(libraries))
	for i, f := range libraries {
		folders[i].Id = int32(f.ID)
		folders[i].Name = f.Name
	}
	response := newResponse()
	response.MusicFolders = &responses.MusicFolders{Folders: folders}
	return response, nil
}

// getArtist 获取艺人索引及其最后修改时间。
// 客户端可用 ifModifiedSince 做增量拉取：上次扫描早于该时间则返回空索引，
// 让客户端沿用本地缓存。
func (api *Router) getArtist(r *http.Request, libIds []int, ifModifiedSince time.Time) (model.ArtistIndexes, int64, error) {
	ctx := r.Context()

	lastScanStr, err := api.ds.Property(ctx).DefaultGet(consts.LastScanStartTimeKey, "")
	if err != nil {
		log.Error(ctx, "Error retrieving last scan start time", err)
		return nil, 0, err
	}
	lastScan := time.Now()
	if lastScanStr != "" {
		lastScan, err = time.Parse(time.RFC3339, lastScanStr)
	}

	var indexes model.ArtistIndexes
	if lastScan.After(ifModifiedSince) {
		indexes, err = api.ds.Artist(ctx).GetIndex(false, libIds, model.RoleAlbumArtist)
		if err != nil {
			log.Error(ctx, "Error retrieving Indexes", err)
			return nil, 0, err
		}
		if len(indexes) == 0 {
			log.Debug(ctx, "No artists found in library", "libId", libIds)
			return nil, 0, newError(responses.ErrorDataNotFound, "Library not found or empty")
		}
	}

	return indexes, lastScan.UnixMilli(), err
}

// getArtistIndex 构造旧版索引响应。
func (api *Router) getArtistIndex(r *http.Request, libIds []int, ifModifiedSince time.Time) (*responses.Indexes, error) {
	indexes, modified, err := api.getArtist(r, libIds, ifModifiedSince)
	if err != nil {
		return nil, err
	}

	res := &responses.Indexes{
		IgnoredArticles: conf.Server.IgnoredArticles,
		LastModified:    modified,
	}

	res.Index = make([]responses.Index, len(indexes))
	for i, idx := range indexes {
		res.Index[i].Name = idx.ID
		res.Index[i].Artists = slice.MapWithArg(idx.Artists, r, toArtist)
	}
	return res, nil
}

// getArtistIndexID3 构造 ID3 风格的索引响应。
func (api *Router) getArtistIndexID3(r *http.Request, libIds []int, ifModifiedSince time.Time) (*responses.Artists, error) {
	indexes, modified, err := api.getArtist(r, libIds, ifModifiedSince)
	if err != nil {
		return nil, err
	}

	res := &responses.Artists{
		IgnoredArticles: conf.Server.IgnoredArticles,
		LastModified:    modified,
	}

	res.Index = make([]responses.IndexID3, len(indexes))
	for i, idx := range indexes {
		res.Index[i].Name = idx.ID
		res.Index[i].Artists = slice.MapWithArg(idx.Artists, r, toArtistID3)
	}
	return res, nil
}

// GetIndexes 返回按首字母分组的艺人索引（旧版目录浏览）。
func (api *Router) GetIndexes(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	musicFolderIds, _ := selectedMusicFolderIds(r, false)
	ifModifiedSince := p.TimeOr("ifModifiedSince", time.Time{})

	res, err := api.getArtistIndex(r, musicFolderIds, ifModifiedSince)
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.Indexes = res
	return response, nil
}

// GetArtists 返回按首字母分组的艺人索引（ID3 浏览）。
func (api *Router) GetArtists(r *http.Request) (*responses.Subsonic, error) {
	musicFolderIds, _ := selectedMusicFolderIds(r, false)

	res, err := api.getArtistIndexID3(r, musicFolderIds, time.Time{})
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.Artist = res
	return response, nil
}

// GetMusicDirectory 返回目录内容。
// 目录在 Navidrome 中并非真实文件夹：艺人展开为专辑列表，专辑展开为曲目列表。
func (api *Router) GetMusicDirectory(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, _ := p.String("id")
	ctx := r.Context()

	entity, err := model.GetEntityByID(ctx, api.ds, id)
	if errors.Is(err, model.ErrNotFound) {
		log.Error(r, "Requested ID not found ", "id", id)
		return nil, newError(responses.ErrorDataNotFound, "Directory not found")
	}
	if err != nil {
		log.Error(err)
		return nil, err
	}

	var dir *responses.Directory

	switch v := entity.(type) {
	case *model.Artist:
		dir, err = api.buildArtistDirectory(ctx, v)
	case *model.Album:
		dir, err = api.buildAlbumDirectory(ctx, v)
	default:
		log.Error(r, "Requested ID of invalid type", "id", id, "entity", v)
		return nil, newError(responses.ErrorDataNotFound, "Directory not found")
	}

	if err != nil {
		log.Error(err)
		return nil, err
	}

	response := newResponse()
	response.Directory = dir
	return response, nil
}

// GetArtist 返回艺人及其专辑列表。
func (api *Router) GetArtist(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, _ := p.String("id")
	ctx := r.Context()

	artist, err := api.ds.Artist(ctx).Get(id)
	if errors.Is(err, model.ErrNotFound) {
		log.Error(ctx, "Requested ArtistID not found ", "id", id)
		return nil, newError(responses.ErrorDataNotFound, "Artist not found")
	}
	if err != nil {
		log.Error(ctx, "Error retrieving artist", "id", id, err)
		return nil, err
	}

	response := newResponse()
	response.ArtistWithAlbumsID3, err = api.buildArtist(r, artist)
	if err != nil {
		log.Error(ctx, "Error retrieving albums by artist", "id", artist.ID, "name", artist.Name, err)
	}
	return response, err
}

// GetAlbum 返回专辑及其曲目列表。
func (api *Router) GetAlbum(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, _ := p.String("id")

	ctx := r.Context()

	album, err := api.ds.Album(ctx).Get(id)
	if errors.Is(err, model.ErrNotFound) {
		log.Error(ctx, "Requested AlbumID not found ", "id", id)
		return nil, newError(responses.ErrorDataNotFound, "Album not found")
	}
	if err != nil {
		log.Error(ctx, "Error retrieving album", "id", id, err)
		return nil, err
	}

	mfs, err := api.ds.MediaFile(ctx).GetAll(filter.SongsByAlbum(id))
	if err != nil {
		log.Error(ctx, "Error retrieving tracks from album", "id", id, "name", album.Name, err)
		return nil, err
	}

	response := newResponse()
	response.AlbumWithSongsID3 = api.buildAlbum(ctx, album, mfs)
	return response, nil
}

// GetAlbumInfo 返回专辑的外部元数据（简介、图片、外链）。
func (api *Router) GetAlbumInfo(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, err := p.String("id")
	ctx := r.Context()

	if err != nil {
		return nil, err
	}

	album, err := api.provider.UpdateAlbumInfo(ctx, id)

	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.AlbumInfo = &responses.AlbumInfo{}
	response.AlbumInfo.Notes = album.Description
	response.AlbumInfo.SmallImageUrl = public.ImageURL(r, album.CoverArtID(), 300)
	response.AlbumInfo.MediumImageUrl = public.ImageURL(r, album.CoverArtID(), 600)
	response.AlbumInfo.LargeImageUrl = public.ImageURL(r, album.CoverArtID(), 1200)

	response.AlbumInfo.LastFmUrl = album.ExternalUrl
	response.AlbumInfo.MusicBrainzID = album.MbzAlbumID

	return response, nil
}

// GetSong 返回单首曲目信息。
func (api *Router) GetSong(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, _ := p.String("id")
	ctx := r.Context()

	mf, err := api.ds.MediaFile(ctx).Get(id)
	if errors.Is(err, model.ErrNotFound) {
		log.Error(r, "Requested MediaFileID not found ", "id", id)
		return nil, newError(responses.ErrorDataNotFound, "Song not found")
	}
	if err != nil {
		log.Error(r, "Error retrieving MediaFile", "id", id, err)
		return nil, err
	}

	response := newResponse()
	child := childFromMediaFile(ctx, *mf)
	response.Song = &child
	return response, nil
}

// GetGenres 返回风格列表。
// 空风格名替换为占位文本，否则客户端会显示成无法点击的空条目。
func (api *Router) GetGenres(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	genres, err := api.ds.Genre(ctx).GetAll(model.QueryOptions{Sort: "song_count, album_count, name desc", Order: "desc"})
	if err != nil {
		log.Error(r, err)
		return nil, err
	}
	for i, g := range genres {
		if g.Name == "" {
			genres[i].Name = "<Empty>"
		}
	}

	response := newResponse()
	response.Genres = toGenres(genres)
	return response, nil
}

// getArtistInfo 拉取艺人的外部元数据与相似艺人。
func (api *Router) getArtistInfo(r *http.Request) (*responses.ArtistInfoBase, *model.Artists, error) {
	ctx := r.Context()
	p := req.Params(r)
	id, err := p.String("id")
	if err != nil {
		return nil, nil, err
	}
	count := p.IntOr("count", 20)
	includeNotPresent := p.BoolOr("includeNotPresent", false)

	artist, err := api.provider.UpdateArtistInfo(ctx, id, count, includeNotPresent)
	if err != nil {
		return nil, nil, err
	}

	base := responses.ArtistInfoBase{}
	base.Biography = artist.Biography
	base.SmallImageUrl = public.ImageURL(r, artist.CoverArtID(), 300)
	base.MediumImageUrl = public.ImageURL(r, artist.CoverArtID(), 600)
	base.LargeImageUrl = public.ImageURL(r, artist.CoverArtID(), 1200)
	base.LastFmUrl = artist.ExternalUrl
	base.MusicBrainzID = artist.MbzArtistID

	return &base, &artist.SimilarArtists, nil
}

// GetArtistInfo 返回艺人信息（旧版结构）。
// 相似艺人若本地库中不存在则以 -1 作为 ID，表示不可跳转。
func (api *Router) GetArtistInfo(r *http.Request) (*responses.Subsonic, error) {
	base, similarArtists, err := api.getArtistInfo(r)
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.ArtistInfo = &responses.ArtistInfo{}
	response.ArtistInfo.ArtistInfoBase = *base

	for _, s := range *similarArtists {
		similar := toArtist(r, s)
		if s.ID == "" {
			similar.Id = "-1"
		}
		response.ArtistInfo.SimilarArtist = append(response.ArtistInfo.SimilarArtist, similar)
	}
	return response, nil
}

// GetArtistInfo2 返回艺人信息（ID3 结构）。
func (api *Router) GetArtistInfo2(r *http.Request) (*responses.Subsonic, error) {
	base, similarArtists, err := api.getArtistInfo(r)
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.ArtistInfo2 = &responses.ArtistInfo2{}
	response.ArtistInfo2.ArtistInfoBase = *base

	for _, s := range *similarArtists {
		similar := toArtistID3(r, s)
		if s.ID == "" {
			similar.Id = "-1"
		}
		response.ArtistInfo2.SimilarArtist = append(response.ArtistInfo2.SimilarArtist, similar)
	}
	return response, nil
}

// GetSimilarSongs 基于艺人电台返回相似曲目。
func (api *Router) GetSimilarSongs(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	p := req.Params(r)
	id, err := p.String("id")
	if err != nil {
		return nil, err
	}
	count := p.IntOr("count", 50)

	songs, err := api.provider.ArtistRadio(ctx, id, count)
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.SimilarSongs = &responses.SimilarSongs{
		Song: slice.MapWithArg(songs, ctx, childFromMediaFile),
	}
	return response, nil
}

// GetSimilarSongs2 与 GetSimilarSongs 内容相同，仅响应字段名不同。
func (api *Router) GetSimilarSongs2(r *http.Request) (*responses.Subsonic, error) {
	res, err := api.GetSimilarSongs(r)
	if err != nil {
		return nil, err
	}

	response := newResponse()
	response.SimilarSongs2 = &responses.SimilarSongs2{
		Song: res.SimilarSongs.Song,
	}
	return response, nil
}

// GetTopSongs 返回艺人热门曲目。
// 未找到不视为错误：外部服务无数据时返回空列表即可。
func (api *Router) GetTopSongs(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	p := req.Params(r)
	artist, err := p.String("artist")
	if err != nil {
		return nil, err
	}
	count := p.IntOr("count", 50)

	songs, err := api.provider.TopSongs(ctx, artist, count)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}

	response := newResponse()
	response.TopSongs = &responses.TopSongs{
		Song: slice.MapWithArg(songs, ctx, childFromMediaFile),
	}
	return response, nil
}

// buildArtistDirectory 把艺人构造成目录，子项为其专辑。
func (api *Router) buildArtistDirectory(ctx context.Context, artist *model.Artist) (*responses.Directory, error) {
	dir := &responses.Directory{}
	dir.Id = artist.ID
	dir.Name = artist.Name
	dir.PlayCount = artist.PlayCount
	if artist.PlayCount > 0 {
		dir.Played = artist.PlayDate
	}
	dir.AlbumCount = getArtistAlbumCount(artist)
	dir.UserRating = int32(artist.Rating)
	if artist.Starred {
		dir.Starred = artist.StarredAt
	}

	albums, err := api.ds.Album(ctx).GetAll(filter.AlbumsByArtistID(artist.ID))
	if err != nil {
		return nil, err
	}

	dir.Child = slice.MapWithArg(albums, ctx, childFromAlbum)
	return dir, nil
}

// buildArtist 构造带专辑列表的艺人响应。
func (api *Router) buildArtist(r *http.Request, artist *model.Artist) (*responses.ArtistWithAlbumsID3, error) {
	ctx := r.Context()
	a := &responses.ArtistWithAlbumsID3{}
	a.ArtistID3 = toArtistID3(r, *artist)

	albums, err := api.ds.Album(ctx).GetAll(filter.AlbumsByArtistID(artist.ID))
	if err != nil {
		return nil, err
	}

	a.Album = slice.MapWithArg(albums, ctx, buildAlbumID3)
	return a, nil
}

// buildAlbumDirectory 把专辑构造成目录，子项为其曲目。
func (api *Router) buildAlbumDirectory(ctx context.Context, album *model.Album) (*responses.Directory, error) {
	dir := &responses.Directory{}
	dir.Id = album.ID
	dir.Name = album.Name
	dir.Parent = album.AlbumArtistID
	dir.PlayCount = album.PlayCount
	if album.PlayCount > 0 {
		dir.Played = album.PlayDate
	}
	dir.UserRating = int32(album.Rating)
	dir.SongCount = int32(album.SongCount)
	dir.CoverArt = album.CoverArtID().String()
	if album.Starred {
		dir.Starred = album.StarredAt
	}

	mfs, err := api.ds.MediaFile(ctx).GetAll(filter.SongsByAlbum(album.ID))
	if err != nil {
		return nil, err
	}

	dir.Child = slice.MapWithArg(mfs, ctx, childFromMediaFile)
	return dir, nil
}

// buildAlbum 构造带曲目列表的专辑响应。
func (api *Router) buildAlbum(ctx context.Context, album *model.Album, mfs model.MediaFiles) *responses.AlbumWithSongsID3 {
	dir := &responses.AlbumWithSongsID3{}
	dir.AlbumID3 = buildAlbumID3(ctx, *album)
	dir.Song = slice.MapWithArg(mfs, ctx, childFromMediaFile)
	return dir
}
