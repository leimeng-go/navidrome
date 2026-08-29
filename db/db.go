// Package db 负责 SQLite 连接的创建与数据库迁移。
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"runtime"

	"github.com/mattn/go-sqlite3"
	"github.com/navidrome/navidrome/conf"
	_ "github.com/navidrome/navidrome/db/migrations"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/utils/hasher"
	"github.com/navidrome/navidrome/utils/singleton"
	"github.com/pressly/goose/v3"
)

// 自定义驱动名：需要注册 SEEDEDRAND 等自定义函数，故不能直接用原生 sqlite3 驱动。
var (
	Dialect = "sqlite3"
	Driver  = Dialect + "_custom"
	Path    string
)

//go:embed migrations/*.sql
var embedMigrations embed.FS

const migrationsFolder = "migrations"

// Db 返回全局数据库连接池单例。
//
// 注册连接钩子以提供 SEEDEDRAND 函数，供「带种子的随机排序」使用——
// 这样分页取随机结果时各页之间顺序仍一致。
// 内存库需加 cache=shared，否则每个连接会各自持有一个独立的空库。
func Db() *sql.DB {
	return singleton.GetInstance(func() *sql.DB {
		sql.Register(Driver, &sqlite3.SQLiteDriver{
			ConnectHook: func(conn *sqlite3.SQLiteConn) error {
				return conn.RegisterFunc("SEEDEDRAND", hasher.HashFunc(), false)
			},
		})
		Path = conf.Server.DbPath
		if Path == ":memory:" {
			Path = "file::memory:?cache=shared&_foreign_keys=on"
			conf.Server.DbPath = Path
		}
		log.Debug("Opening DataBase", "dbPath", Path, "driver", Driver)
		db, err := sql.Open(Driver, Path)
		db.SetMaxOpenConns(max(4, runtime.NumCPU()))
		if err != nil {
			log.Fatal("Error opening database", err)
		}
		if conf.Server.DevOptimizeDB {
			_, err = db.Exec("PRAGMA optimize=0x10002")
			if err != nil {
				log.Error("Error applying PRAGMA optimize", err)
				return nil
			}
		}
		return db
	})
}

// Close 关闭数据库。
// 用 WithoutCancel 剥离取消信号：停机时 context 往往已被取消，
// 但收尾的优化与关闭操作仍需执行完。
func Close(ctx context.Context) {
	// Ignore cancellations when closing the DB
	ctx = context.WithoutCancel(ctx)

	// Run optimize before closing
	Optimize(ctx)

	log.Info(ctx, "Closing Database")
	err := Db().Close()
	if err != nil {
		log.Error(ctx, "Error closing Database", err)
	}
}

// Init 打开数据库并升级到最新 schema，返回关闭函数。
//
// 迁移期间临时关闭外键约束：部分迁移需重建表，
// 开着外键会因中间态的引用不一致而失败。
// 空库属于首次安装，迁移日志静默以免刷屏。
func Init(ctx context.Context) func() {
	db := Db()

	// Disable foreign_keys to allow re-creating tables in migrations
	_, err := db.ExecContext(ctx, "PRAGMA foreign_keys=off")
	defer func() {
		_, err := db.ExecContext(ctx, "PRAGMA foreign_keys=on")
		if err != nil {
			log.Error(ctx, "Error re-enabling foreign_keys", err)
		}
	}()
	if err != nil {
		log.Error(ctx, "Error disabling foreign_keys", err)
	}

	goose.SetBaseFS(embedMigrations)
	err = goose.SetDialect(Dialect)
	if err != nil {
		log.Fatal(ctx, "Invalid DB driver", "driver", Driver, err)
	}
	schemaEmpty := isSchemaEmpty(ctx, db)
	hasSchemaChanges := hasPendingMigrations(ctx, db, migrationsFolder)
	if !schemaEmpty && hasSchemaChanges {
		log.Info(ctx, "Upgrading DB Schema to latest version")
	}
	goose.SetLogger(&logAdapter{ctx: ctx, silent: schemaEmpty})
	err = goose.UpContext(ctx, db, migrationsFolder)
	if err != nil {
		log.Fatal(ctx, "Failed to apply new migrations", err)
	}

	if hasSchemaChanges && conf.Server.DevOptimizeDB {
		log.Debug(ctx, "Applying PRAGMA optimize after schema changes")
		_, err = db.ExecContext(ctx, "PRAGMA optimize")
		if err != nil {
			log.Error(ctx, "Error applying PRAGMA optimize", err)
		}
	}

	return func() {
		Close(ctx)
	}
}

// Optimize runs PRAGMA optimize on each connection in the pool
//
// Optimize 对连接池中每条连接执行 PRAGMA optimize。
// SQLite 的统计信息是按连接维护的，只在一条连接上执行无法惠及其余连接，
// 故逐一取出所有连接执行后再统一归还。
func Optimize(ctx context.Context) {
	if !conf.Server.DevOptimizeDB {
		return
	}
	numConns := Db().Stats().OpenConnections
	if numConns == 0 {
		log.Debug(ctx, "No open connections to optimize")
		return
	}
	log.Debug(ctx, "Optimizing open connections", "numConns", numConns)
	var conns []*sql.Conn
	for i := 0; i < numConns; i++ {
		conn, err := Db().Conn(ctx)
		conns = append(conns, conn)
		if err != nil {
			log.Error(ctx, "Error getting connection from pool", err)
			continue
		}
		_, err = conn.ExecContext(ctx, "PRAGMA optimize;")
		if err != nil {
			log.Error(ctx, "Error running PRAGMA optimize", err)
		}
	}

	// Return all connections to the Connection Pool
	for _, conn := range conns {
		conn.Close()
	}
}

// statusLogger 借 goose 的状态输出统计待执行的迁移数量。
// goose 未提供查询接口，只能从其日志中解析。
type statusLogger struct{ numPending int }

func (*statusLogger) Fatalf(format string, v ...interface{}) { log.Fatal(fmt.Sprintf(format, v...)) }
func (l *statusLogger) Printf(format string, v ...interface{}) {
	if len(v) < 1 {
		return
	}
	if v0, ok := v[0].(string); !ok {
		return
	} else if v0 == "Pending" {
		l.numPending++
	}
}

// hasPendingMigrations 判断是否存在未执行的迁移。
func hasPendingMigrations(ctx context.Context, db *sql.DB, folder string) bool {
	l := &statusLogger{}
	goose.SetLogger(l)
	err := goose.StatusContext(ctx, db, folder)
	if err != nil {
		log.Fatal(ctx, "Failed to check for pending migrations", err)
	}
	return l.numPending > 0
}

// isSchemaEmpty 通过 goose 版本表是否存在判断这是否为全新数据库。
func isSchemaEmpty(ctx context.Context, db *sql.DB) bool {
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_master WHERE type='table' AND name='goose_db_version';") // nolint:rowserrcheck
	if err != nil {
		log.Fatal(ctx, "Database could not be opened!", err)
	}
	defer rows.Close()
	return !rows.Next()
}

// logAdapter 把 goose 的日志接入 Navidrome 日志系统，silent 用于首次建库时静默。
type logAdapter struct {
	ctx    context.Context
	silent bool
}

func (l *logAdapter) Fatal(v ...interface{}) {
	log.Fatal(l.ctx, fmt.Sprint(v...))
}

func (l *logAdapter) Fatalf(format string, v ...interface{}) {
	log.Fatal(l.ctx, fmt.Sprintf(format, v...))
}

func (l *logAdapter) Print(v ...interface{}) {
	if !l.silent {
		log.Info(l.ctx, fmt.Sprint(v...))
	}
}

func (l *logAdapter) Println(v ...interface{}) {
	if !l.silent {
		log.Info(l.ctx, fmt.Sprintln(v...))
	}
}

func (l *logAdapter) Printf(format string, v ...interface{}) {
	if !l.silent {
		log.Info(l.ctx, fmt.Sprintf(format, v...))
	}
}
