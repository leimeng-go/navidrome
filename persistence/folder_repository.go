package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

// folderRepository 是文件夹仓储。
// 文件夹表镜像磁盘目录结构，是扫描器判断「哪些目录需要重扫」的依据。
type folderRepository struct {
	sqlRepository
}

// dbFolder 是 model.Folder 的数据库映射层。
// ImageFiles（封面等图片文件名列表）以 JSON 字符串存储。
type dbFolder struct {
	*model.Folder `structs:",flatten"`
	ImageFiles    string `structs:"-" json:"-"`
}

// PostScan 解析图片文件列表。
func (f *dbFolder) PostScan() error {
	var err error
	if f.ImageFiles != "" {
		if err = json.Unmarshal([]byte(f.ImageFiles), &f.Folder.ImageFiles); err != nil {
			return fmt.Errorf("parsing folder image files from db: %w", err)
		}
	}
	return nil
}

// PostMapArgs 序列化图片文件列表。
// nil 写成 "[]" 而非 NULL，purgeEmpty 依赖该值判断空目录。
func (f *dbFolder) PostMapArgs(args map[string]any) error {
	if f.Folder.ImageFiles == nil {
		args["image_files"] = "[]"
	} else {
		imgFiles, err := json.Marshal(f.Folder.ImageFiles)
		if err != nil {
			return fmt.Errorf("marshalling image files: %w", err)
		}
		args["image_files"] = string(imgFiles)
	}
	return nil
}

type dbFolders []dbFolder

func (fs dbFolders) toModels() []model.Folder {
	return slice.Map(fs, func(f dbFolder) model.Folder { return *f.Folder })
}

// newFolderRepository 创建文件夹仓储（仅供包内使用）。
func newFolderRepository(ctx context.Context, db dbx.Builder) model.FolderRepository {
	r := &folderRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "folder"
	return r
}

// selectFolder 构建标准查询，关联库路径以便还原绝对路径，并施加库权限过滤。
func (r folderRepository) selectFolder(options ...model.QueryOptions) SelectBuilder {
	sql := r.newSelect(options...).Columns("folder.*", "library.path as library_path").
		Join("library on library.id = folder.library_id")
	return r.applyLibraryFilter(sql)
}

// Get 按 ID 读取文件夹。
func (r folderRepository) Get(id string) (*model.Folder, error) {
	sq := r.selectFolder().Where(Eq{"folder.id": id})
	var res dbFolder
	err := r.queryOne(sq, &res)
	return res.Folder, err
}

// GetByPath 按「库 + 相对路径」读取文件夹。
// 文件夹 ID 由库与路径确定性推导，故可直接算出 ID 再查。
func (r folderRepository) GetByPath(lib model.Library, path string) (*model.Folder, error) {
	id := model.NewFolder(lib, path).ID
	return r.Get(id)
}

// GetAll 查询文件夹列表。
func (r folderRepository) GetAll(opt ...model.QueryOptions) ([]model.Folder, error) {
	sq := r.selectFolder(opt...)
	var res dbFolders
	err := r.queryAll(sq, &res)
	return res.toModels(), err
}

// CountAll 统计当前用户可见的文件夹数。
func (r folderRepository) CountAll(opt ...model.QueryOptions) (int64, error) {
	query := r.newSelect(opt...).Columns("count(*)")
	query = r.applyLibraryFilter(query)
	return r.count(query)
}

// GetFolderUpdateInfo 返回文件夹的更新时间与内容哈希，
// 供扫描器比对以决定是否需要重新读取该目录。
//
// targetPaths 为空或包含根路径时返回全库信息；
// 否则只返回指定路径及其所有子目录的信息（增量扫描场景）。
func (r folderRepository) GetFolderUpdateInfo(lib model.Library, targetPaths ...string) (map[string]model.FolderUpdateInfo, error) {
	// If no specific paths, return all folders in the library
	if len(targetPaths) == 0 {
		return r.getFolderUpdateInfoAll(lib)
	}

	// Check if any path is root (return all folders)
	for _, targetPath := range targetPaths {
		if targetPath == "" || targetPath == "." {
			return r.getFolderUpdateInfoAll(lib)
		}
	}

	// Process paths in batches to avoid SQLite's expression tree depth limit (max 1000).
	// Each path generates ~3 conditions, so batch size of 100 keeps us well under the limit.
	const batchSize = 100
	result := make(map[string]model.FolderUpdateInfo)

	for batch := range slices.Chunk(targetPaths, batchSize) {
		batchResult, err := r.getFolderUpdateInfoBatch(lib, batch)
		if err != nil {
			return nil, err
		}
		for id, info := range batchResult {
			result[id] = info
		}
	}

	return result, nil
}

// getFolderUpdateInfoAll returns update info for all non-missing folders in the library
func (r folderRepository) getFolderUpdateInfoAll(lib model.Library) (map[string]model.FolderUpdateInfo, error) {
	where := And{
		Eq{"library_id": lib.ID},
		Eq{"missing": false},
	}
	return r.queryFolderUpdateInfo(where)
}

// getFolderUpdateInfoBatch returns update info for a batch of target paths and their descendants
// getFolderUpdateInfoBatch 查询一批目标路径及其子孙目录的更新信息。
//
// 每个路径产生三个条件：目录自身按 ID 精确匹配，
// 子目录按 path 相等或以 "path/" 开头匹配（Folder.Path 存的是父目录路径，
// 故直接子目录的 path 恰好等于目标路径）。
func (r folderRepository) getFolderUpdateInfoBatch(lib model.Library, targetPaths []string) (map[string]model.FolderUpdateInfo, error) {
	where := And{
		Eq{"library_id": lib.ID},
		Eq{"missing": false},
	}

	// Collect folder IDs for exact target folders and path conditions for descendants
	folderIDs := make([]string, 0, len(targetPaths))
	pathConditions := make(Or, 0, len(targetPaths)*2)

	for _, targetPath := range targetPaths {
		// Clean the path to normalize it. Paths stored in the folder table do not have leading/trailing slashes.
		cleanPath := strings.TrimPrefix(targetPath, string(os.PathSeparator))
		cleanPath = filepath.Clean(cleanPath)

		// Include the target folder itself by ID
		folderIDs = append(folderIDs, model.FolderID(lib, cleanPath))

		// Include all descendants: folders whose path field equals or starts with the target path
		// Note: Folder.Path is the directory path, so children have path = targetPath
		pathConditions = append(pathConditions, Eq{"path": cleanPath})
		pathConditions = append(pathConditions, Like{"path": cleanPath + "/%"})
	}

	// Combine conditions: exact folder IDs OR descendant path patterns
	if len(folderIDs) > 0 {
		where = append(where, Or{Eq{"id": folderIDs}, pathConditions})
	} else if len(pathConditions) > 0 {
		where = append(where, pathConditions)
	}

	return r.queryFolderUpdateInfo(where)
}

// queryFolderUpdateInfo executes the query and returns the result map
func (r folderRepository) queryFolderUpdateInfo(where And) (map[string]model.FolderUpdateInfo, error) {
	sq := r.newSelect().Columns("id", "updated_at", "hash").Where(where)
	var res []struct {
		ID        string
		UpdatedAt time.Time
		Hash      string
	}
	err := r.queryAll(sq, &res)
	if err != nil {
		return nil, err
	}
	m := make(map[string]model.FolderUpdateInfo, len(res))
	for _, f := range res {
		m[f.ID] = model.FolderUpdateInfo{UpdatedAt: f.UpdatedAt, Hash: f.Hash}
	}
	return m, nil
}

// Put 写入文件夹记录。
func (r folderRepository) Put(f *model.Folder) error {
	dbf := dbFolder{Folder: f}
	_, err := r.put(dbf.ID, &dbf)
	return err
}

// MarkMissing 批量标记文件夹缺失/恢复。
// 每 200 个一批，规避 SQLite 参数数量上限。
func (r folderRepository) MarkMissing(missing bool, ids ...string) error {
	log.Debug(r.ctx, "Marking folders as missing", "ids", ids, "missing", missing)
	for chunk := range slices.Chunk(ids, 200) {
		sq := Update(r.tableName).
			Set("missing", missing).
			Set("updated_at", time.Now()).
			Where(Eq{"id": chunk})
		_, err := r.executeSQL(sq)
		if err != nil {
			return err
		}
	}
	return nil
}

// GetTouchedWithPlaylists 返回本轮扫描中有变动且含播放列表文件的目录，
// 供扫描器的播放列表导入阶段使用。
// 「有变动」的判据是目录更新时间晚于所属库的上次扫描时间。
// 返回游标而非切片，以避免一次性加载大量数据。
func (r folderRepository) GetTouchedWithPlaylists() (model.FolderCursor, error) {
	query := r.selectFolder().Where(And{
		Eq{"missing": false},
		Gt{"num_playlists": 0},
		ConcatExpr("folder.updated_at > library.last_scan_at"),
	})
	cursor, err := queryWithStableResults[dbFolder](r.sqlRepository, query)
	if err != nil {
		return nil, err
	}
	return func(yield func(model.Folder, error) bool) {
		for f, err := range cursor {
			if !yield(*f.Folder, err) || err != nil {
				return
			}
		}
	}, nil
}

// purgeEmpty 删除真正为空的目录记录：
// 无音频、无播放列表、无图片，且既不是他人的父目录、也没有曲目挂在其下。
// 五个条件缺一不可，否则会误删仍在使用的中间层目录。
func (r folderRepository) purgeEmpty(libraryIDs ...int) error {
	sq := Delete(r.tableName).Where(And{
		Eq{"num_audio_files": 0},
		Eq{"num_playlists": 0},
		Eq{"image_files": "[]"},
		ConcatExpr("id not in (select parent_id from folder)"),
		ConcatExpr("id not in (select folder_id from media_file)"),
	})
	// If libraryIDs are specified, only purge folders from those libraries
	if len(libraryIDs) > 0 {
		sq = sq.Where(Eq{"library_id": libraryIDs})
	}
	c, err := r.executeSQL(sq)
	if err != nil {
		return fmt.Errorf("purging empty folders: %w", err)
	}
	if c > 0 {
		log.Debug(r.ctx, "Purging empty folders", "totalDeleted", c)
	}
	return nil
}

var _ model.FolderRepository = (*folderRepository)(nil)
