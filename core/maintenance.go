package core

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/slice"
)

// Maintenance 提供库维护操作，目前是清理标记为缺失的文件记录。
type Maintenance interface {
	// DeleteMissingFiles deletes specific missing files by their IDs
	// DeleteMissingFiles 按 ID 删除指定的缺失文件记录
	DeleteMissingFiles(ctx context.Context, ids []string) error
	// DeleteAllMissingFiles deletes all files marked as missing
	// DeleteAllMissingFiles 删除全部标记为缺失的文件记录
	DeleteAllMissingFiles(ctx context.Context) error
}

// maintenanceService 是 Maintenance 的实现。
// wg 用于在测试中等待后台统计刷新完成。
type maintenanceService struct {
	ds model.DataStore
	wg sync.WaitGroup
}

// NewMaintenance 创建维护服务。
func NewMaintenance(ds model.DataStore) Maintenance {
	return &maintenanceService{
		ds: ds,
	}
}

func (s *maintenanceService) DeleteMissingFiles(ctx context.Context, ids []string) error {
	return s.deleteMissing(ctx, ids)
}

func (s *maintenanceService) DeleteAllMissingFiles(ctx context.Context) error {
	return s.deleteMissing(ctx, nil)
}

// deleteMissing handles the deletion of missing files and triggers necessary cleanup operations
//
// deleteMissing 删除缺失文件并做后续清理。ids 为空表示删除全部缺失记录。
//
// 顺序要点：必须先取出受影响的专辑 ID，删除之后就查不到了。
// 删除后跑一次 GC 清理孤儿记录（空专辑、空艺术家），
// 最后异步刷新统计——统计刷新耗时较长，不应阻塞请求。
func (s *maintenanceService) deleteMissing(ctx context.Context, ids []string) error {
	// Track affected album IDs before deletion for refresh
	affectedAlbumIDs, err := s.getAffectedAlbumIDs(ctx, ids)
	if err != nil {
		log.Warn(ctx, "Error tracking affected albums for refresh", err)
		// Don't fail the operation, just log the warning
	}

	// Delete missing files within a transaction
	err = s.ds.WithTx(func(tx model.DataStore) error {
		if len(ids) == 0 {
			_, err := tx.MediaFile(ctx).DeleteAllMissing()
			return err
		}
		return tx.MediaFile(ctx).DeleteMissing(ids)
	})
	if err != nil {
		log.Error(ctx, "Error deleting missing tracks from DB", "ids", ids, err)
		return err
	}

	// Run garbage collection to clean up orphaned records
	if err := s.ds.GC(ctx); err != nil {
		log.Error(ctx, "Error running GC after deleting missing tracks", err)
		return err
	}

	// Refresh statistics in background
	s.refreshStatsAsync(ctx, affectedAlbumIDs)

	return nil
}

// refreshAlbums recalculates album attributes (size, duration, song count, etc.) from media files.
// It uses batch queries to minimize database round-trips for efficiency.
//
// refreshAlbums 依据现存曲目重算专辑的大小、时长、曲目数等聚合属性。
// 按 100 个一批处理，避免 SQL 的 IN 子句过长触发参数数量限制。
func (s *maintenanceService) refreshAlbums(ctx context.Context, albumIDs []string) error {
	if len(albumIDs) == 0 {
		return nil
	}

	log.Debug(ctx, "Refreshing albums", "count", len(albumIDs))

	// Process in chunks to avoid query size limits
	const chunkSize = 100
	for chunk := range slice.CollectChunks(slices.Values(albumIDs), chunkSize) {
		if err := s.refreshAlbumChunk(ctx, chunk); err != nil {
			return fmt.Errorf("refreshing album chunk: %w", err)
		}
	}

	log.Debug(ctx, "Successfully refreshed albums", "count", len(albumIDs))
	return nil
}

// refreshAlbumChunk processes a single chunk of album IDs
//
// refreshAlbumChunk 处理一批专辑：两次批量查询取回专辑与曲目，在内存中分组重算。
//
// 只有内容真正变化时才写库，避免无谓的 UPDATE 与 UpdatedAt 抖动。
// CreatedAt 需从旧记录继承，因为重算出的专辑是全新对象。
// 已无曲目的专辑跳过，交由 GC 删除。
// 单个专辑写入失败只记日志继续，不因一张专辑中断整批。
func (s *maintenanceService) refreshAlbumChunk(ctx context.Context, albumIDs []string) error {
	albumRepo := s.ds.Album(ctx)
	mfRepo := s.ds.MediaFile(ctx)

	// Batch load existing albums
	albums, err := albumRepo.GetAll(model.QueryOptions{
		Filters: squirrel.Eq{"album.id": albumIDs},
	})
	if err != nil {
		return fmt.Errorf("loading albums: %w", err)
	}

	// Create a map for quick lookup
	albumMap := make(map[string]*model.Album, len(albums))
	for i := range albums {
		albumMap[albums[i].ID] = &albums[i]
	}

	// Batch load all media files for these albums
	mediaFiles, err := mfRepo.GetAll(model.QueryOptions{
		Filters: squirrel.Eq{"album_id": albumIDs},
		Sort:    "album_id, path",
	})
	if err != nil {
		return fmt.Errorf("loading media files: %w", err)
	}

	// Group media files by album ID
	filesByAlbum := make(map[string]model.MediaFiles)
	for i := range mediaFiles {
		albumID := mediaFiles[i].AlbumID
		filesByAlbum[albumID] = append(filesByAlbum[albumID], mediaFiles[i])
	}

	// Recalculate each album from its media files
	for albumID, oldAlbum := range albumMap {
		mfs, hasTracks := filesByAlbum[albumID]
		if !hasTracks {
			// Album has no tracks anymore, skip (will be cleaned up by GC)
			log.Debug(ctx, "Skipping album with no tracks", "albumID", albumID)
			continue
		}

		// Recalculate album from media files
		newAlbum := mfs.ToAlbum()

		// Only update if something changed (avoid unnecessary writes)
		if !oldAlbum.Equals(newAlbum) {
			// Preserve original timestamps
			newAlbum.UpdatedAt = time.Now()
			newAlbum.CreatedAt = oldAlbum.CreatedAt

			if err := albumRepo.Put(&newAlbum); err != nil {
				log.Error(ctx, "Error updating album during refresh", "albumID", albumID, err)
				// Continue with other albums instead of failing entirely
				continue
			}
			log.Trace(ctx, "Refreshed album", "albumID", albumID, "name", newAlbum.Name)
		}
	}

	return nil
}

// getAffectedAlbumIDs returns distinct album IDs from missing media files
//
// getAffectedAlbumIDs 取出即将被删除的缺失文件所属的专辑 ID（去重）。
// 必须在删除前调用。
func (s *maintenanceService) getAffectedAlbumIDs(ctx context.Context, ids []string) ([]string, error) {
	var filters squirrel.Sqlizer = squirrel.Eq{"missing": true}
	if len(ids) > 0 {
		filters = squirrel.And{
			squirrel.Eq{"missing": true},
			squirrel.Eq{"media_file.id": ids},
		}
	}

	mfs, err := s.ds.MediaFile(ctx).GetAll(model.QueryOptions{
		Filters: filters,
	})
	if err != nil {
		return nil, err
	}

	// Extract unique album IDs
	albumIDMap := make(map[string]struct{}, len(mfs))
	for _, mf := range mfs {
		if mf.AlbumID != "" {
			albumIDMap[mf.AlbumID] = struct{}{}
		}
	}

	albumIDs := make([]string, 0, len(albumIDMap))
	for id := range albumIDMap {
		albumIDs = append(albumIDs, id)
	}

	return albumIDs, nil
}

// refreshStatsAsync refreshes artist and album statistics in background goroutines
//
// refreshStatsAsync 在后台协程中刷新艺术家与专辑统计。
// 使用 background 上下文并携带请求值：请求结束后统计仍需继续，
// 但日志中要保留用户等上下文信息。
func (s *maintenanceService) refreshStatsAsync(ctx context.Context, affectedAlbumIDs []string) {
	// Refresh artist stats in background
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		bgCtx := request.AddValues(context.Background(), ctx)
		if _, err := s.ds.Artist(bgCtx).RefreshStats(true); err != nil {
			log.Error(bgCtx, "Error refreshing artist stats after deleting missing files", err)
		} else {
			log.Debug(bgCtx, "Successfully refreshed artist stats after deleting missing files")
		}

		// Refresh album stats in background if we have affected albums
		if len(affectedAlbumIDs) > 0 {
			if err := s.refreshAlbums(bgCtx, affectedAlbumIDs); err != nil {
				log.Error(bgCtx, "Error refreshing album stats after deleting missing files", err)
			} else {
				log.Debug(bgCtx, "Successfully refreshed album stats after deleting missing files", "count", len(affectedAlbumIDs))
			}
		}
	}()
}

// Wait waits for all background goroutines to complete.
// WARNING: This method is ONLY for testing. Never call this in production code.
// Calling Wait() in production will block until ALL background operations complete
// and may cause race conditions with new operations starting.
//
// wait 等待所有后台协程结束。仅供测试使用，生产代码不得调用。
func (s *maintenanceService) wait() {
	s.wg.Wait()
}
