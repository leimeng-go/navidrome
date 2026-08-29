package persistence

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/criteria"
	"github.com/pocketbase/dbx"
)

// playlistRepository 是播放列表仓储，同时处理普通列表与智能列表。
// 二者共用一张表，靠 rules 列是否为空区分。
type playlistRepository struct {
	sqlRepository
}

// dbPlaylist 是 model.Playlist 的数据库映射层。
// Rules 用 NullString：普通列表该列为 NULL。
type dbPlaylist struct {
	model.Playlist `structs:",flatten"`
	Rules          sql.NullString `structs:"-"`
}

// PostScan 解析智能列表的规则 JSON。
func (p *dbPlaylist) PostScan() error {
	if p.Rules.String != "" {
		return json.Unmarshal([]byte(p.Rules.String), &p.Playlist.Rules)
	}
	return nil
}

// PostMapArgs 写库前处理规则列：
// 智能列表序列化规则，普通列表则删除该列以保持 NULL——
// 写空字符串会让 IsSmartPlaylist 之类的判断失准。
func (p dbPlaylist) PostMapArgs(args map[string]any) error {
	var err error
	if p.Playlist.IsSmartPlaylist() {
		args["rules"], err = json.Marshal(p.Playlist.Rules)
		if err != nil {
			return fmt.Errorf("invalid criteria expression: %w", err)
		}
		return nil
	}
	delete(args, "rules")
	return nil
}

// NewPlaylistRepository 创建播放列表仓储。
func NewPlaylistRepository(ctx context.Context, db dbx.Builder) model.PlaylistRepository {
	r := &playlistRepository{}
	r.ctx = ctx
	r.db = db
	r.registerModel(&model.Playlist{}, map[string]filterFunc{
		"q":     playlistFilter,
		"smart": smartPlaylistFilter,
	})
	r.setSortMappings(map[string]string{
		"owner_name": "owner_name",
	})
	return r
}

// playlistFilter 是通用搜索：名称或备注命中即可。
func playlistFilter(_ string, value interface{}) Sqlizer {
	return Or{
		substringFilter("playlist.name", value),
		substringFilter("playlist.comment", value),
	}
}

// smartPlaylistFilter 筛选普通（非智能）列表，即规则为空的列表。
// 空串与 NULL 都要判断：历史数据中两种形式都存在。
func smartPlaylistFilter(string, interface{}) Sqlizer {
	return Or{
		Eq{"rules": ""},
		Eq{"rules": nil},
	}
}

// userFilter 限定可见范围：管理员可见全部，普通用户只能看到公开列表与自己的列表。
// 管理员返回空的 And{}（恒真），使调用方无需分支处理。
func (r *playlistRepository) userFilter() Sqlizer {
	user := loggedUser(r.ctx)
	if user.IsAdmin {
		return And{}
	}
	return Or{
		Eq{"public": true},
		Eq{"owner_id": user.ID},
	}
}

// CountAll 统计当前用户可见的播放列表数。
func (r *playlistRepository) CountAll(options ...model.QueryOptions) (int64, error) {
	sq := Select().Where(r.userFilter())
	return r.count(sq, options...)
}

// Exists 判断列表存在且当前用户可见。
func (r *playlistRepository) Exists(id string) (bool, error) {
	return r.exists(And{Eq{"id": id}, r.userFilter()})
}

// Delete 删除播放列表。
// 非管理员只能删除自己拥有的列表——可见（公开）不等于可删。
func (r *playlistRepository) Delete(id string) error {
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		pls, err := r.Get(id)
		if err != nil {
			return err
		}
		if pls.OwnerID != usr.ID {
			return rest.ErrPermissionDenied
		}
	}
	return r.delete(And{Eq{"id": id}, r.userFilter()})
}

// Put 保存播放列表。
//
// 更新既有列表前先校验可见性，防止越权修改他人的私有列表。
// 智能列表不在此处刷新曲目：求值可能很慢并长时间持有写锁，
// 会阻塞正在进行的扫描，故延迟到读取时再刷新。
func (r *playlistRepository) Put(p *model.Playlist) error {
	pls := dbPlaylist{Playlist: *p}
	if pls.ID == "" {
		pls.CreatedAt = time.Now()
	} else {
		ok, err := r.Exists(pls.ID)
		if err != nil {
			return err
		}
		if !ok {
			return model.ErrNotAuthorized
		}
	}
	pls.UpdatedAt = time.Now()

	id, err := r.put(pls.ID, pls)
	if err != nil {
		return err
	}
	p.ID = id

	if p.IsSmartPlaylist() {
		// Do not update tracks at this point, as it may take a long time and lock the DB, breaking the scan process
		//r.refreshSmartPlaylist(p)
		return nil
	}
	// Only update tracks if they were specified
	// 未传曲目时只刷新统计，避免把「仅改名」误当成「清空列表」
	if len(pls.Tracks) > 0 {
		return r.updateTracks(id, p.MediaFiles())
	}
	return r.refreshCounters(&pls.Playlist)
}

// Get 按 ID 读取播放列表（不含曲目）。
func (r *playlistRepository) Get(id string) (*model.Playlist, error) {
	return r.findBy(And{Eq{"playlist.id": id}, r.userFilter()})
}

// GetWithTracks 读取播放列表及其曲目。
// refreshSmartPlaylist 控制是否重新求值智能列表规则；
// 过滤 missing 曲目，避免播放时遇到已不存在的文件。
func (r *playlistRepository) GetWithTracks(id string, refreshSmartPlaylist, includeMissing bool) (*model.Playlist, error) {
	pls, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	if refreshSmartPlaylist {
		r.refreshSmartPlaylist(pls)
	}
	tracks, err := r.loadTracks(Select().From("playlist_tracks").
		Where(Eq{"missing": false}).
		OrderBy("playlist_tracks.id"), id)
	if err != nil {
		log.Error(r.ctx, "Error loading playlist tracks ", "playlist", pls.Name, "id", pls.ID, err)
		return nil, err
	}
	pls.SetTracks(tracks)
	return pls, nil
}

func (r *playlistRepository) FindByPath(path string) (*model.Playlist, error) {
	return r.findBy(Eq{"path": path})
}

func (r *playlistRepository) findBy(sql Sqlizer) (*model.Playlist, error) {
	sel := r.selectPlaylist().Where(sql)
	var pls []dbPlaylist
	err := r.queryAll(sel, &pls)
	if err != nil {
		return nil, err
	}
	if len(pls) == 0 {
		return nil, model.ErrNotFound
	}

	return &pls[0].Playlist, nil
}

// GetAll 查询当前用户可见的播放列表。
func (r *playlistRepository) GetAll(options ...model.QueryOptions) (model.Playlists, error) {
	sel := r.selectPlaylist(options...).Where(r.userFilter())
	var res []dbPlaylist
	err := r.queryAll(sel, &res)
	if err != nil {
		return nil, err
	}
	playlists := make(model.Playlists, len(res))
	for i, p := range res {
		playlists[i] = p.Playlist
	}
	return playlists, err
}

// GetPlaylists 返回包含指定曲目的所有播放列表。
// 曲目不在任何列表中时返回空切片而非错误，方便调用方直接遍历。
func (r *playlistRepository) GetPlaylists(mediaFileId string) (model.Playlists, error) {
	sel := r.selectPlaylist(model.QueryOptions{Sort: "name"}).
		Join("playlist_tracks on playlist.id = playlist_tracks.playlist_id").
		Where(And{Eq{"playlist_tracks.media_file_id": mediaFileId}, r.userFilter()})
	var res []dbPlaylist
	err := r.queryAll(sel, &res)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return model.Playlists{}, nil
		}
		return nil, err
	}
	playlists := make(model.Playlists, len(res))
	for i, p := range res {
		playlists[i] = p.Playlist
	}
	return playlists, nil
}

// selectPlaylist 构建标准查询，附带关联出所有者用户名以便前端展示。
func (r *playlistRepository) selectPlaylist(options ...model.QueryOptions) SelectBuilder {
	return r.newSelect(options...).Join("user on user.id = owner_id").
		Columns(r.tableName+".*", "user.user_name as owner_name")
}

// refreshSmartPlaylist 重新求值智能列表并回填曲目，返回是否实际执行了刷新。
//
// 三种情况跳过刷新：非智能列表、距上次求值未超过配置的间隔（避免频繁重算）、
// 以及列表属于其他用户——规则求值依赖当前用户的标注（评分、播放次数等），
// 用他人身份求值会写入错误结果。
//
// 刷新过程：先清空旧曲目，递归刷新被引用的子列表（规则中可引用其他播放列表），
// 然后用 INSERT ... SELECT 一次性写入匹配曲目，
// row_number() 按规则的排序生成序号，保证列表内顺序稳定。
// 最后更新统计与 evaluated_at 时间戳作为缓存依据。
func (r *playlistRepository) refreshSmartPlaylist(pls *model.Playlist) bool {
	// Only refresh if it is a smart playlist and was not refreshed within the interval provided by the refresh delay config
	if !pls.IsSmartPlaylist() || (pls.EvaluatedAt != nil && time.Since(*pls.EvaluatedAt) < conf.Server.SmartPlaylistRefreshDelay) {
		return false
	}

	// Never refresh other users' playlists
	usr := loggedUser(r.ctx)
	if pls.OwnerID != usr.ID {
		log.Trace(r.ctx, "Not refreshing smart playlist from other user", "playlist", pls.Name, "id", pls.ID)
		return false
	}

	log.Debug(r.ctx, "Refreshing smart playlist", "playlist", pls.Name, "id", pls.ID)
	start := time.Now()

	// Remove old tracks
	del := Delete("playlist_tracks").Where(Eq{"playlist_id": pls.ID})
	_, err := r.executeSQL(del)
	if err != nil {
		log.Error(r.ctx, "Error deleting old smart playlist tracks", "playlist", pls.Name, "id", pls.ID, err)
		return false
	}

	// Re-populate playlist based on Smart Playlist criteria
	rules := *pls.Rules

	// If the playlist depends on other playlists, recursively refresh them first
	childPlaylistIds := rules.ChildPlaylistIds()
	for _, id := range childPlaylistIds {
		childPls, err := r.Get(id)
		if err != nil {
			log.Error(r.ctx, "Error loading child playlist", "id", pls.ID, "childId", id, err)
			return false
		}
		r.refreshSmartPlaylist(childPls)
	}

	sq := Select("row_number() over (order by "+rules.OrderBy()+") as id", "'"+pls.ID+"' as playlist_id", "media_file.id as media_file_id").
		From("media_file").LeftJoin("annotation on (" +
		"annotation.item_id = media_file.id" +
		" AND annotation.item_type = 'media_file'" +
		" AND annotation.user_id = '" + usr.ID + "')")

	// Only include media files from libraries the user has access to
	sq = r.applyLibraryFilter(sq, "media_file")

	// Apply the criteria rules
	sq = r.addCriteria(sq, rules)
	insSql := Insert("playlist_tracks").Columns("id", "playlist_id", "media_file_id").Select(sq)
	_, err = r.executeSQL(insSql)
	if err != nil {
		log.Error(r.ctx, "Error refreshing smart playlist tracks", "playlist", pls.Name, "id", pls.ID, err)
		return false
	}

	// Update playlist stats
	err = r.refreshCounters(pls)
	if err != nil {
		log.Error(r.ctx, "Error updating smart playlist stats", "playlist", pls.Name, "id", pls.ID, err)
		return false
	}

	// Update when the playlist was last refreshed (for cache purposes)
	updSql := Update(r.tableName).Set("evaluated_at", time.Now()).Where(Eq{"id": pls.ID})
	_, err = r.executeSQL(updSql)
	if err != nil {
		log.Error(r.ctx, "Error updating smart playlist", "playlist", pls.Name, "id", pls.ID, err)
		return false
	}

	log.Debug(r.ctx, "Refreshed playlist", "playlist", pls.Name, "id", pls.ID, "numTracks", pls.SongCount, "elapsed", time.Since(start))

	return true
}

// addCriteria 把智能列表规则转换为 WHERE / ORDER BY / LIMIT 子句。
func (r *playlistRepository) addCriteria(sql SelectBuilder, c criteria.Criteria) SelectBuilder {
	sql = sql.Where(c)
	if c.Limit > 0 {
		sql = sql.Limit(uint64(c.Limit)).Offset(uint64(c.Offset))
	}
	if order := c.OrderBy(); order != "" {
		sql = sql.OrderBy(order)
	}
	return sql
}

// updateTracks 用给定曲目整体替换列表内容。
func (r *playlistRepository) updateTracks(id string, tracks model.MediaFiles) error {
	ids := make([]string, len(tracks))
	for i := range tracks {
		ids[i] = tracks[i].ID
	}
	return r.updatePlaylist(id, ids)
}

// updatePlaylist 以「先删后插」的方式重建列表内容。
// 全量替换比逐条 diff 更简单可靠，且列表规模通常有限。
func (r *playlistRepository) updatePlaylist(playlistId string, mediaFileIds []string) error {
	if !r.isWritable(playlistId) {
		return rest.ErrPermissionDenied
	}

	// Remove old tracks
	del := Delete("playlist_tracks").Where(Eq{"playlist_id": playlistId})
	_, err := r.executeSQL(del)
	if err != nil {
		return err
	}

	return r.addTracks(playlistId, 1, mediaFileIds)
}

// addTracks 从 startingPos 开始追加曲目。
// playlist_tracks.id 即列表内的位置序号，故插入时需自行递增维护。
func (r *playlistRepository) addTracks(playlistId string, startingPos int, mediaFileIds []string) error {
	// Break the track list in chunks to avoid hitting SQLITE_MAX_VARIABLE_NUMBER limit
	// Add new tracks, chunk by chunk
	// 每 200 条一批，规避 SQLite 单语句参数数量上限
	pos := startingPos
	for chunk := range slices.Chunk(mediaFileIds, 200) {
		ins := Insert("playlist_tracks").Columns("playlist_id", "media_file_id", "id")
		for _, t := range chunk {
			ins = ins.Values(playlistId, t, pos)
			pos++
		}
		_, err := r.executeSQL(ins)
		if err != nil {
			return err
		}
	}

	return r.refreshCounters(&model.Playlist{ID: playlistId})
}

// refreshCounters updates total playlist duration, size and count
// refreshCounters 重算并写回列表的总时长、总大小与曲目数，
// 同时同步更新传入对象的字段，省去调用方重新查询。
func (r *playlistRepository) refreshCounters(pls *model.Playlist) error {
	statsSql := Select(
		"coalesce(sum(duration), 0) as duration",
		"coalesce(sum(size), 0) as size",
		"count(*) as count",
	).
		From("media_file").
		Join("playlist_tracks f on f.media_file_id = media_file.id").
		Where(Eq{"playlist_id": pls.ID})
	var res struct{ Duration, Size, Count float32 }
	err := r.queryOne(statsSql, &res)
	if err != nil {
		return err
	}

	// Update playlist's total duration, size and count
	upd := Update("playlist").
		Set("duration", res.Duration).
		Set("size", res.Size).
		Set("song_count", res.Count).
		Set("updated_at", time.Now()).
		Where(Eq{"id": pls.ID})
	_, err = r.executeSQL(upd)
	if err != nil {
		return err
	}
	pls.SongCount = int(res.Count)
	pls.Duration = res.Duration
	pls.Size = int64(res.Size)
	return nil
}

// loadTracks 加载列表曲目，附带当前用户的标注（星标、评分、播放次数）
// 与所属音乐库信息。annotation 用 LEFT JOIN 并 coalesce 兜底，
// 因为从未被标注过的曲目不存在对应记录。
func (r *playlistRepository) loadTracks(sel SelectBuilder, id string) (model.PlaylistTracks, error) {
	sel = r.applyLibraryFilter(sel, "f")
	userID := loggedUser(r.ctx).ID
	tracksQuery := sel.
		Columns(
			"coalesce(starred, 0) as starred",
			"starred_at",
			"coalesce(play_count, 0) as play_count",
			"play_date",
			"coalesce(rating, 0) as rating",
			"rated_at",
			"f.*",
			"playlist_tracks.*",
			"library.path as library_path",
			"library.name as library_name",
		).
		LeftJoin("annotation on (" +
			"annotation.item_id = media_file_id" +
			" AND annotation.item_type = 'media_file'" +
			" AND annotation.user_id = '" + userID + "')").
		Join("media_file f on f.id = media_file_id").
		Join("library on f.library_id = library.id").
		Where(Eq{"playlist_id": id})
	tracks := dbPlaylistTracks{}
	err := r.queryAll(tracksQuery, &tracks)
	if err != nil {
		return nil, err
	}
	return tracks.toModels(), err
}

// 以下实现 rest.Repository / rest.Persistable，供通用 REST 层调用。

func (r *playlistRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *playlistRepository) Read(id string) (interface{}, error) {
	return r.Get(id)
}

func (r *playlistRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *playlistRepository) EntityName() string {
	return "playlist"
}

func (r *playlistRepository) NewInstance() interface{} {
	return &model.Playlist{}
}

// Save 新建播放列表，所有者强制设为当前用户，
// 并清空传入的 ID 以防客户端伪造 ID 覆盖已有列表。
func (r *playlistRepository) Save(entity interface{}) (string, error) {
	pls := entity.(*model.Playlist)
	pls.OwnerID = loggedUser(r.ctx).ID
	pls.ID = "" // Make sure we don't override an existing playlist
	err := r.Put(pls)
	if err != nil {
		return "", err
	}
	return pls.ID, err
}

// Update 更新播放列表。
// 非管理员有两重限制：只能改自己的列表，且不能转移所有权。
func (r *playlistRepository) Update(id string, entity interface{}, cols ...string) error {
	pls := dbPlaylist{Playlist: *entity.(*model.Playlist)}
	current, err := r.Get(id)
	if err != nil {
		return err
	}
	usr := loggedUser(r.ctx)
	if !usr.IsAdmin {
		// Only the owner can update the playlist
		if current.OwnerID != usr.ID {
			return rest.ErrPermissionDenied
		}
		// Regular users can't change the ownership of a playlist
		if pls.OwnerID != "" && pls.OwnerID != usr.ID {
			return rest.ErrPermissionDenied
		}
	}
	pls.ID = id
	pls.UpdatedAt = time.Now()
	_, err = r.put(id, pls, append(cols, "updatedAt")...)
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

// removeOrphans 清理指向已删除曲目的列表项，由 GC 调用。
// 先找出受影响的列表，逐个删除孤儿项后重新编号，
// 否则位置序号会出现空洞。
func (r *playlistRepository) removeOrphans() error {
	sel := Select("playlist_tracks.playlist_id as id", "p.name").From("playlist_tracks").
		Join("playlist p on playlist_tracks.playlist_id = p.id").
		LeftJoin("media_file mf on playlist_tracks.media_file_id = mf.id").
		Where(Eq{"mf.id": nil}).
		GroupBy("playlist_tracks.playlist_id")

	var pls []struct{ Id, Name string }
	err := r.queryAll(sel, &pls)
	if err != nil {
		return fmt.Errorf("fetching playlists with orphan tracks: %w", err)
	}

	for _, pl := range pls {
		log.Debug(r.ctx, "Cleaning-up orphan tracks from playlist", "id", pl.Id, "name", pl.Name)
		del := Delete("playlist_tracks").Where(And{
			ConcatExpr("media_file_id not in (select id from media_file)"),
			Eq{"playlist_id": pl.Id},
		})
		n, err := r.executeSQL(del)
		if n == 0 || err != nil {
			return fmt.Errorf("deleting orphan tracks from playlist %s: %w", pl.Name, err)
		}
		log.Debug(r.ctx, "Deleted tracks, now reordering", "id", pl.Id, "name", pl.Name, "deleted", n)

		// Renumber the playlist if any track was removed
		if err := r.renumber(pl.Id); err != nil {
			return fmt.Errorf("renumbering playlist %s: %w", pl.Name, err)
		}
	}
	return nil
}

// renumber 按现有顺序重建列表，使位置序号重新连续。
func (r *playlistRepository) renumber(id string) error {
	var ids []string
	sq := Select("media_file_id").From("playlist_tracks").Where(Eq{"playlist_id": id}).OrderBy("id")
	err := r.queryAllSlice(sq, &ids)
	if err != nil {
		return err
	}
	return r.updatePlaylist(id, ids)
}

// isWritable 判断当前用户能否修改列表内容：管理员或所有者。
func (r *playlistRepository) isWritable(playlistId string) bool {
	usr := loggedUser(r.ctx)
	if usr.IsAdmin {
		return true
	}
	pls, err := r.Get(playlistId)
	return err == nil && pls.OwnerID == usr.ID
}

var _ model.PlaylistRepository = (*playlistRepository)(nil)
var _ rest.Repository = (*playlistRepository)(nil)
var _ rest.Persistable = (*playlistRepository)(nil)
