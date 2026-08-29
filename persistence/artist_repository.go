package persistence

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/google/uuid"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

// artistRepository 是艺人仓储。
//
// 艺人与其他实体的显著差别在于统计信息：同一位艺人可跨多个音乐库、
// 并以多种角色（主唱、作曲、制作人等）出现，
// 因此统计数据按「库 × 角色」二维存放在 library_artist 表的 JSON 列中，
// 读取时需按用户可见范围聚合。
type artistRepository struct {
	sqlRepository
	indexGroups utils.IndexGroups
}

// dbArtist 是 model.Artist 的数据库映射层。
type dbArtist struct {
	*model.Artist    `structs:",flatten"`
	SimilarArtists   string `structs:"-" json:"-"`
	LibraryStatsJSON string `structs:"-" json:"-"`
}

// dbSimilarArtist 是相似艺人在 JSON 列中的存储形式，只存 ID 与名字。
type dbSimilarArtist struct {
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
}

// PostScan 把按库分组的统计 JSON 聚合为按角色汇总的结果。
//
// JSON 结构为 {库ID: {角色: {"m": 曲目数, "a": 专辑数, "s": 字节数}}}，
// 键名极短是为了压缩存储体积（每位艺人每个库都有一份）。
// 这里遍历所有库并按角色累加，得到该用户可见范围内的总计；
// 特殊键 "total" 汇总到艺人的顶层字段。
func (a *dbArtist) PostScan() error {
	a.Artist.Stats = make(map[model.Role]model.ArtistStats)

	if a.LibraryStatsJSON != "" {
		var rawLibStats map[string]map[string]map[string]int64
		if err := json.Unmarshal([]byte(a.LibraryStatsJSON), &rawLibStats); err != nil {
			return fmt.Errorf("parsing artist stats from db: %w", err)
		}

		for _, stats := range rawLibStats {
			// Sum all libraries roles stats
			// 跨库累加同一角色的统计
			for key, stat := range stats {
				// Aggregate stats into the main Artist.Stats map
				artistStats := model.ArtistStats{
					SongCount:  int(stat["m"]),
					AlbumCount: int(stat["a"]),
					Size:       stat["s"],
				}

				// Store total stats into the main attributes
				if key == "total" {
					a.Artist.Size += artistStats.Size
					a.Artist.SongCount += artistStats.SongCount
					a.Artist.AlbumCount += artistStats.AlbumCount
				}

				// 跳过无法识别的角色（可能来自旧版本或配置变更）
				role := model.RoleFromString(key)
				if role == model.RoleInvalid {
					continue
				}

				current := a.Artist.Stats[role]
				current.Size += artistStats.Size
				current.SongCount += artistStats.SongCount
				current.AlbumCount += artistStats.AlbumCount
				a.Artist.Stats[role] = current
			}
		}
	}

	a.Artist.SimilarArtists = nil
	if a.SimilarArtists == "" {
		return nil
	}
	var sa []dbSimilarArtist
	if err := json.Unmarshal([]byte(a.SimilarArtists), &sa); err != nil {
		return fmt.Errorf("parsing similar artists from db: %w", err)
	}
	for _, s := range sa {
		a.Artist.SimilarArtists = append(a.Artist.SimilarArtists, model.Artist{
			ID:   s.ID,
			Name: s.Name,
		})
	}
	return nil
}

// PostMapArgs 写库前生成相似艺人 JSON 与全文索引。
//
// 排序名与 MBID 为空时从写入集合中删除而非写空值：
// 艺人记录会被多首曲目反复写入，其中部分曲目可能缺少这些标签，
// 若写空会把先前从完整标签中获得的信息抹掉。
func (a *dbArtist) PostMapArgs(m map[string]any) error {
	sa := make([]dbSimilarArtist, 0)
	for _, s := range a.Artist.SimilarArtists {
		sa = append(sa, dbSimilarArtist{ID: s.ID, Name: s.Name})
	}
	similarArtists, _ := json.Marshal(sa)
	m["similar_artists"] = string(similarArtists)
	m["full_text"] = formatFullText(a.Name, a.SortArtistName)

	// Do not override the sort_artist_name and mbz_artist_id fields if they are empty
	// TODO: Better way to handle this?
	if v, ok := m["sort_artist_name"]; !ok || v.(string) == "" {
		delete(m, "sort_artist_name")
	}
	if v, ok := m["mbz_artist_id"]; !ok || v.(string) == "" {
		delete(m, "mbz_artist_id")
	}
	return nil
}

type dbArtists []dbArtist

func (dba dbArtists) toModels() model.Artists {
	res := make(model.Artists, len(dba))
	for i := range dba {
		res[i] = *dba[i].Artist
	}
	return res
}

// NewArtistRepository 创建艺人仓储。
// indexGroups 是首字母索引的分组规则（如把 A/Á/À 归为一组），
// 用于生成按字母浏览的索引。
// 统计相关的排序直接用 JSON 路径提取器，避免额外建列。
func NewArtistRepository(ctx context.Context, db dbx.Builder) model.ArtistRepository {
	r := &artistRepository{}
	r.ctx = ctx
	r.db = db
	r.indexGroups = utils.ParseIndexGroups(conf.Server.IndexGroups)
	r.tableName = "artist" // To be used by the idFilter below
	r.registerModel(&model.Artist{}, map[string]filterFunc{
		"id":         idFilter(r.tableName),
		"name":       fullTextFilter(r.tableName, "mbz_artist_id"),
		"starred":    booleanFilter,
		"role":       roleFilter,
		"missing":    booleanFilter,
		"library_id": artistLibraryIdFilter,
	})
	r.setSortMappings(map[string]string{
		"name":        "order_artist_name",
		"starred_at":  "starred, starred_at",
		"rated_at":    "rating, rated_at",
		"song_count":  "stats->>'total'->>'m'",
		"album_count": "stats->>'total'->>'a'",
		"size":        "stats->>'total'->>'s'",

		// Stats by credits that are currently available
		"maincredit_song_count":  "sum(stats->>'maincredit'->>'m')",
		"maincredit_album_count": "sum(stats->>'maincredit'->>'a')",
		"maincredit_size":        "sum(stats->>'maincredit'->>'s')",
	})
	return r
}

// roleFilter 按角色筛选艺人：该角色下的曲目数存在即视为担任过此角色。
// 角色名必须先经白名单校验，非法值返回恒假条件，
// 因为角色名会被直接拼入 JSON 路径而无法参数化。
func roleFilter(_ string, role any) Sqlizer {
	if role, ok := role.(string); ok {
		if _, ok := model.AllRoles[role]; ok {
			return Expr("JSON_EXTRACT(library_artist.stats, '$." + role + ".m') IS NOT NULL")
		}
	}
	return Eq{"1": 2}
}

// artistLibraryIdFilter filters artists based on library access through the library_artist table
// artistLibraryIdFilter 通过 library_artist 关联表按音乐库过滤艺人。
func artistLibraryIdFilter(_ string, value interface{}) Sqlizer {
	return Eq{"library_artist.library_id": value}
}

// applyLibraryFilterToArtistQuery applies library filtering to artist queries through the library_artist junction table
// applyLibraryFilterToArtistQuery 通过 library_artist 关联表施加音乐库过滤。
//
// 与其他实体不同，艺人表本身没有 library_id 列（艺人跨库共享），
// 必须经由关联表判断归属。
// 这里用 INNER JOIN 还有一个副作用：在任何库中都没有作品的艺人
// 会被自动排除，无需额外条件。
func (r *artistRepository) applyLibraryFilterToArtistQuery(query SelectBuilder) SelectBuilder {
	user := loggedUser(r.ctx)
	// Join with library_artist first to ensure only artists with content in libraries are included
	// Exclude artists with empty stats (no actual content in the library)
	query = query.Join("library_artist on library_artist.artist_id = artist.id")
	//query = query.Join("library_artist on library_artist.artist_id = artist.id AND library_artist.stats != '{}'")

	// Admin users see all artists from all libraries, no additional filtering needed
	if user.ID != invalidUserId && !user.IsAdmin {
		// Apply library filtering only for non-admin users by joining with their accessible libraries
		query = query.Join("user_library on user_library.library_id = library_artist.library_id AND user_library.user_id = ?", user.ID)
	}

	return query
}

// selectArtist 构建艺人的标准查询。
//
// 用 JSON_GROUP_OBJECT 把该艺人在各个库中的统计聚合成单个 JSON 对象，
// 配合 GROUP BY artist.id 把 JOIN 展开的多行折回一行，
// 再由 PostScan 解析并累加。这样一次查询即可拿到跨库汇总的统计。
func (r *artistRepository) selectArtist(options ...model.QueryOptions) SelectBuilder {
	// Stats Format: {"1": {"albumartist": {"m": 10, "a": 5, "s": 1024}, "artist": {...}}, "2": {...}}
	query := r.newSelect(options...).Columns("artist.*",
		"JSON_GROUP_OBJECT(library_artist.library_id, JSONB(library_artist.stats)) as library_stats_json")

	query = r.applyLibraryFilterToArtistQuery(query)
	query = query.GroupBy("artist.id")
	return r.withAnnotation(query, "artist.id")
}

// CountAll 统计当前用户可见的艺人数。
func (r *artistRepository) CountAll(options ...model.QueryOptions) (int64, error) {
	query := r.newSelect()
	query = r.applyLibraryFilterToArtistQuery(query)
	query = r.withAnnotation(query, "artist.id")
	return r.count(query, options...)
}

// Exists checks if an artist with the given ID exists in the database and is accessible by the current user.
// Exists 判断艺人是否存在且当前用户有权访问。
// 不能直接用基类的 exists：艺人的权限判定需经 library_artist 关联表。
func (r *artistRepository) Exists(id string) (bool, error) {
	// Create a query using the same library filtering logic as selectArtist()
	query := r.newSelect().Columns("count(distinct artist.id) as exist").Where(Eq{"artist.id": id})
	query = r.applyLibraryFilterToArtistQuery(query)

	var res struct{ Exist int64 }
	err := r.queryOne(query, &res)
	return res.Exist > 0, err
}

// Put 写入艺人。colsToUpdate 可限定只更新部分列。
func (r *artistRepository) Put(a *model.Artist, colsToUpdate ...string) error {
	dba := &dbArtist{Artist: a}
	dba.CreatedAt = P(time.Now())
	dba.UpdatedAt = dba.CreatedAt
	_, err := r.put(dba.ID, dba, colsToUpdate...)
	return err
}

// UpdateExternalInfo 只更新来自外部服务的信息（简介、头像、相似艺人等），
// 避免覆盖扫描器写入的本地元数据。
func (r *artistRepository) UpdateExternalInfo(a *model.Artist) error {
	dba := &dbArtist{Artist: a}
	_, err := r.put(a.ID, dba,
		"biography", "small_image_url", "medium_image_url", "large_image_url",
		"similar_artists", "external_url", "external_info_updated_at")
	return err
}

// Get 按 ID 读取单个艺人。
// 用 queryAll 而非 queryOne：查询含 GROUP BY 聚合，走同一路径更稳妥。
func (r *artistRepository) Get(id string) (*model.Artist, error) {
	sel := r.selectArtist().Where(Eq{"artist.id": id})
	var dba dbArtists
	if err := r.queryAll(sel, &dba); err != nil {
		return nil, err
	}
	if len(dba) == 0 {
		return nil, model.ErrNotFound
	}
	res := dba.toModels()
	return &res[0], nil
}

// GetAll 按条件查询艺人列表。
func (r *artistRepository) GetAll(options ...model.QueryOptions) (model.Artists, error) {
	sel := r.selectArtist(options...)
	var dba dbArtists
	err := r.queryAll(sel, &dba)
	if err != nil {
		return nil, err
	}
	res := dba.toModels()
	return res, err
}

// getIndexKey 计算艺人在字母索引中的归属分组。
// 依配置决定用排序标签还是系统规整名作为依据，
// 按前缀匹配索引分组规则，未命中的归入 "#"（数字、符号、非拉丁字符等）。
func (r *artistRepository) getIndexKey(a model.Artist) string {
	source := a.OrderArtistName
	if conf.Server.PreferSortTags {
		source = cmp.Or(a.SortArtistName, a.OrderArtistName)
	}
	name := strings.ToLower(source)
	for k, v := range r.indexGroups {
		if strings.HasPrefix(name, strings.ToLower(k)) {
			return v
		}
	}
	return "#"
}

// GetIndex returns a list of artists grouped by the first letter of their name, or by the index group if configured.
// It can filter by roles and libraries, and optionally include artists that are missing (i.e., have no albums).
// TODO Cache the index (recalculate at scan time)
//
// GetIndex 返回按首字母（或配置的索引分组）归类的艺人索引，供客户端字母导航使用。
//
// 库 ID 为空时直接返回空索引：这表示用户无任何可访问的库，
// 不加此判断会退化为查询全部艺人。
// 多个角色之间是 OR 关系——担任其中任一角色即应出现在索引中。
// 分组在应用层完成（SQL 难以表达前缀分组规则），最后按分组键排序。
func (r *artistRepository) GetIndex(includeMissing bool, libraryIds []int, roles ...model.Role) (model.ArtistIndexes, error) {
	// Validate library IDs. If no library IDs are provided, return an empty index.
	if len(libraryIds) == 0 {
		return nil, nil
	}

	options := model.QueryOptions{Sort: "name"}
	if len(roles) > 0 {
		roleFilters := slice.Map(roles, func(r model.Role) Sqlizer {
			return roleFilter("role", r.String())
		})
		options.Filters = Or(roleFilters)
	}
	if !includeMissing {
		if options.Filters == nil {
			options.Filters = Eq{"artist.missing": false}
		} else {
			options.Filters = And{options.Filters, Eq{"artist.missing": false}}
		}
	}

	libFilter := artistLibraryIdFilter("library_id", libraryIds)
	if options.Filters == nil {
		options.Filters = libFilter
	} else {
		options.Filters = And{options.Filters, libFilter}
	}

	artists, err := r.GetAll(options)
	if err != nil {
		return nil, err
	}

	var result model.ArtistIndexes
	for k, v := range slice.Group(artists, r.getIndexKey) {
		result = append(result, model.ArtistIndex{ID: k, Artists: v})
	}
	slices.SortFunc(result, func(a, b model.ArtistIndex) int {
		return cmp.Compare(a.ID, b.ID)
	})
	return result, nil
}

// purgeEmpty 删除不再关联任何专辑的艺人，由 GC 调用。
func (r *artistRepository) purgeEmpty() error {
	del := Delete(r.tableName).Where("id not in (select artist_id from album_artists)")
	c, err := r.executeSQL(del)
	if err != nil {
		return fmt.Errorf("purging empty artists: %w", err)
	}
	if c > 0 {
		log.Debug(r.ctx, "Purged empty artists", "totalDeleted", c)
	}
	return nil
}

// markMissing marks artists as missing if all their albums are missing.
// markMissing 把「所有专辑都已缺失」的艺人标记为缺失。
//
// 单条 SQL 完成：CTE 先求出仍有在线专辑的艺人集合，
// 再用「不在该集合中」一次性更新全部艺人的 missing 标志。
// 注意这是全量重算而非增量，因此也会把恢复的艺人重新标为未缺失。
func (r *artistRepository) markMissing() error {
	q := Expr(`
with artists_with_non_missing_albums as (
    select distinct aa.artist_id
    from album_artists aa
    join album a on aa.album_id = a.id
    where a.missing = false
)
update artist
set missing = (artist.id not in (select artist_id from artists_with_non_missing_albums));
        `)
	_, err := r.executeSQL(q)
	if err != nil {
		return fmt.Errorf("marking missing artists: %w", err)
	}
	return nil
}

// RefreshPlayCounts updates the play count and last play date annotations for all artists, based
// on the media files associated with them.
//
// RefreshPlayCounts 依据曲目播放记录重算所有艺人的播放次数与最近播放时间。
//
// 与专辑不同，曲目与艺人是多对多关系且存于 JSON 列，
// 故需用 json_tree 展开 participants 中的 artist 数组取出艺人 ID
// （key = 'id' 用于只取 ID 节点，排除 name 等其他字段），
// 再按「用户 + 艺人」聚合后 upsert。
func (r *artistRepository) RefreshPlayCounts() (int64, error) {
	query := Expr(`
with play_counts as (
    select user_id, atom as artist_id, sum(play_count) as total_play_count, max(play_date) as last_play_date
    from media_file
    join annotation on item_id = media_file.id
    left join json_tree(participants, '$.artist') as jt
    where atom is not null and key = 'id'
    group by user_id, atom
)
insert into annotation (user_id, item_id, item_type, play_count, play_date)
select user_id, artist_id, 'artist', total_play_count, last_play_date
from play_counts
where total_play_count > 0
on conflict (user_id, item_id, item_type) do update
    set play_count = excluded.play_count,
        play_date  = excluded.play_date;
`)
	return r.executeSQL(query)
}

// RefreshStats updates the stats field for artists whose associated media files were updated after the oldest recorded library scan time.
// When allArtists is true, it refreshes stats for all artists. It processes artists in batches to handle potentially large updates.
// This method now calculates per-library statistics and stores them in the library_artist junction table.
//
// RefreshStats 重算艺人统计（曲目数、专辑数、总字节数），按「库 × 角色」写入 library_artist.stats。
//
// 增量模式下只处理 updated_at 晚于「最早一次库扫描时间」的艺人；
// 取最早而非最晚，是为了避免多库扫描时间不一致导致漏算。
//
// SQL 中用四个 CTE 分别统计：按具体角色、总计（total）、
// 以及主要参与（maincredit，只含专辑艺人与曲目艺人，
// 用于区分「是这张专辑的艺人」与「只是参与了某首曲子」）。
// 三者 UNION 后按「艺人 × 库」聚合成 JSON 写回。
//
// 分批处理（每批 1000）并手工拼接 IN 占位符，是因为 SQLite
// 对单条语句的参数个数有上限；占位符在四处 IN 子句中重复出现，
// 故参数数组需按批长度重复四份。
func (r *artistRepository) RefreshStats(allArtists bool) (int64, error) {
	var allTouchedArtistIDs []string
	if allArtists {
		// Refresh stats for all artists
		allArtistsQuerySQL := `SELECT DISTINCT id FROM artist WHERE id <> ''`
		if err := r.db.NewQuery(allArtistsQuerySQL).Column(&allTouchedArtistIDs); err != nil {
			return 0, fmt.Errorf("fetching all artist IDs: %w", err)
		}
		log.Debug(r.ctx, "RefreshStats: Refreshing all artists.", "count", len(allTouchedArtistIDs))
	} else {
		// Only refresh artists with updated timestamps
		touchedArtistsQuerySQL := `
        SELECT DISTINCT id
        FROM artist
        WHERE updated_at > (SELECT last_scan_at FROM library ORDER BY last_scan_at ASC LIMIT 1)
        `
		if err := r.db.NewQuery(touchedArtistsQuerySQL).Column(&allTouchedArtistIDs); err != nil {
			return 0, fmt.Errorf("fetching touched artist IDs: %w", err)
		}
		log.Debug(r.ctx, "RefreshStats: Refreshing touched artists.", "count", len(allTouchedArtistIDs))
	}

	if len(allTouchedArtistIDs) == 0 {
		log.Debug(r.ctx, "RefreshStats: No artists to update.")
		return 0, nil
	}

	// Template for the batch update with placeholder markers that we'll replace
	// This now calculates per-library statistics and stores them in library_artist.stats
	batchUpdateStatsSQL := `
    WITH artist_role_counters AS (
        SELECT mfa.artist_id,
               mf.library_id,
               mfa.role,
               count(DISTINCT mf.album_id) AS album_count,
               count(DISTINCT mf.id) AS count,
               sum(mf.size) AS size
        FROM media_file_artists mfa
        JOIN media_file mf ON mfa.media_file_id = mf.id
        WHERE mfa.artist_id IN (ROLE_IDS_PLACEHOLDER) -- Will replace with actual placeholders
        GROUP BY mfa.artist_id, mf.library_id, mfa.role
    ),
    artist_total_counters AS (
        SELECT mfa.artist_id,
               mf.library_id,
               'total' AS role,
               count(DISTINCT mf.album_id) AS album_count,
               count(DISTINCT mf.id) AS count,
               sum(mf.size) AS size
        FROM media_file_artists mfa
        JOIN media_file mf ON mfa.media_file_id = mf.id
        WHERE mfa.artist_id IN (ROLE_IDS_PLACEHOLDER) -- Will replace with actual placeholders
        GROUP BY mfa.artist_id, mf.library_id
    ),
    artist_participant_counter AS (
        SELECT mfa.artist_id,
               mf.library_id,
               'maincredit' AS role,
               count(DISTINCT mf.album_id) AS album_count,
               count(DISTINCT mf.id) AS count,
               sum(mf.size) AS size
        FROM media_file_artists mfa
        JOIN media_file mf ON mfa.media_file_id = mf.id
        WHERE mfa.artist_id IN (ROLE_IDS_PLACEHOLDER) -- Will replace with actual placeholders
        AND mfa.role IN ('albumartist', 'artist')
        GROUP BY mfa.artist_id, mf.library_id
    ),
    combined_counters AS (
        SELECT artist_id, library_id, role, album_count, count, size FROM artist_role_counters
        UNION ALL
        SELECT artist_id, library_id, role, album_count, count, size FROM artist_total_counters
        UNION ALL
        SELECT artist_id, library_id, role, album_count, count, size FROM artist_participant_counter
    ),
    library_artist_counters AS (
        SELECT artist_id,
               library_id,
               json_group_object(
                       role,
                       json_object('a', album_count, 'm', count, 's', size)
               ) AS counters
        FROM combined_counters
        GROUP BY artist_id, library_id
    )
    UPDATE library_artist
    SET stats = coalesce((SELECT counters FROM library_artist_counters lac
                         WHERE lac.artist_id = library_artist.artist_id
                         AND lac.library_id = library_artist.library_id), '{}')
    WHERE library_artist.artist_id IN (ROLE_IDS_PLACEHOLDER);` // Will replace with actual placeholders

	var totalRowsAffected int64 = 0
	const batchSize = 1000

	batchCounter := 0
	for artistIDBatch := range slice.CollectChunks(slices.Values(allTouchedArtistIDs), batchSize) {
		batchCounter++
		log.Trace(r.ctx, "RefreshStats: Processing batch", "batchNum", batchCounter, "batchSize", len(artistIDBatch))

		// Create placeholders for each ID in the IN clauses
		placeholders := make([]string, len(artistIDBatch))
		for i := range artistIDBatch {
			placeholders[i] = "?"
		}
		// Don't add extra parentheses, the IN clause already expects them in SQL syntax
		inClause := strings.Join(placeholders, ",")

		// Replace the placeholder markers with actual SQL placeholders
		batchSQL := strings.Replace(batchUpdateStatsSQL, "ROLE_IDS_PLACEHOLDER", inClause, 4)

		// Create a single parameter array with all IDs (repeated 4 times for each IN clause)
		// We need to repeat each ID 4 times (once for each IN clause)
		args := make([]any, 4*len(artistIDBatch))
		for idx, id := range artistIDBatch {
			for i := range 4 {
				startIdx := i * len(artistIDBatch)
				args[startIdx+idx] = id
			}
		}

		// Now use Expr with the expanded SQL and all parameters
		sqlizer := Expr(batchSQL, args...)

		rowsAffected, err := r.executeSQL(sqlizer)
		if err != nil {
			return totalRowsAffected, fmt.Errorf("executing batch update for artist stats (batch %d): %w", batchCounter, err)
		}
		totalRowsAffected += rowsAffected
	}

	// // Remove library_artist entries for artists that no longer have any content in any library
	// 清理统计为空的关联行：这些艺人在该库中已无任何内容，
	// 保留会让 selectArtist 的 INNER JOIN 把无内容艺人也带出来
	cleanupSQL := Delete("library_artist").Where("stats = '{}'")
	cleanupRows, err := r.executeSQL(cleanupSQL)
	if err != nil {
		log.Warn(r.ctx, "Failed to cleanup empty library_artist entries", "error", err)
	} else if cleanupRows > 0 {
		log.Debug(r.ctx, "Cleaned up empty library_artist entries", "rowsDeleted", cleanupRows)
	}

	log.Debug(r.ctx, "RefreshStats: Successfully updated stats.", "totalArtistsProcessed", len(allTouchedArtistIDs), "totalDBRowsAffected", totalRowsAffected)
	return totalRowsAffected, nil
}

// Search 搜索艺人。全文搜索时按曲目总数倒序，让作品多的艺人优先出现。
func (r *artistRepository) Search(q string, offset int, size int, options ...model.QueryOptions) (model.Artists, error) {
	var res dbArtists
	if uuid.Validate(q) == nil {
		err := r.searchByMBID(r.selectArtist(options...), q, []string{"mbz_artist_id"}, &res)
		if err != nil {
			return nil, fmt.Errorf("searching artist by MBID %q: %w", q, err)
		}
	} else {
		// Natural order for artists is more performant by ID, due to GROUP BY clause in selectArtist
		// 自然序用 artist.id 而非 rowid：selectArtist 含 GROUP BY artist.id，
		// 按同一列排序可复用分组结果，避免额外排序
		err := r.doSearch(r.selectArtist(options...), q, offset, size, &res, "artist.id",
			"sum(json_extract(stats, '$.total.m')) desc", "name")
		if err != nil {
			return nil, fmt.Errorf("searching artist by query %q: %w", q, err)
		}
	}
	return res.toModels(), nil
}

// 以下实现 model.ResourceRepository，供通用 REST 层调用。

func (r *artistRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *artistRepository) Read(id string) (interface{}, error) {
	return r.Get(id)
}

// ReadAll 查询艺人列表。
//
// 统计类排序需依当前筛选的角色动态改写：
// 按「作曲家」浏览时，song_count 应指其作曲的曲目数而非全部曲目数。
// 角色名取自过滤条件，已由 roleFilter 的白名单保证合法。
func (r *artistRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	role := "total"
	if len(options) > 0 {
		if v, ok := options[0].Filters["role"].(string); ok {
			role = v
		}
	}
	r.sortMappings["song_count"] = "sum(stats->>'" + role + "'->>'m')"
	r.sortMappings["album_count"] = "sum(stats->>'" + role + "'->>'a')"
	r.sortMappings["size"] = "sum(stats->>'" + role + "'->>'s')"
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *artistRepository) EntityName() string {
	return "artist"
}

func (r *artistRepository) NewInstance() interface{} {
	return &model.Artist{}
}

var _ model.ArtistRepository = (*artistRepository)(nil)
var _ model.ResourceRepository = (*artistRepository)(nil)
