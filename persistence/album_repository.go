package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/google/uuid"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

// albumRepository 是专辑仓储。专辑并非独立录入，
// 而是扫描时由曲目聚合而来，故有大量与扫描器协作的方法。
type albumRepository struct {
	sqlRepository
}

// dbAlbum 是 model.Album 的数据库映射层，
// 承接四个以 JSON 字符串存储的字段。
type dbAlbum struct {
	*model.Album `structs:",flatten"`
	Discs        string `structs:"-" json:"discs"`
	Participants string `structs:"-" json:"-"`
	Tags         string `structs:"-" json:"-"`
	FolderIDs    string `structs:"-" json:"-"`
}

// PostScan 读库后把 JSON 字段还原为结构化数据。
func (a *dbAlbum) PostScan() error {
	var err error
	if a.Discs != "" {
		if err = json.Unmarshal([]byte(a.Discs), &a.Album.Discs); err != nil {
			return fmt.Errorf("parsing album discs from db: %w", err)
		}
	}
	a.Album.Participants, err = unmarshalParticipants(a.Participants)
	if err != nil {
		return fmt.Errorf("parsing album from db: %w", err)
	}
	if a.Tags != "" {
		a.Album.Tags, err = unmarshalTags(a.Tags)
		if err != nil {
			return fmt.Errorf("parsing album from db: %w", err)
		}
		a.Genre, a.Genres = a.Album.Tags.ToGenres()
	}
	if a.FolderIDs != "" {
		var ids []string
		if err = json.Unmarshal([]byte(a.FolderIDs), &ids); err != nil {
			return fmt.Errorf("parsing album folder_ids from db: %w", err)
		}
		a.Album.FolderIDs = ids
	}
	return nil
}

// PostMapArgs 写库前生成全文索引与各 JSON 列。
// 全文索引除专辑名与艺人外，还纳入碟片副标题、版本号与目录编号，
// 使用户能通过「豪华版」「碟名」等信息搜到专辑。
func (a *dbAlbum) PostMapArgs(args map[string]any) error {
	fullText := []string{a.Name, a.SortAlbumName, a.AlbumArtist}
	fullText = append(fullText, a.Album.Participants.AllNames()...)
	fullText = append(fullText, slices.Collect(maps.Values(a.Album.Discs))...)
	fullText = append(fullText, a.Album.Tags[model.TagAlbumVersion]...)
	fullText = append(fullText, a.Album.Tags[model.TagCatalogNumber]...)
	args["full_text"] = formatFullText(fullText...)

	args["tags"] = marshalTags(a.Album.Tags)
	args["participants"] = marshalParticipants(a.Album.Participants)

	folderIDs, err := json.Marshal(a.Album.FolderIDs)
	if err != nil {
		return fmt.Errorf("marshalling album folder_ids: %w", err)
	}
	args["folder_ids"] = string(folderIDs)

	b, err := json.Marshal(a.Album.Discs)
	if err != nil {
		return fmt.Errorf("marshalling album discs: %w", err)
	}
	args["discs"] = string(b)
	return nil
}

type dbAlbums []dbAlbum

func (as dbAlbums) toModels() model.Albums {
	return slice.Map(as, func(a dbAlbum) model.Album { return *a.Album })
}

// NewAlbumRepository 创建专辑仓储。
//
// 按艺人排序时以 compilation 打头，使合辑集中排在一处，
// 不与该艺人的个人专辑混杂。
// max_year 的排序表达式优先用原始发行日期，缺失时退回年份，
// 从而让同年发行的专辑仍能按具体日期排序。
func NewAlbumRepository(ctx context.Context, db dbx.Builder) model.AlbumRepository {
	r := &albumRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "album"
	r.registerModel(&model.Album{}, albumFilters())
	r.setSortMappings(map[string]string{
		"name":         "order_album_name, order_album_artist_name",
		"artist":       "compilation, order_album_artist_name, order_album_name",
		"album_artist": "compilation, order_album_artist_name, order_album_name",
		// TODO Rename this to just year (or date)
		"max_year":       "coalesce(nullif(original_date,''), cast(max_year as text)), release_date, name",
		"random":         "random",
		"recently_added": recentlyAddedSort(),
		"starred_at":     "starred, starred_at",
		"rated_at":       "rating, rated_at",
	})
	return r
}

// albumFilters 构建专辑的过滤器集合，惰性初始化以等待配置就绪。
// 除固定过滤器外，还自动为每个专辑级标签和每种艺人角色注册过滤器，
// 使前端可按任意标签或「某人担任制作人的专辑」来筛选。
var albumFilters = sync.OnceValue(func() map[string]filterFunc {
	filters := map[string]filterFunc{
		"id":              idFilter("album"),
		"name":            fullTextFilter("album", "mbz_album_id", "mbz_release_group_id"),
		"compilation":     booleanFilter,
		"artist_id":       artistFilter,
		"year":            yearFilter,
		"recently_played": recentlyPlayedFilter,
		"starred":         booleanFilter,
		"has_rating":      hasRatingFilter,
		"missing":         booleanFilter,
		"genre_id":        tagIDFilter,
		"role_total_id":   allRolesFilter,
		"library_id":      libraryIdFilter,
	}
	// Add all album tags as filters
	for tag := range model.AlbumLevelTags() {
		filters[string(tag)] = tagIDFilter
	}

	for role := range model.AllRoles {
		filters["role_"+role+"_id"] = artistRoleFilter
	}

	return filters
})

// recentlyAddedSort 决定「最近添加」依据入库时间还是文件修改时间。
func recentlyAddedSort() string {
	if conf.Server.RecentlyAddedByModTime {
		return "updated_at"
	}
	return "created_at"
}

// recentlyPlayedFilter 筛选播放过的专辑。
func recentlyPlayedFilter(string, interface{}) Sqlizer {
	return Gt{"play_count": 0}
}

// hasRatingFilter 筛选已评分的专辑。
func hasRatingFilter(string, interface{}) Sqlizer {
	return Gt{"rating": 0}
}

// yearFilter 按年份筛选专辑。
//
// 专辑可能横跨多年（如合辑），故记录了 min_year 与 max_year。
// 第一个分支匹配「目标年份落在区间内」，要求 min_year > 0 排除未知年份；
// 第二个分支兜底只有 max_year 的情况。
func yearFilter(_ string, value interface{}) Sqlizer {
	return Or{
		And{
			Gt{"min_year": 0},
			LtOrEq{"min_year": value},
			GtOrEq{"max_year": value},
		},
		Eq{"max_year": value},
	}
}

// artistFilter 按艺人筛选：专辑艺人或曲目艺人命中即可。
// 两者都查是为了让「合辑中的参演艺人」也能检索到该专辑。
func artistFilter(_ string, value interface{}) Sqlizer {
	return Or{
		Exists("json_tree(participants, '$.albumartist')", Eq{"value": value}),
		Exists("json_tree(participants, '$.artist')", Eq{"value": value}),
	}
}

// artistRoleFilter 按「某艺人担任某角色」筛选，参数名形如 role_composer_id。
// 角色名非法时返回恒假条件（Gt{"": nil}），
// 使查询安全返回空结果而非把非法角色名拼进 SQL。
func artistRoleFilter(name string, value interface{}) Sqlizer {
	roleName := strings.TrimSuffix(strings.TrimPrefix(name, "role_"), "_id")

	// Check if the role name is valid. If not, return an invalid filter
	if _, ok := model.AllRoles[roleName]; !ok {
		return Gt{"": nil}
	}
	return Exists(fmt.Sprintf("json_tree(participants, '$.%s')", roleName), Eq{"value": value})
}

// allRolesFilter 按艺人筛选，不限角色。
// 直接对 JSON 文本做 LIKE 匹配艺人 ID，
// 比逐个角色展开 json_tree 快得多；引号包裹避免匹配到 ID 的子串。
func allRolesFilter(_ string, value interface{}) Sqlizer {
	return Like{"participants": fmt.Sprintf(`%%"%s"%%`, value)}
}

// CountAll 统计当前用户可见的专辑总数。
func (r *albumRepository) CountAll(options ...model.QueryOptions) (int64, error) {
	query := r.newSelect()
	query = r.withAnnotation(query, "album.id")
	query = r.applyLibraryFilter(query)
	return r.count(query, options...)
}

// Exists 判断专辑是否存在。
func (r *albumRepository) Exists(id string) (bool, error) {
	return r.exists(Eq{"album.id": id})
}

// Put 写入专辑并同步参与者关联表。
// ImportedAt 每次都刷新，作为「本轮扫描已处理过」的标记，
// 供 GetTouchedAlbums 识别需要重新聚合的专辑。
func (r *albumRepository) Put(al *model.Album) error {
	al.ImportedAt = time.Now()
	id, err := r.put(al.ID, &dbAlbum{Album: al})
	if err != nil {
		return err
	}
	al.ID = id
	if len(al.Participants) > 0 {
		err = r.updateParticipants(al.ID, al.Participants)
		if err != nil {
			return err
		}
	}
	return err
}

// TODO Move external metadata to a separated table
// UpdateExternalInfo 只更新从外部服务（Last.fm、Spotify 等）获取的信息。
// 显式列出待更新列，避免覆盖扫描器写入的本地元数据。
func (r *albumRepository) UpdateExternalInfo(al *model.Album) error {
	_, err := r.put(al.ID, &dbAlbum{Album: al}, "description", "small_image_url", "medium_image_url", "large_image_url", "external_url", "external_info_updated_at")
	return err
}

// selectAlbum 构建专辑的标准查询：附带所属库信息、标注与权限过滤。
func (r *albumRepository) selectAlbum(options ...model.QueryOptions) SelectBuilder {
	sql := r.newSelect(options...).Columns("album.*", "library.path as library_path", "library.name as library_name").
		LeftJoin("library on album.library_id = library.id")
	sql = r.withAnnotation(sql, "album.id")
	return r.applyLibraryFilter(sql)
}

// Get 按 ID 读取单张专辑。
func (r *albumRepository) Get(id string) (*model.Album, error) {
	res, err := r.GetAll(model.QueryOptions{Filters: Eq{"album.id": id}})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, model.ErrNotFound
	}
	return &res[0], nil
}

// GetAll 按条件查询专辑列表。
func (r *albumRepository) GetAll(options ...model.QueryOptions) (model.Albums, error) {
	sq := r.selectAlbum(options...)
	var res dbAlbums
	err := r.queryAll(sq, &res)
	if err != nil {
		return nil, err
	}
	return res.toModels(), err
}

// CopyAttributes 把指定列的值从一张专辑复制到另一张。
// 用于专辑 ID 变更时迁移外部元数据（封面、简介等），避免重新抓取。
// 用 NullStringMap 接收，使 NULL 值也能被正确复制。
func (r *albumRepository) CopyAttributes(fromID, toID string, columns ...string) error {
	var from dbx.NullStringMap
	err := r.queryOne(Select(columns...).From(r.tableName).Where(Eq{"id": fromID}), &from)
	if err != nil {
		return fmt.Errorf("getting album to copy fields from: %w", err)
	}
	to := make(map[string]interface{})
	for _, col := range columns {
		to[col] = from[col]
	}
	_, err = r.executeSQL(Update(r.tableName).SetMap(to).Where(Eq{"id": toID}))
	return err
}

// Touch flags an album as being scanned by the scanner, but not necessarily updated.
// This is used for when missing tracks are detected for an album during scan.
//
// Touch 只刷新 imported_at，把专辑标记为「本轮扫描需要重新聚合」。
// 用于曲目被删除等专辑自身未变、但聚合结果需重算的情形。
func (r *albumRepository) Touch(ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	for ids := range slices.Chunk(ids, 200) {
		upd := Update(r.tableName).Set("imported_at", time.Now()).Where(Eq{"id": ids})
		c, err := r.executeSQL(upd)
		if err != nil {
			return fmt.Errorf("error touching albums: %w", err)
		}
		log.Debug(r.ctx, "Touching albums", "ids", ids, "updated", c)
	}
	return nil
}

// TouchByMissingFolder touches all albums that have missing folders
// TouchByMissingFolder 标记所有含缺失文件夹的专辑，使其在本轮扫描中重新聚合。
// 用 json_each 展开专辑的 folder_ids 列并关联 folder 表判断是否有缺失，
// 一条语句完成，避免逐张专辑检查。
func (r *albumRepository) TouchByMissingFolder() (int64, error) {
	upd := Update(r.tableName).Set("imported_at", time.Now()).
		Where(And{
			NotEq{"folder_ids": nil},
			ConcatExpr("EXISTS (SELECT 1 FROM json_each(folder_ids) AS je JOIN main.folder AS f ON je.value = f.id WHERE f.missing = true)"),
		})
	c, err := r.executeSQL(upd)
	if err != nil {
		return 0, fmt.Errorf("error touching albums by missing folder: %w", err)
	}
	return c, nil
}

// GetTouchedAlbums returns all albums that were touched by the scanner for a given library, in the
// current library scan run.
// It does not need to load participants, as they are not used by the scanner.
//
// GetTouchedAlbums 返回本轮扫描中被触碰过的专辑，供后续重新聚合。
// 判据是 imported_at 晚于上次扫描完成时间；用游标遍历以控制内存。
func (r *albumRepository) GetTouchedAlbums(libID int) (model.AlbumCursor, error) {
	query := r.selectAlbum().
		Where(And{
			Eq{"library.id": libID},
			ConcatExpr("album.imported_at > library.last_scan_at"),
		})
	cursor, err := queryWithStableResults[dbAlbum](r.sqlRepository, query)
	if err != nil {
		return nil, err
	}
	return func(yield func(model.Album, error) bool) {
		for a, err := range cursor {
			if a.Album == nil {
				yield(model.Album{}, fmt.Errorf("unexpected nil album: %v", a))
				return
			}
			if !yield(*a.Album, err) || err != nil {
				return
			}
		}
	}, nil
}

// RefreshPlayCounts updates the play count and last play date annotations for all albums, based
// on the media files associated with them.
//
// RefreshPlayCounts 依据曲目播放记录重算所有专辑的播放次数与最近播放时间。
//
// 用单条 SQL 完成：CTE 先按「用户 + 专辑」聚合曲目播放数据，
// 再 upsert 进 annotation 表。相比在应用层循环，
// 避免了海量往返，也天然保证了原子性。
// 只写入播放数大于 0 的记录，不为从未播放的专辑创建无意义的标注行。
func (r *albumRepository) RefreshPlayCounts() (int64, error) {
	query := Expr(`
with play_counts as (
    select user_id, album_id, sum(play_count) as total_play_count, max(play_date) as last_play_date
    from media_file
             join annotation on item_id = media_file.id
    group by user_id, album_id
)
insert into annotation (user_id, item_id, item_type, play_count, play_date)
select user_id, album_id, 'album', total_play_count, last_play_date
from play_counts
where total_play_count > 0
on conflict (user_id, item_id, item_type) do update
    set play_count = excluded.play_count,
        play_date  = excluded.play_date;
`)
	return r.executeSQL(query)
}

// purgeEmpty 删除已无任何曲目的专辑，由 GC 调用。
// 指定 libraryIDs 时只清理这些库，用于单库扫描后的增量清理。
func (r *albumRepository) purgeEmpty(libraryIDs ...int) error {
	del := Delete(r.tableName).Where("id not in (select distinct(album_id) from media_file)")
	// If libraryIDs are specified, only purge albums from those libraries
	if len(libraryIDs) > 0 {
		del = del.Where(Eq{"library_id": libraryIDs})
	}
	c, err := r.executeSQL(del)
	if err != nil {
		return fmt.Errorf("purging empty albums: %w", err)
	}
	if c > 0 {
		log.Debug(r.ctx, "Purged empty albums", "totalDeleted", c)
	}
	return nil
}

// Search 搜索专辑，关键词为 UUID 时按 MusicBrainz ID 精确查找。
func (r *albumRepository) Search(q string, offset int, size int, options ...model.QueryOptions) (model.Albums, error) {
	var res dbAlbums
	if uuid.Validate(q) == nil {
		err := r.searchByMBID(r.selectAlbum(options...), q, []string{"mbz_album_id", "mbz_release_group_id"}, &res)
		if err != nil {
			return nil, fmt.Errorf("searching album by MBID %q: %w", q, err)
		}
	} else {
		err := r.doSearch(r.selectAlbum(options...), q, offset, size, &res, "album.rowid", "name")
		if err != nil {
			return nil, fmt.Errorf("searching album by query %q: %w", q, err)
		}
	}
	return res.toModels(), nil
}

// 以下实现 model.ResourceRepository，供通用 REST 层调用。

func (r *albumRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *albumRepository) Read(id string) (interface{}, error) {
	return r.Get(id)
}

func (r *albumRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *albumRepository) EntityName() string {
	return "album"
}

func (r *albumRepository) NewInstance() interface{} {
	return &model.Album{}
}

// 编译期接口实现检查
var _ model.AlbumRepository = (*albumRepository)(nil)
var _ model.ResourceRepository = (*albumRepository)(nil)
