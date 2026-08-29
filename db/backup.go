package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/mattn/go-sqlite3"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
)

const (
	backupPrefix      = "navidrome_backup"
	backupRegexString = backupPrefix + "_(.+)\\.db"
)

// backupRegex 用于从文件名中解析出备份时间。
var backupRegex = regexp.MustCompile(backupRegexString)

// backupSuffixLayout 是文件名中的时间格式，只用不含分隔歧义的字符，保证跨平台可用。
const backupSuffixLayout = "2006.01.02_15.04.05"

// backupPath 依据时间生成备份文件路径。
func backupPath(t time.Time) string {
	return filepath.Join(
		conf.Server.Backup.Path,
		fmt.Sprintf("%s_%s.db", backupPrefix, t.Format(backupSuffixLayout)),
	)
}

// backupOrRestore 借 SQLite 的在线备份 API 复制整库，备份与恢复只是源与目标互换。
//
// 不直接拷贝文件：运行中的数据库随时可能被写入，文件拷贝会得到不一致的快照。
// 一步完成（Step(-1)）意味着整个过程持有读锁，期间写操作会被阻塞，
// 但换来的是一致的快照，对备份场景更重要。
func backupOrRestore(ctx context.Context, isBackup bool, path string) error {
	// heavily inspired by https://codingrabbits.dev/posts/go_and_sqlite_backup_and_maybe_restore/
	existingConn, err := Db().Conn(ctx)
	if err != nil {
		return fmt.Errorf("getting existing connection: %w", err)
	}
	defer existingConn.Close()

	backupDb, err := sql.Open(Driver, path)
	if err != nil {
		return fmt.Errorf("opening backup database in '%s': %w", path, err)
	}
	defer backupDb.Close()

	backupConn, err := backupDb.Conn(ctx)
	if err != nil {
		return fmt.Errorf("getting backup connection: %w", err)
	}
	defer backupConn.Close()

	err = existingConn.Raw(func(existing any) error {
		return backupConn.Raw(func(backup any) error {
			var sourceOk, destOk bool
			var sourceConn, destConn *sqlite3.SQLiteConn

			if isBackup {
				sourceConn, sourceOk = existing.(*sqlite3.SQLiteConn)
				destConn, destOk = backup.(*sqlite3.SQLiteConn)
			} else {
				sourceConn, sourceOk = backup.(*sqlite3.SQLiteConn)
				destConn, destOk = existing.(*sqlite3.SQLiteConn)
			}

			if !sourceOk {
				return fmt.Errorf("error trying to convert source to sqlite connection")
			}
			if !destOk {
				return fmt.Errorf("error trying to convert destination to sqlite connection")
			}

			backupOp, err := destConn.Backup("main", sourceConn, "main")
			if err != nil {
				return fmt.Errorf("error starting sqlite backup: %w", err)
			}
			defer backupOp.Close()

			// Caution: -1 means that sqlite will hold a read lock until the operation finishes
			// This will lock out other writes that could happen at the same time
			done, err := backupOp.Step(-1)
			if !done {
				return fmt.Errorf("backup not done with step -1")
			}
			if err != nil {
				return fmt.Errorf("error during backup step: %w", err)
			}

			err = backupOp.Finish()
			if err != nil {
				return fmt.Errorf("error finishing backup: %w", err)
			}

			return nil
		})
	})

	return err
}

// Backup 创建一份带时间戳的数据库备份，返回其路径。
func Backup(ctx context.Context) (string, error) {
	destPath := backupPath(time.Now())
	log.Debug(ctx, "Creating backup", "path", destPath)
	err := backupOrRestore(ctx, true, destPath)
	if err != nil {
		return "", err
	}

	return destPath, nil
}

// Restore 从备份文件恢复数据库。
func Restore(ctx context.Context, path string) error {
	log.Debug(ctx, "Restoring backup", "path", path)
	return backupOrRestore(ctx, false, path)
}

// Prune 按保留数量清理旧备份。
// 时间从文件名解析而非取文件修改时间，后者会被拷贝、同步等操作改写。
// 单个文件删除失败不中断整体，最后汇总错误一并返回。
func Prune(ctx context.Context) (int, error) {
	files, err := os.ReadDir(conf.Server.Backup.Path)
	if err != nil {
		return 0, fmt.Errorf("unable to read database backup entries: %w", err)
	}

	var backupTimes []time.Time

	for _, file := range files {
		if !file.IsDir() {
			submatch := backupRegex.FindStringSubmatch(file.Name())
			if len(submatch) == 2 {
				timestamp, err := time.Parse(backupSuffixLayout, submatch[1])
				if err == nil {
					backupTimes = append(backupTimes, timestamp)
				}
			}
		}
	}

	if len(backupTimes) <= conf.Server.Backup.Count {
		return 0, nil
	}

	slices.SortFunc(backupTimes, func(a, b time.Time) int {
		return b.Compare(a)
	})

	pruneCount := 0
	var errs []error

	for _, timeToPrune := range backupTimes[conf.Server.Backup.Count:] {
		log.Debug(ctx, "Pruning backup", "time", timeToPrune)
		path := backupPath(timeToPrune)
		err = os.Remove(path)
		if err != nil {
			errs = append(errs, err)
		} else {
			pruneCount++
		}
	}

	if len(errs) > 0 {
		err = errors.Join(errs...)
		log.Error(ctx, "Failed to delete one or more files", "errors", err)
	}

	return pruneCount, err
}
