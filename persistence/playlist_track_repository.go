package persistence

import (
	"database/sql"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
)

// playlistTrackRepository 是「某个播放列表内曲目」的仓储，
// 由 playlistRepository.Tracks 创建，实例与具体列表绑定。
type playlistTrackRepository struct {
	sqlRepository
	playlistId   string
	playlist     *model.Playlist
	playlistRepo *playlistRepository
}

// dbPlaylistTrack 嵌入 dbMediaFile 复用曲目字段的解析逻辑。
type dbPlaylistTrack struct {
	dbMediaFile
	*model.PlaylistTrack `structs:",flatten"`
}

// PostScan 先解析曲目本体，再拷贝到 PlaylistTrack。
// ID 需覆写为 MediaFileID：查询中 playlist_tracks.id 是列表内位置序号，
// 会盖掉曲目自身的 ID。
func (t *dbPlaylistTrack) PostScan() error {
	if err := t.dbMediaFile.PostScan(); err != nil {
		return err
	}
	t.PlaylistTrack.MediaFile = *t.dbMediaFile.MediaFile
	t.PlaylistTrack.MediaFile.ID = t.MediaFileID
	return nil
}

type dbPlaylistTracks []dbPlaylistTrack

func (t dbPlaylistTracks) toModels() model.PlaylistTracks {
	return slice.Map(t, func(trk dbPlaylistTrack) model.PlaylistTrack {
		return *trk.PlaylistTrack
	})
}

// Tracks 返回指定播放列表的曲目仓储。
// 构造时即加载列表本身，后续的可写性判断需要它；
// 列表不存在或无权访问时返回 nil。
func (r *playlistRepository) Tracks(playlistId string, refreshSmartPlaylist bool) model.PlaylistTrackRepository {
	p := &playlistTrackRepository{}
	p.playlistRepo = r
	p.playlistId = playlistId
	p.ctx = r.ctx
	p.db = r.db
	p.tableName = "playlist_tracks"
	p.registerModel(&model.PlaylistTrack{}, map[string]filterFunc{
		"missing":    booleanFilter,
		"library_id": libraryIdFilter,
	})
	p.setSortMappings(
		map[string]string{
			"id":           "playlist_tracks.id",
			"artist":       "order_artist_name",
			"album_artist": "order_album_artist_name",
			"album":        "order_album_name, album_id, disc_number, track_number, order_artist_name, title",
			"title":        "order_title",
			// To make sure these fields will be whitelisted
			"duration": "duration",
			"year":     "year",
			"bpm":      "bpm",
			"channels": "channels",
		},
		"f") // TODO I don't like this solution, but I won't change it now as it's not the focus of BFR.

	pls, err := r.Get(playlistId)
	if err != nil {
		log.Warn(r.ctx, "Error getting playlist's tracks", "playlistId", playlistId, err)
		return nil
	}
	if refreshSmartPlaylist {
		r.refreshSmartPlaylist(pls)
	}
	p.playlist = pls
	return p
}

// Count 统计列表内曲目数。
func (r *playlistTrackRepository) Count(options ...rest.QueryOptions) (int64, error) {
	query := Select().
		LeftJoin("media_file f on f.id = media_file_id").
		Where(Eq{"playlist_id": r.playlistId})
	return r.count(query, r.parseRestOptions(r.ctx, options...))
}

// Read 读取列表中某个位置的曲目，附带当前用户的标注。
func (r *playlistTrackRepository) Read(id string) (interface{}, error) {
	userID := loggedUser(r.ctx).ID
	sel := r.newSelect().
		LeftJoin("annotation on ("+
			"annotation.item_id = media_file_id"+
			" AND annotation.item_type = 'media_file'"+
			" AND annotation.user_id = '"+userID+"')").
		Columns(
			"coalesce(starred, 0) as starred",
			"coalesce(play_count, 0) as play_count",
			"coalesce(rating, 0) as rating",
			"starred_at",
			"play_date",
			"rated_at",
			"f.*",
			"playlist_tracks.*",
		).
		Join("media_file f on f.id = media_file_id").
		Where(And{Eq{"playlist_id": r.playlistId}, Eq{"playlist_tracks.id": id}})
	var trk dbPlaylistTrack
	err := r.queryOne(sel, &trk)
	return trk.PlaylistTrack, err
}

// GetAll 返回列表内全部曲目。
func (r *playlistTrackRepository) GetAll(options ...model.QueryOptions) (model.PlaylistTracks, error) {
	tracks, err := r.playlistRepo.loadTracks(r.newSelect(options...), r.playlistId)
	if err != nil {
		return nil, err
	}
	return tracks, err
}

// GetAlbumIDs 返回列表涉及的去重专辑 ID，用于批量获取封面等场景。
func (r *playlistTrackRepository) GetAlbumIDs(options ...model.QueryOptions) ([]string, error) {
	query := r.newSelect(options...).Columns("distinct mf.album_id").
		Join("media_file mf on mf.id = media_file_id").
		Where(Eq{"playlist_id": r.playlistId})
	var ids []string
	err := r.queryAllSlice(query, &ids)
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func (r *playlistTrackRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *playlistTrackRepository) EntityName() string {
	return "playlist_tracks"
}

func (r *playlistTrackRepository) NewInstance() interface{} {
	return &model.PlaylistTrack{}
}

// isTracksEditable 判断能否手工增删曲目：
// 需有列表写权限，且不是智能列表——智能列表的内容由规则决定，
// 手工改动会在下次刷新时被覆盖。
func (r *playlistTrackRepository) isTracksEditable() bool {
	return r.playlistRepo.isWritable(r.playlistId) && !r.playlist.IsSmartPlaylist()
}

// Add 把曲目追加到列表末尾。
// 位置序号从当前最大值 +1 开始；列表为空时 max 为 NULL，
// NullInt32 取零值后正好从 1 开始。
func (r *playlistTrackRepository) Add(mediaFileIds []string) (int, error) {
	if !r.isTracksEditable() {
		return 0, rest.ErrPermissionDenied
	}

	if len(mediaFileIds) > 0 {
		log.Debug(r.ctx, "Adding songs to playlist", "playlistId", r.playlistId, "mediaFileIds", mediaFileIds)
	} else {
		return 0, nil
	}

	// Get next pos (ID) in playlist
	sq := r.newSelect().Columns("max(id) as max").Where(Eq{"playlist_id": r.playlistId})
	var res struct{ Max sql.NullInt32 }
	err := r.queryOne(sq, &res)
	if err != nil {
		return 0, err
	}

	return len(mediaFileIds), r.playlistRepo.addTracks(r.playlistId, int(res.Max.Int32+1), mediaFileIds)
}

// addMediaFileIds 按条件查出曲目并追加。
// 排序保证成批添加时保持专辑内的自然曲序。
func (r *playlistTrackRepository) addMediaFileIds(cond Sqlizer) (int, error) {
	sq := Select("id").From("media_file").Where(cond).OrderBy("album_artist, album, release_date, disc_number, track_number")
	var ids []string
	err := r.queryAllSlice(sq, &ids)
	if err != nil {
		log.Error(r.ctx, "Error getting tracks to add to playlist", err)
		return 0, err
	}
	return r.Add(ids)
}

// AddAlbums 追加整张专辑的曲目。
func (r *playlistTrackRepository) AddAlbums(albumIds []string) (int, error) {
	return r.addMediaFileIds(Eq{"album_id": albumIds})
}

// AddArtists 追加指定专辑艺人的全部曲目。
func (r *playlistTrackRepository) AddArtists(artistIds []string) (int, error) {
	return r.addMediaFileIds(Eq{"album_artist_id": artistIds})
}

// AddDiscs 追加指定碟片的曲目。
// 碟片需由「专辑 + 发行日期 + 碟号」三者共同定位，
// 因为同一专辑可能有多个发行版本。
func (r *playlistTrackRepository) AddDiscs(discs []model.DiscID) (int, error) {
	if len(discs) == 0 {
		return 0, nil
	}
	var clauses Or
	for _, d := range discs {
		clauses = append(clauses, And{Eq{"album_id": d.AlbumID}, Eq{"release_date": d.ReleaseDate}, Eq{"disc_number": d.DiscNumber}})
	}
	return r.addMediaFileIds(clauses)
}

// Get ids from all current tracks
// getTracks 按当前顺序返回列表内全部曲目 ID。
func (r *playlistTrackRepository) getTracks() ([]string, error) {
	all := r.newSelect().Columns("media_file_id").Where(Eq{"playlist_id": r.playlistId}).OrderBy("id")
	var ids []string
	err := r.queryAllSlice(all, &ids)
	if err != nil {
		log.Error(r.ctx, "Error querying current tracks from playlist", "playlistId", r.playlistId, err)
		return nil, err
	}
	return ids, nil
}

// Delete 按位置序号删除曲目，删除后重新编号以消除序号空洞。
func (r *playlistTrackRepository) Delete(ids ...string) error {
	if !r.isTracksEditable() {
		return rest.ErrPermissionDenied
	}
	err := r.delete(And{Eq{"playlist_id": r.playlistId}, Eq{"id": ids}})
	if err != nil {
		return err
	}

	return r.playlistRepo.renumber(r.playlistId)
}

// DeleteAll 清空列表内容。
func (r *playlistTrackRepository) DeleteAll() error {
	if !r.isTracksEditable() {
		return rest.ErrPermissionDenied
	}
	err := r.delete(Eq{"playlist_id": r.playlistId})
	if err != nil {
		return err
	}

	return r.playlistRepo.renumber(r.playlistId)
}

// Reorder 把第 pos 首移动到第 newPos 位（位置从 1 计数）。
// 做法是取出完整顺序、在内存中移动后整体重写，
// 比在 SQL 中批量位移序号更直观且不易出错。
func (r *playlistTrackRepository) Reorder(pos int, newPos int) error {
	if !r.isTracksEditable() {
		return rest.ErrPermissionDenied
	}
	ids, err := r.getTracks()
	if err != nil {
		return err
	}
	newOrder := slice.Move(ids, pos-1, newPos-1)
	return r.playlistRepo.updatePlaylist(r.playlistId, newOrder)
}

var _ model.PlaylistTrackRepository = (*playlistTrackRepository)(nil)
