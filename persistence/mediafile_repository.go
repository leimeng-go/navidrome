package persistence

import (
	"context"
	"fmt"
	"slices"
	"strconv"
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

// mediaFileRepository 是曲目仓储，是全库数据量最大的表。
type mediaFileRepository struct {
	sqlRepository
}

// dbMediaFile 是 model.MediaFile 的数据库映射层。
//
// 需要这层包装的原因：
//   - Participants 与 Tags 在库中是 JSON 字符串，模型中是结构化类型
//   - ReplayGain 字段的列名（rg_*）与模型字段名（RG*）大小写不匹配，
//     用中间字段承接后在 PostScan 中转存，避免给模型加 db 标签
//
// structs:"-" 表示这些辅助字段不参与写库，写入值由 PostMapArgs 另行提供。
type dbMediaFile struct {
	*model.MediaFile `structs:",flatten"`
	Participants     string `structs:"-" json:"-"`
	Tags             string `structs:"-" json:"-"`
	// These are necessary to map the correct names (rg_*) to the correct fields (RG*)
	// without using `db` struct tags in the model.MediaFile struct
	RgAlbumGain *float64 `structs:"-" json:"-"`
	RgAlbumPeak *float64 `structs:"-" json:"-"`
	RgTrackGain *float64 `structs:"-" json:"-"`
	RgTrackPeak *float64 `structs:"-" json:"-"`
}

// PostScan 在读库后把辅助字段还原为模型字段。
func (m *dbMediaFile) PostScan() error {
	m.RGTrackGain = m.RgTrackGain
	m.RGTrackPeak = m.RgTrackPeak
	m.RGAlbumGain = m.RgAlbumGain
	m.RGAlbumPeak = m.RgAlbumPeak
	var err error
	m.MediaFile.Participants, err = unmarshalParticipants(m.Participants)
	if err != nil {
		return fmt.Errorf("parsing media_file from db: %w", err)
	}
	if m.Tags != "" {
		m.MediaFile.Tags, err = unmarshalTags(m.Tags)
		if err != nil {
			return fmt.Errorf("parsing media_file from db: %w", err)
		}
		// 流派是标签的一个视图，从标签派生而非单独存储
		m.Genre, m.Genres = m.MediaFile.Tags.ToGenres()
	}
	return nil
}

// PostMapArgs 在写库前补充派生列：全文索引与两个 JSON 列。
// full_text 汇总了所有可搜索的文本（含全部参与者姓名），
// 使搜索只需匹配一列。
func (m *dbMediaFile) PostMapArgs(args map[string]any) error {
	fullText := []string{m.FullTitle(), m.Album, m.Artist, m.AlbumArtist,
		m.SortTitle, m.SortAlbumName, m.SortArtistName, m.SortAlbumArtistName, m.DiscSubtitle}
	fullText = append(fullText, m.MediaFile.Participants.AllNames()...)
	args["full_text"] = formatFullText(fullText...)
	args["tags"] = marshalTags(m.MediaFile.Tags)
	args["participants"] = marshalParticipants(m.MediaFile.Participants)
	return nil
}

type dbMediaFiles []dbMediaFile

func (m dbMediaFiles) toModels() model.MediaFiles {
	return slice.Map(m, func(mf dbMediaFile) model.MediaFile { return *mf.MediaFile })
}

// NewMediaFileRepository 创建曲目仓储。
//
// 排序映射中多数字段是复合排序：例如按艺人排序时，
// 实际次序为「艺人 → 专辑 → 发行日期 → 碟号 → 音轨号」，
// 这样同一专辑内的曲目保持自然顺序而非按名称乱排。
// starred_at/rated_at 前置对应的布尔/数值列，
// 使未收藏、未评分的条目聚在一端。
func NewMediaFileRepository(ctx context.Context, db dbx.Builder) model.MediaFileRepository {
	r := &mediaFileRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "media_file"
	r.registerModel(&model.MediaFile{}, mediaFileFilter())
	r.setSortMappings(map[string]string{
		"title":          "order_title",
		"artist":         "order_artist_name, order_album_name, release_date, disc_number, track_number",
		"album_artist":   "order_album_artist_name, order_album_name, release_date, disc_number, track_number",
		"album":          "order_album_name, album_id, disc_number, track_number, order_artist_name, title",
		"random":         "random",
		"created_at":     "media_file.created_at",
		"recently_added": mediaFileRecentlyAddedSort(),
		"starred_at":     "starred, starred_at",
		"rated_at":       "rating, rated_at",
	})
	return r
}

// mediaFileFilter 构建曲目的过滤器集合。
// 用 sync.OnceValue 惰性初始化并缓存：标签映射依赖配置加载完成，
// 不能在包级变量初始化时求值；同时避免每次建仓储都重建整个表。
var mediaFileFilter = sync.OnceValue(func() map[string]filterFunc {
	filters := map[string]filterFunc{
		"id":         idFilter("media_file"),
		"title":      fullTextFilter("media_file", "mbz_recording_id", "mbz_release_track_id"),
		"starred":    booleanFilter,
		"genre_id":   tagIDFilter,
		"missing":    booleanFilter,
		"artists_id": artistFilter,
		"library_id": libraryIdFilter,
	}
	// Add all album tags as filters
	// 把所有已配置的标签自动注册为可过滤字段，
	// 使用户新增自定义标签后无需改代码即可用于筛选
	for tag := range model.TagMappings() {
		if _, exists := filters[string(tag)]; !exists {
			filters[string(tag)] = tagIDFilter
		}
	}
	return filters
})

// mediaFileRecentlyAddedSort 决定「最近添加」依据哪个时间。
// 默认用入库时间；配置后改用文件修改时间，
// 适合把整理过的旧文件重新纳入「最近添加」的用户。
func mediaFileRecentlyAddedSort() string {
	if conf.Server.RecentlyAddedByModTime {
		return "media_file.updated_at"
	}
	return "media_file.created_at"
}

// CountAll 统计当前用户可见的曲目总数。
func (r *mediaFileRepository) CountAll(options ...model.QueryOptions) (int64, error) {
	query := r.newSelect()
	query = r.withAnnotation(query, "media_file.id")
	query = r.applyLibraryFilter(query)
	return r.count(query, options...)
}

// Exists 判断曲目是否存在。
func (r *mediaFileRepository) Exists(id string) (bool, error) {
	return r.exists(Eq{"media_file.id": id})
}

// Put 写入曲目并同步参与者关联表。
// 用「路径 + 库」作为匹配键：扫描器可能算出新的 PID，
// 但同一路径下的文件应视为同一条记录而非新增。
func (r *mediaFileRepository) Put(m *model.MediaFile) error {
	m.CreatedAt = time.Now()
	id, err := r.putByMatch(Eq{"path": m.Path, "library_id": m.LibraryID}, m.ID, &dbMediaFile{MediaFile: m})
	if err != nil {
		return err
	}
	m.ID = id
	return r.updateParticipants(m.ID, m.Participants)
}

// selectMediaFile 构建曲目的标准查询：附带所属库信息、标注、书签与权限过滤。
func (r *mediaFileRepository) selectMediaFile(options ...model.QueryOptions) SelectBuilder {
	sql := r.newSelect(options...).Columns("media_file.*", "library.path as library_path", "library.name as library_name").
		LeftJoin("library on media_file.library_id = library.id")
	sql = r.withAnnotation(sql, "media_file.id")
	sql = r.withBookmark(sql, "media_file.id")
	return r.applyLibraryFilter(sql)
}

// Get 按 ID 读取单首曲目。
// 复用 GetAll 而非直接 queryOne，以确保权限过滤等逻辑一致。
func (r *mediaFileRepository) Get(id string) (*model.MediaFile, error) {
	res, err := r.GetAll(model.QueryOptions{Filters: Eq{"media_file.id": id}})
	if err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, model.ErrNotFound
	}
	return &res[0], nil
}

// GetWithParticipants 读取曲目并补齐参与者的完整艺人信息。
func (r *mediaFileRepository) GetWithParticipants(id string) (*model.MediaFile, error) {
	m, err := r.Get(id)
	if err != nil {
		return nil, err
	}
	m.Participants, err = r.getParticipants(m)
	return m, err
}

// GetAll 按条件查询曲目列表。
func (r *mediaFileRepository) GetAll(options ...model.QueryOptions) (model.MediaFiles, error) {
	sq := r.selectMediaFile(options...)
	var res dbMediaFiles
	err := r.queryAll(sq, &res, options...)
	if err != nil {
		return nil, err
	}
	return res.toModels(), nil
}

// GetCursor 以游标方式遍历曲目，用于扫描等大结果集场景，内存占用恒定。
func (r *mediaFileRepository) GetCursor(options ...model.QueryOptions) (model.MediaFileCursor, error) {
	sq := r.selectMediaFile(options...)
	cursor, err := queryWithStableResults[dbMediaFile](r.sqlRepository, sq)
	if err != nil {
		return nil, err
	}
	return func(yield func(model.MediaFile, error) bool) {
		for m, err := range cursor {
			if m.MediaFile == nil {
				yield(model.MediaFile{}, fmt.Errorf("unexpected nil mediafile: %v", m))
				return
			}
			if !yield(*m.MediaFile, err) || err != nil {
				return
			}
		}
	}, nil
}

// FindByPaths finds media files by their paths.
// The paths can be library-qualified (format: "libraryID:path") or unqualified ("path").
// Library-qualified paths search within the specified library, while unqualified paths
// search across all libraries for backward compatibility.
//
// FindByPaths 按路径批量查找曲目，主要供导入 M3U 播放列表使用。
//
// 路径可带库限定前缀（"库ID:路径"）以精确定位；
// 不带前缀时跨全部库匹配，用于兼容旧版单库时代生成的播放列表。
// 匹配用 collate nocase：不同文件系统对大小写的处理不一致。
func (r *mediaFileRepository) FindByPaths(paths []string) (model.MediaFiles, error) {
	query := Or{}

	for _, path := range paths {
		parts := strings.SplitN(path, ":", 2)
		if len(parts) == 2 {
			// Library-qualified path: "libraryID:path"
			libraryID, err := strconv.Atoi(parts[0])
			if err != nil {
				// Invalid format, skip
				// 前缀不是数字说明不是库限定格式，跳过该项
				continue
			}
			relativePath := parts[1]
			query = append(query, And{
				Eq{"path collate nocase": relativePath},
				Eq{"library_id": libraryID},
			})
		} else {
			// Unqualified path: search across all libraries
			query = append(query, Eq{"path collate nocase": path})
		}
	}

	// 无有效路径时直接返回空，避免生成空的 OR 条件（会匹配全表）
	if len(query) == 0 {
		return model.MediaFiles{}, nil
	}

	sel := r.newSelect().Columns("*").Where(query)
	var res dbMediaFiles
	if err := r.queryAll(sel, &res); err != nil {
		return nil, err
	}

	return res.toModels(), nil
}

// Delete 删除曲目。
func (r *mediaFileRepository) Delete(id string) error {
	return r.delete(Eq{"id": id})
}

// DeleteAllMissing 清除所有标记为缺失的曲目，仅管理员可执行。
// 这会连带丢失这些曲目的用户数据，属破坏性操作，故限制权限。
func (r *mediaFileRepository) DeleteAllMissing() (int64, error) {
	user := loggedUser(r.ctx)
	if !user.IsAdmin {
		return 0, rest.ErrPermissionDenied
	}
	del := Delete(r.tableName).Where(Eq{"missing": true})
	return r.executeSQL(del)
}

// DeleteMissing 按 ID 删除指定的缺失曲目，仅管理员可执行。
// 条件中保留 missing = true，防止误删仍然存在的文件。
func (r *mediaFileRepository) DeleteMissing(ids []string) error {
	user := loggedUser(r.ctx)
	if !user.IsAdmin {
		return rest.ErrPermissionDenied
	}
	return r.delete(
		And{
			Eq{"missing": true},
			Eq{"id": ids},
		},
	)
}

// MarkMissing 批量标记曲目的缺失状态。
// 分块处理是必需的：SQLite 对单条语句的参数个数有上限，
// 大批量 IN 会直接报错。
func (r *mediaFileRepository) MarkMissing(missing bool, mfs ...*model.MediaFile) error {
	ids := slice.SeqFunc(mfs, func(m *model.MediaFile) string { return m.ID })
	for chunk := range slice.CollectChunks(ids, 200) {
		upd := Update(r.tableName).
			Set("missing", missing).
			Set("updated_at", time.Now()).
			Where(Eq{"id": chunk})
		c, err := r.executeSQL(upd)
		if err != nil || c == 0 {
			log.Error(r.ctx, "Error setting mediafile missing flag", "ids", chunk, err)
			return err
		}
		log.Debug(r.ctx, "Marked missing mediafiles", "total", c, "ids", chunk)
	}
	return nil
}

// MarkMissingByFolder 按文件夹批量标记缺失状态，用于整个目录消失或恢复的场景。
// 条件中加上 missing = !missing，跳过状态已正确的行，
// 避免无谓地刷新 updated_at 触发下游重新处理。
func (r *mediaFileRepository) MarkMissingByFolder(missing bool, folderIDs ...string) error {
	for chunk := range slices.Chunk(folderIDs, 200) {
		upd := Update(r.tableName).
			Set("missing", missing).
			Set("updated_at", time.Now()).
			Where(And{
				Eq{"folder_id": chunk},
				Eq{"missing": !missing},
			})
		c, err := r.executeSQL(upd)
		if err != nil {
			log.Error(r.ctx, "Error setting mediafile missing flag", "folderIDs", chunk, err)
			return err
		}
		log.Debug(r.ctx, "Marked missing mediafiles from missing folders", "total", c, "folders", chunk)
	}
	return nil
}

// GetMissingAndMatching returns all mediafiles that are missing and their potential matches (comparing PIDs)
// that were added/updated after the last scan started. The result is ordered by PID.
// It does not need to load bookmarks, annotations and participants, as they are not used by the scanner.
//
// GetMissingAndMatching 查出缺失曲目及其可能的「新位置」候选，用于文件移动检测。
//
// 原理：文件被移动后路径变了，但 PID 基于内容特征保持不变。
// 因此先用子查询取出该库中所有缺失曲目的 PID，
// 再捞出具有相同 PID 的全部记录——既包括缺失的旧记录，
// 也包括本轮扫描新建的记录（created_at 晚于扫描开始时间）。
//
// 按 PID 排序，使同一 PID 的新旧记录在遍历时相邻，
// 调用方可顺序配对而无需在内存中建索引。
// 不加载标注、书签与参与者：扫描器用不到，省去多次 JOIN。
func (r *mediaFileRepository) GetMissingAndMatching(libId int) (model.MediaFileCursor, error) {
	subQ := r.newSelect().Columns("pid").
		Where(And{
			Eq{"media_file.missing": true},
			Eq{"library_id": libId},
		})
	subQText, subQArgs, err := subQ.PlaceholderFormat(Question).ToSql()
	if err != nil {
		return nil, err
	}
	sel := r.newSelect().Columns("media_file.*", "library.path as library_path", "library.name as library_name").
		LeftJoin("library on media_file.library_id = library.id").
		Where("pid in ("+subQText+")", subQArgs...).
		Where(Or{
			Eq{"missing": true},
			ConcatExpr("media_file.created_at > library.last_scan_started_at"),
		}).
		OrderBy("pid")
	cursor, err := queryWithStableResults[dbMediaFile](r.sqlRepository, sel)
	if err != nil {
		return nil, err
	}
	return func(yield func(model.MediaFile, error) bool) {
		for m, err := range cursor {
			if !yield(*m.MediaFile, err) || err != nil {
				return
			}
		}
	}, nil
}

// FindRecentFilesByMBZTrackID finds recently added files by MusicBrainz Track ID in other libraries
// FindRecentFilesByMBZTrackID 在其他音乐库中按 MusicBrainz 音轨 ID 查找近期新增的同一曲目，
// 用于识别「文件被移动到另一个库」的情况，从而迁移用户数据。
//
// 限定条件说明：排除源库自身、要求 MBID 非空（空值会误匹配所有无 MBID 的文件）、
// 扩展名一致、且必须是未缺失的新增文件。按新增时间倒序，优先匹配最近的。
func (r *mediaFileRepository) FindRecentFilesByMBZTrackID(missing model.MediaFile, since time.Time) (model.MediaFiles, error) {
	sel := r.selectMediaFile().Where(And{
		NotEq{"media_file.library_id": missing.LibraryID},
		Eq{"media_file.mbz_release_track_id": missing.MbzReleaseTrackID},
		NotEq{"media_file.mbz_release_track_id": ""}, // Exclude empty MBZ Track IDs
		Eq{"media_file.suffix": missing.Suffix},
		Gt{"media_file.created_at": since},
		Eq{"media_file.missing": false},
	}).OrderBy("media_file.created_at DESC")

	var res dbMediaFiles
	err := r.queryAll(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.toModels(), nil
}

// FindRecentFilesByProperties finds recently added files by intrinsic properties in other libraries
// FindRecentFilesByProperties 在无 MusicBrainz ID 时，改用文件固有属性组合来匹配跨库移动的曲目。
//
// 标题、大小、扩展名、碟号、音轨号、专辑名全部相同才认定为同一文件——
// 单个属性都不足以区分，组合起来误判概率极低。
// 这里要求 MBID 为空，与上一个方法互斥，避免同一文件被两种策略重复处理。
func (r *mediaFileRepository) FindRecentFilesByProperties(missing model.MediaFile, since time.Time) (model.MediaFiles, error) {
	sel := r.selectMediaFile().Where(And{
		NotEq{"media_file.library_id": missing.LibraryID},
		Eq{"media_file.title": missing.Title},
		Eq{"media_file.size": missing.Size},
		Eq{"media_file.suffix": missing.Suffix},
		Eq{"media_file.disc_number": missing.DiscNumber},
		Eq{"media_file.track_number": missing.TrackNumber},
		Eq{"media_file.album": missing.Album},
		Eq{"media_file.mbz_release_track_id": ""}, // Exclude files with MBZ Track ID
		Gt{"media_file.created_at": since},
		Eq{"media_file.missing": false},
	}).OrderBy("media_file.created_at DESC")

	var res dbMediaFiles
	err := r.queryAll(sel, &res)
	if err != nil {
		return nil, err
	}
	return res.toModels(), nil
}

// Search 搜索曲目。关键词是合法 UUID 时按 MusicBrainz ID 精确查找，
// 否则走全文搜索。
func (r *mediaFileRepository) Search(q string, offset int, size int, options ...model.QueryOptions) (model.MediaFiles, error) {
	var res dbMediaFiles
	if uuid.Validate(q) == nil {
		err := r.searchByMBID(r.selectMediaFile(options...), q, []string{"mbz_recording_id", "mbz_release_track_id"}, &res)
		if err != nil {
			return nil, fmt.Errorf("searching media_file by MBID %q: %w", q, err)
		}
	} else {
		err := r.doSearch(r.selectMediaFile(options...), q, offset, size, &res, "media_file.rowid", "title")
		if err != nil {
			return nil, fmt.Errorf("searching media_file by query %q: %w", q, err)
		}
	}
	return res.toModels(), nil
}

// 以下实现 model.ResourceRepository，供通用 REST 层调用。

func (r *mediaFileRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *mediaFileRepository) Read(id string) (interface{}, error) {
	return r.Get(id)
}

func (r *mediaFileRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *mediaFileRepository) EntityName() string {
	return "mediafile"
}

func (r *mediaFileRepository) NewInstance() interface{} {
	return &model.MediaFile{}
}

// 编译期接口实现检查
var _ model.MediaFileRepository = (*mediaFileRepository)(nil)
var _ model.ResourceRepository = (*mediaFileRepository)(nil)
