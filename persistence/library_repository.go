package persistence

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/run"
	"github.com/pocketbase/dbx"
)

// libraryRepository 是音乐库仓储。
type libraryRepository struct {
	sqlRepository
}

// 库 ID 到路径的进程级缓存。
// 路径在把数据库中的相对路径还原为绝对路径时被频繁使用，
// 而库数量少、极少变动，故常驻内存。
var (
	libCache = map[int]string{}
	libLock  sync.RWMutex
)

// NewLibraryRepository 创建音乐库仓储。
func NewLibraryRepository(ctx context.Context, db dbx.Builder) model.LibraryRepository {
	r := &libraryRepository{}
	r.ctx = ctx
	r.db = db
	r.registerModel(&model.Library{}, nil)
	return r
}

// Get 按 ID 读取音乐库。
func (r *libraryRepository) Get(id int) (*model.Library, error) {
	sq := r.newSelect().Columns("*").Where(Eq{"id": id})
	var res model.Library
	err := r.queryOne(sq, &res)
	return &res, err
}

// GetPath 返回音乐库的根路径，优先走缓存。
// 未命中时一次性加载全部库刷新缓存，而不是只查这一条，
// 因为调用通常成批发生，整体加载更划算。
func (r *libraryRepository) GetPath(id int) (string, error) {
	l := func() string {
		libLock.RLock()
		defer libLock.RUnlock()
		if l, ok := libCache[id]; ok {
			return l
		}
		return ""
	}()
	if l != "" {
		return l, nil
	}

	libLock.Lock()
	defer libLock.Unlock()
	libs, err := r.GetAll()
	if err != nil {
		log.Error(r.ctx, "Error loading libraries from DB", err)
		return "", err
	}
	for _, l := range libs {
		libCache[l.ID] = l.Path
	}
	if l, ok := libCache[id]; ok {
		return l, nil
	} else {
		return "", model.ErrNotFound
	}
}

// Put 新增或更新音乐库。
//
// 默认库（ID=1）的路径不允许变更：它由配置项 MusicFolder 决定，
// 改路径会让已入库的相对路径全部失效。
//
// ID 为 0 时走自增插入；否则先尝试 UPDATE，
// 影响行数为 0 再插入——用于支持指定 ID 建库（如迁移场景）。
//
// 每次写入后都为所有管理员补齐授权，保证新建的库对管理员立即可见。
func (r *libraryRepository) Put(l *model.Library) error {
	if l.ID == model.DefaultLibraryID {
		currentLib, err := r.Get(1)
		// if we are creating it, it's ok.
		if err == nil { // it exists, so we are updating it
			if currentLib.Path != l.Path {
				return fmt.Errorf("%w: path for library with ID 1 cannot be changed", model.ErrValidation)
			}
		}
	}

	var err error
	l.UpdatedAt = time.Now()
	if l.ID == 0 {
		// Insert with autoassigned ID
		l.CreatedAt = time.Now()
		err = r.db.Model(l).Insert()
	} else {
		// Try to update first
		cols := map[string]any{
			"name":              l.Name,
			"path":              l.Path,
			"remote_path":       l.RemotePath,
			"default_new_users": l.DefaultNewUsers,
			"updated_at":        l.UpdatedAt,
		}
		sq := Update(r.tableName).SetMap(cols).Where(Eq{"id": l.ID})
		rowsAffected, updateErr := r.executeSQL(sq)
		if updateErr != nil {
			return updateErr
		}

		// If no rows were affected, the record doesn't exist, so insert it
		if rowsAffected == 0 {
			l.CreatedAt = time.Now()
			l.UpdatedAt = time.Now()
			err = r.db.Model(l).Insert()
		}
	}
	if err != nil {
		return err
	}

	// Auto-assign all libraries to all admin users
	sql := Expr(`
INSERT INTO user_library (user_id, library_id)
SELECT u.id, l.id
FROM user u
CROSS JOIN library l
WHERE u.is_admin = true
ON CONFLICT (user_id, library_id) DO NOTHING;`,
	)
	if _, err = r.executeSQL(sql); err != nil {
		return fmt.Errorf("failed to assign library to admin users: %w", err)
	}

	libLock.Lock()
	defer libLock.Unlock()
	libCache[l.ID] = l.Path
	return nil
}

// TODO Remove this method when we have a proper UI to add libraries
// This is a temporary method to store the music folder path from the config in the DB
// StoreMusicFolder 把配置中的 MusicFolder 同步到默认库，
// 是尚无多库管理界面时的过渡手段。
func (r *libraryRepository) StoreMusicFolder() error {
	sq := Update(r.tableName).Set("path", conf.Server.MusicFolder).
		Set("updated_at", time.Now()).
		Where(Eq{"id": model.DefaultLibraryID})
	_, err := r.executeSQL(sq)
	if err != nil {
		libLock.Lock()
		defer libLock.Unlock()
		libCache[model.DefaultLibraryID] = conf.Server.MusicFolder
	}
	return err
}

// AddArtist 建立艺人与音乐库的归属关系，重复调用幂等。
func (r *libraryRepository) AddArtist(id int, artistID string) error {
	sq := Insert("library_artist").Columns("library_id", "artist_id").Values(id, artistID).
		Suffix(`on conflict(library_id, artist_id) do nothing`)
	_, err := r.executeSQL(sq)
	if err != nil {
		return err
	}
	return nil
}

// ScanBegin 标记扫描开始。last_scan_started_at 非零即表示扫描进行中。
func (r *libraryRepository) ScanBegin(id int, fullScan bool) error {
	sq := Update(r.tableName).
		Set("last_scan_started_at", time.Now()).
		Set("full_scan_in_progress", fullScan).
		Where(Eq{"id": id})
	_, err := r.executeSQL(sq)
	return err
}

// ScanEnd 标记扫描结束，并把开始时间清零以解除「进行中」状态。
func (r *libraryRepository) ScanEnd(id int) error {
	sq := Update(r.tableName).
		Set("last_scan_at", time.Now()).
		Set("full_scan_in_progress", false).
		Set("last_scan_started_at", time.Time{}).
		Where(Eq{"id": id})
	_, err := r.executeSQL(sq)
	if err != nil {
		return err
	}
	// https://www.sqlite.org/pragma.html#pragma_optimize
	// Use mask 0x10000 to check table sizes without running ANALYZE
	// Running ANALYZE can cause query planner issues with expression-based collation indexes
	// 掩码 0x10000 只让 SQLite 检查表大小而不执行 ANALYZE：
	// 完整 ANALYZE 会干扰基于表达式排序规则的索引，导致查询计划变差
	if conf.Server.DevOptimizeDB {
		_, err = r.executeSQL(Expr("PRAGMA optimize=0x10000;"))
	}
	return err
}

// ScanInProgress 判断是否有任一音乐库正在扫描。
func (r *libraryRepository) ScanInProgress() (bool, error) {
	query := r.newSelect().Where(NotEq{"last_scan_started_at": time.Time{}})
	count, err := r.count(query)
	return count > 0, err
}

// RefreshStats 重算音乐库的各项统计。
//
// 八项统计彼此独立，用 run.Parallel 并发查询以缩短总耗时。
// 除「缺失文件数」外均排除 missing 记录；
// 文件夹数只统计含音频的目录，避免把空目录计入。
func (r *libraryRepository) RefreshStats(id int) error {
	var songsRes, albumsRes, artistsRes, foldersRes, filesRes, missingRes struct{ Count int64 }
	var sizeRes struct{ Sum int64 }
	var durationRes struct{ Sum float64 }

	err := run.Parallel(
		func() error {
			return r.queryOne(Select("count(*) as count").From("media_file").Where(Eq{"library_id": id, "missing": false}), &songsRes)
		},
		func() error {
			return r.queryOne(Select("count(*) as count").From("album").Where(Eq{"library_id": id, "missing": false}), &albumsRes)
		},
		func() error {
			return r.queryOne(Select("count(*) as count").From("library_artist la").
				Join("artist a on la.artist_id = a.id").
				Where(Eq{"la.library_id": id, "a.missing": false}), &artistsRes)
		},
		func() error {
			return r.queryOne(Select("count(*) as count").From("folder").
				Where(And{
					Eq{"library_id": id, "missing": false},
					Gt{"num_audio_files": 0},
				}), &foldersRes)
		},
		func() error {
			return r.queryOne(Select("ifnull(sum(num_audio_files + num_playlists + json_array_length(image_files)),0) as count").
				From("folder").Where(Eq{"library_id": id, "missing": false}), &filesRes)
		},
		func() error {
			return r.queryOne(Select("count(*) as count").From("media_file").Where(Eq{"library_id": id, "missing": true}), &missingRes)
		},
		func() error {
			return r.queryOne(Select("ifnull(sum(size),0) as sum").From("album").Where(Eq{"library_id": id, "missing": false}), &sizeRes)
		},
		func() error {
			return r.queryOne(Select("ifnull(sum(duration),0) as sum").From("album").Where(Eq{"library_id": id, "missing": false}), &durationRes)
		},
	)()
	if err != nil {
		return err
	}

	sq := Update(r.tableName).
		Set("total_songs", songsRes.Count).
		Set("total_albums", albumsRes.Count).
		Set("total_artists", artistsRes.Count).
		Set("total_folders", foldersRes.Count).
		Set("total_files", filesRes.Count).
		Set("total_missing_files", missingRes.Count).
		Set("total_size", sizeRes.Sum).
		Set("total_duration", durationRes.Sum).
		Set("updated_at", time.Now()).
		Where(Eq{"id": id})
	_, err = r.executeSQL(sq)
	return err
}

// Delete 删除音乐库，仅管理员可操作。
// 默认库（ID=1）不可删除——系统假定它始终存在。
func (r *libraryRepository) Delete(id int) error {
	if !loggedUser(r.ctx).IsAdmin {
		return model.ErrNotAuthorized
	}
	if id == 1 {
		return fmt.Errorf("%w: library with ID 1 cannot be deleted", model.ErrValidation)
	}

	err := r.delete(Eq{"id": id})
	if err != nil {
		return err
	}

	// Clear cache entry for this library only if DB operation was successful
	libLock.Lock()
	defer libLock.Unlock()
	delete(libCache, id)

	return nil
}

// GetAll 返回全部音乐库。
func (r *libraryRepository) GetAll(ops ...model.QueryOptions) (model.Libraries, error) {
	sq := r.newSelect(ops...).Columns("*")
	res := model.Libraries{}
	err := r.queryAll(sq, &res)
	return res, err
}

// CountAll 统计音乐库数量。
func (r *libraryRepository) CountAll(ops ...model.QueryOptions) (int64, error) {
	sq := r.newSelect(ops...)
	return r.count(sq)
}

// User-library association methods
// 以下为音乐库与用户的授权关系查询。

// GetUsersWithLibraryAccess 返回有权访问该库的所有用户。
func (r *libraryRepository) GetUsersWithLibraryAccess(libraryID int) (model.Users, error) {
	sel := Select("u.*").
		From("user u").
		Join("user_library ul ON u.id = ul.user_id").
		Where(Eq{"ul.library_id": libraryID}).
		OrderBy("u.name")

	var res model.Users
	err := r.queryAll(sel, &res)
	return res, err
}

// REST interface methods
// 以下实现 rest 接口。音乐库 ID 是整数，
// 而 REST 层统一用字符串，故需转换并对非法值返回 404。

func (r *libraryRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.CountAll(r.parseRestOptions(r.ctx, options...))
}

func (r *libraryRepository) Read(id string) (interface{}, error) {
	idInt, err := strconv.Atoi(id)
	if err != nil {
		log.Trace(r.ctx, "invalid library id: %s", id, err)
		return nil, rest.ErrNotFound
	}
	return r.Get(idInt)
}

func (r *libraryRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *libraryRepository) EntityName() string {
	return "library"
}

func (r *libraryRepository) NewInstance() interface{} {
	return &model.Library{}
}

// Save 新建音乐库，清空 ID 以走自增插入，避免客户端指定 ID。
func (r *libraryRepository) Save(entity interface{}) (string, error) {
	lib := entity.(*model.Library)
	lib.ID = 0 // Reset ID to ensure we create a new library
	err := r.Put(lib)
	if err != nil {
		return "", err
	}
	return strconv.Itoa(lib.ID), nil
}

// Update 更新音乐库。cols 被忽略：Put 内部固定更新一组列。
func (r *libraryRepository) Update(id string, entity interface{}, cols ...string) error {
	lib := entity.(*model.Library)
	idInt, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid library ID: %s", id)
	}

	lib.ID = idInt
	return r.Put(lib)
}

var _ model.LibraryRepository = (*libraryRepository)(nil)
var _ rest.Repository = (*libraryRepository)(nil)
