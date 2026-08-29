package subsonic

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
)

// GetPlaylists 返回当前用户可见的所有歌单。
func (api *Router) GetPlaylists(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	allPls, err := api.ds.Playlist(ctx).GetAll(model.QueryOptions{Sort: "name"})
	if err != nil {
		log.Error(r, err)
		return nil, err
	}
	response := newResponse()
	response.Playlists = &responses.Playlists{
		Playlist: slice.Map(allPls, api.buildPlaylist),
	}
	return response, nil
}

// GetPlaylist 返回歌单及其曲目。
func (api *Router) GetPlaylist(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	p := req.Params(r)
	id, err := p.String("id")
	if err != nil {
		return nil, err
	}
	return api.getPlaylist(ctx, id)
}

// getPlaylist 读取歌单内容并构造响应。
func (api *Router) getPlaylist(ctx context.Context, id string) (*responses.Subsonic, error) {
	pls, err := api.ds.Playlist(ctx).GetWithTracks(id, true, false)
	if errors.Is(err, model.ErrNotFound) {
		log.Error(ctx, err.Error(), "id", id)
		return nil, newError(responses.ErrorDataNotFound, "playlist not found")
	}
	if err != nil {
		log.Error(ctx, err)
		return nil, err
	}

	response := newResponse()
	response.Playlist = &responses.PlaylistWithSongs{
		Playlist: api.buildPlaylist(*pls),
	}
	response.Playlist.Entry = slice.MapWithArg(pls.MediaFiles(), ctx, childFromMediaFile)
	return response, nil
}

// create 创建歌单，或以给定曲目整体覆盖已有歌单。
//
// Subsonic 的 createPlaylist 带 playlistId 时语义是「替换全部曲目」，故先清空 Tracks。
// 只有歌单所有者才能覆盖，防止越权修改他人歌单。
func (api *Router) create(ctx context.Context, playlistId, name string, ids []string) (string, error) {
	err := api.ds.WithTxImmediate(func(tx model.DataStore) error {
		owner := getUser(ctx)
		var pls *model.Playlist
		var err error

		if playlistId != "" {
			pls, err = tx.Playlist(ctx).Get(playlistId)
			if err != nil {
				return err
			}
			if owner.ID != pls.OwnerID {
				return model.ErrNotAuthorized
			}
		} else {
			pls = &model.Playlist{Name: name}
			pls.OwnerID = owner.ID
		}
		pls.Tracks = nil
		pls.AddMediaFilesByID(ids)

		err = tx.Playlist(ctx).Put(pls)
		playlistId = pls.ID
		return err
	})
	return playlistId, err
}

// CreatePlaylist 创建歌单（或覆盖已有歌单的曲目）。
func (api *Router) CreatePlaylist(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	p := req.Params(r)
	songIds, _ := p.Strings("songId")
	playlistId, _ := p.String("playlistId")
	name, _ := p.String("name")
	if playlistId == "" && name == "" {
		return nil, errors.New("required parameter name is missing")
	}
	id, err := api.create(ctx, playlistId, name, songIds)
	if err != nil {
		log.Error(r, err)
		return nil, err
	}
	return api.getPlaylist(ctx, id)
}

// DeletePlaylist 删除歌单。
func (api *Router) DeletePlaylist(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	id, err := p.String("id")
	if err != nil {
		return nil, err
	}
	err = api.ds.Playlist(r.Context()).Delete(id)
	if errors.Is(err, model.ErrNotAuthorized) {
		return nil, newError(responses.ErrorAuthorizationFail)
	}
	if err != nil {
		log.Error(r, err)
		return nil, err
	}
	return newResponse(), nil
}

// UpdatePlaylist 增量更新歌单：改名/改注释/改公开状态、追加曲目、按下标移除曲目。
func (api *Router) UpdatePlaylist(r *http.Request) (*responses.Subsonic, error) {
	p := req.Params(r)
	playlistId, err := p.String("playlistId")
	if err != nil {
		return nil, err
	}
	songsToAdd, _ := p.Strings("songIdToAdd")
	songIndexesToRemove, _ := p.Ints("songIndexToRemove")
	var plsName *string
	if s, err := p.String("name"); err == nil {
		plsName = &s
	}
	comment := p.StringPtr("comment")
	public := p.BoolPtr("public")

	log.Debug(r, "Updating playlist", "id", playlistId)
	if plsName != nil {
		log.Trace(r, fmt.Sprintf("-- New Name: '%s'", *plsName))
	}
	log.Trace(r, fmt.Sprintf("-- Adding: '%v'", songsToAdd))
	log.Trace(r, fmt.Sprintf("-- Removing: '%v'", songIndexesToRemove))

	err = api.playlists.Update(r.Context(), playlistId, plsName, comment, public, songsToAdd, songIndexesToRemove)
	if errors.Is(err, model.ErrNotAuthorized) {
		return nil, newError(responses.ErrorAuthorizationFail)
	}
	if err != nil {
		log.Error(r, "Error updating playlist", "id", playlistId, err)
		return nil, err
	}
	return newResponse(), nil
}

// buildPlaylist 转换歌单为响应结构。
// 智能歌单内容随查询实时变化，故修改时间取当前时间，避免客户端缓存到过期内容。
func (api *Router) buildPlaylist(p model.Playlist) responses.Playlist {
	pls := responses.Playlist{}
	pls.Id = p.ID
	pls.Name = p.Name
	pls.Comment = p.Comment
	pls.SongCount = int32(p.SongCount)
	pls.Owner = p.OwnerName
	pls.Duration = int32(p.Duration)
	pls.Public = p.Public
	pls.Created = p.CreatedAt
	pls.CoverArt = p.CoverArtID().String()
	if p.IsSmartPlaylist() {
		pls.Changed = time.Now()
	} else {
		pls.Changed = p.UpdatedAt
	}
	return pls
}
