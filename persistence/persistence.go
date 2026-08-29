// Package persistence 是数据访问层，基于 SQLite 实现 model 层定义的各仓储接口。
//
// 组织方式：SQLStore 作为 model.DataStore 的实现，是所有仓储的工厂；
// 每个仓储内嵌 sqlRepository 复用通用的查询构建与增删改能力。
// 仓储按请求创建（携带 context），因此可从上下文获取用户身份做权限过滤。
package persistence

import (
	"context"
	"database/sql"
	"reflect"
	"time"

	"github.com/navidrome/navidrome/db"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/run"
	"github.com/pocketbase/dbx"
)

// SQLStore 实现 model.DataStore，作为各仓储的工厂。
// db 字段既可以是连接池（*dbx.DB），也可以是事务（*dbx.Tx），
// 从而让同一套仓储代码在事务内外都能工作。
type SQLStore struct {
	db dbx.Builder
}

// New 基于已有的数据库连接创建 DataStore。
func New(conn *sql.DB) model.DataStore {
	return &SQLStore{db: dbx.NewFromDB(conn, db.Driver)}
}

// 以下是各仓储的工厂方法。每次调用都新建实例并绑定当前 context，
// 使仓储能读取到请求级信息（用户、客户端等）用于权限与行为控制。
func (s *SQLStore) Album(ctx context.Context) model.AlbumRepository {
	return NewAlbumRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Artist(ctx context.Context) model.ArtistRepository {
	return NewArtistRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) MediaFile(ctx context.Context) model.MediaFileRepository {
	return NewMediaFileRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Library(ctx context.Context) model.LibraryRepository {
	return NewLibraryRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Folder(ctx context.Context) model.FolderRepository {
	return newFolderRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Genre(ctx context.Context) model.GenreRepository {
	return NewGenreRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Tag(ctx context.Context) model.TagRepository {
	return NewTagRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) PlayQueue(ctx context.Context) model.PlayQueueRepository {
	return NewPlayQueueRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Playlist(ctx context.Context) model.PlaylistRepository {
	return NewPlaylistRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Property(ctx context.Context) model.PropertyRepository {
	return NewPropertyRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Radio(ctx context.Context) model.RadioRepository {
	return NewRadioRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) UserProps(ctx context.Context) model.UserPropsRepository {
	return NewUserPropsRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Share(ctx context.Context) model.ShareRepository {
	return NewShareRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) User(ctx context.Context) model.UserRepository {
	return NewUserRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Transcoding(ctx context.Context) model.TranscodingRepository {
	return NewTranscodingRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Player(ctx context.Context) model.PlayerRepository {
	return NewPlayerRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) ScrobbleBuffer(ctx context.Context) model.ScrobbleBufferRepository {
	return NewScrobbleBufferRepository(ctx, s.getDBXBuilder())
}

func (s *SQLStore) Scrobble(ctx context.Context) model.ScrobbleRepository {
	return NewScrobbleRepository(ctx, s.getDBXBuilder())
}

// Resource 按模型类型返回对应的通用资源仓储，供 REST API 层统一处理增删改查。
// 未实现的模型记录错误并返回 nil——这属于编码错误，应在开发期暴露。
func (s *SQLStore) Resource(ctx context.Context, m interface{}) model.ResourceRepository {
	switch m.(type) {
	case model.User:
		return s.User(ctx).(model.ResourceRepository)
	case model.Transcoding:
		return s.Transcoding(ctx).(model.ResourceRepository)
	case model.Player:
		return s.Player(ctx).(model.ResourceRepository)
	case model.Artist:
		return s.Artist(ctx).(model.ResourceRepository)
	case model.Album:
		return s.Album(ctx).(model.ResourceRepository)
	case model.MediaFile:
		return s.MediaFile(ctx).(model.ResourceRepository)
	case model.Genre:
		return s.Genre(ctx).(model.ResourceRepository)
	case model.Playlist:
		return s.Playlist(ctx).(model.ResourceRepository)
	case model.Radio:
		return s.Radio(ctx).(model.ResourceRepository)
	case model.Share:
		return s.Share(ctx).(model.ResourceRepository)
	case model.Tag:
		return s.Tag(ctx).(model.ResourceRepository)
	}
	log.Error("Resource not implemented", "model", reflect.TypeOf(m).Name())
	return nil
}

// WithTx 在事务中执行 block。
//
// 支持嵌套调用：若当前 SQLStore 已绑定事务（db 不是 *dbx.DB），
// 则新开一个独立连接的事务；否则在当前连接上开启事务。
// block 中拿到的是绑定该事务的新 SQLStore，
// 因此其创建的所有仓储都自动参与同一事务。
//
// scope 仅用于日志标注，便于排查事务耗时与嵌套关系。
func (s *SQLStore) WithTx(block func(tx model.DataStore) error, scope ...string) error {
	var msg string
	if len(scope) > 0 {
		msg = scope[0]
	}
	start := time.Now()
	conn, inTx := s.db.(*dbx.DB)
	if !inTx {
		log.Trace("Nested Transaction started", "scope", msg)
		conn = dbx.NewFromDB(db.Db(), db.Driver)
	} else {
		log.Trace("Transaction started", "scope", msg)
	}
	return conn.Transactional(func(tx *dbx.Tx) error {
		newDb := &SQLStore{db: tx}
		err := block(newDb)
		if !inTx {
			log.Trace("Nested Transaction finished", "scope", msg, "elapsed", time.Since(start), err)
		} else {
			log.Trace("Transaction finished", "scope", msg, "elapsed", time.Since(start), err)
		}
		return err
	})
}

// WithTxImmediate 在「立即模式」事务中执行 block。
//
// SQLite 默认延迟获取写锁：事务开始时只拿读锁，遇到首个写操作才升级为写锁。
// 若此时另一事务已持有写锁，升级会直接失败（database is locked），
// 且不受 busy_timeout 保护，无法自动重试。
//
// 变通做法是在事务开头先做一次无关紧要的写入，
// 强制立刻取得写锁，从而让后续的锁等待走正常的超时重试路径。
func (s *SQLStore) WithTxImmediate(block func(tx model.DataStore) error, scope ...string) error {
	ctx := context.Background()
	return s.WithTx(func(tx model.DataStore) error {
		// Workaround to force the transaction to be upgraded to immediate mode to avoid deadlocks
		// See https://berthub.eu/articles/posts/a-brief-post-on-sqlite3-database-locked-despite-timeout/
		// 写入并随即删除一个临时标记，仅为触发写锁升级
		_ = tx.Property(ctx).Put("tmp_lock_flag", "")
		defer func() {
			_ = tx.Property(ctx).Delete("tmp_lock_flag")
		}()

		return block(tx)
	}, scope...)
}

// GC 清理数据库中的孤立与冗余数据。
//
// 各步骤有明确的先后依赖，故用 run.Sequentially 串行执行、任一失败即中止：
// 先清空专辑与艺人（依赖曲目已被删除），再清理文件夹，
// 然后清理指向已删实体的标注与书签，最后清理无引用的标签与孤立的播放列表曲目。
//
// libraryIDs 非空时只清理指定音乐库，用于单库扫描后的增量清理，
// 避免每次都全库扫描。部分步骤（如艺人）跨库共享，无法按库缩小范围。
func (s *SQLStore) GC(ctx context.Context, libraryIDs ...int) error {
	// trace 包装每个步骤以记录耗时，便于定位 GC 慢在哪一环
	trace := func(ctx context.Context, msg string, f func() error) func() error {
		return func() error {
			start := time.Now()
			err := f()
			log.Debug(ctx, "GC: "+msg, "elapsed", time.Since(start), err)
			return err
		}
	}

	// If libraryIDs are provided, scope operations to those libraries where possible
	scoped := len(libraryIDs) > 0
	if scoped {
		log.Debug(ctx, "GC: Running selective garbage collection", "libraryIDs", libraryIDs)
	}

	err := run.Sequentially(
		trace(ctx, "purge empty albums", func() error { return s.Album(ctx).(*albumRepository).purgeEmpty(libraryIDs...) }),
		trace(ctx, "purge empty artists", func() error { return s.Artist(ctx).(*artistRepository).purgeEmpty() }),
		trace(ctx, "mark missing artists", func() error { return s.Artist(ctx).(*artistRepository).markMissing() }),
		trace(ctx, "purge empty folders", func() error { return s.Folder(ctx).(*folderRepository).purgeEmpty(libraryIDs...) }),
		trace(ctx, "clean album annotations", func() error { return s.Album(ctx).(*albumRepository).cleanAnnotations() }),
		trace(ctx, "clean artist annotations", func() error { return s.Artist(ctx).(*artistRepository).cleanAnnotations() }),
		trace(ctx, "clean media file annotations", func() error { return s.MediaFile(ctx).(*mediaFileRepository).cleanAnnotations() }),
		trace(ctx, "clean media file bookmarks", func() error { return s.MediaFile(ctx).(*mediaFileRepository).cleanBookmarks() }),
		trace(ctx, "purge non used tags", func() error { return s.Tag(ctx).(*tagRepository).purgeUnused() }),
		trace(ctx, "remove orphan playlist tracks", func() error { return s.Playlist(ctx).(*playlistRepository).removeOrphans() }),
	)
	if err != nil {
		log.Error(ctx, "Error tidying up database", err)
	}
	return err
}

// getDBXBuilder 返回可用的查询构建器。
// s.db 为空时回退到全局连接池，使零值 SQLStore 也可用（主要便于测试）。
func (s *SQLStore) getDBXBuilder() dbx.Builder {
	if s.db == nil {
		return dbx.NewFromDB(db.Db(), db.Driver)
	}
	return s.db
}
