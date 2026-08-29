// nolint:unused
package scanner

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Masterminds/squirrel"
	ppl "github.com/google/go-pipeline/pkg/pipeline"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// phaseRefreshAlbums is responsible for refreshing albums that have been
// newly added or changed during the scan process. This phase ensures that
// the album information in the database is up-to-date by performing the
// following steps:
//  1. Loads all libraries and their albums that have been touched (new or changed).
//  2. For each album, it filters out unmodified albums by comparing the current
//     state with the state in the database.
//  3. Refreshes the album information in the database if any changes are detected.
//  4. Logs the results and finalizes the phase by reporting the total number of
//     refreshed and skipped albums.
//  5. As a last step, it refreshes the artist statistics to reflect the changes
//
// phaseRefreshAlbums 是扫描的第三阶段：依据曲目的最新元数据重建专辑信息。
//
// 阶段 1 中的专辑是按目录逐个生成的，可能不完整
// （同一张专辑的曲目可散落在多个目录）；这里按专辑汇总全部曲目重新计算。
type phaseRefreshAlbums struct {
	ds        model.DataStore
	ctx       context.Context
	refreshed atomic.Uint32
	skipped   atomic.Uint32
	state     *scanState
}

func createPhaseRefreshAlbums(ctx context.Context, state *scanState, ds model.DataStore) *phaseRefreshAlbums {
	return &phaseRefreshAlbums{ctx: ctx, ds: ds, state: state}
}

func (p *phaseRefreshAlbums) description() string {
	return "Refresh all new/changed albums"
}

func (p *phaseRefreshAlbums) producer() ppl.Producer[*model.Album] {
	return ppl.NewProducer(p.produce, ppl.Name("load albums from db"))
}

// produce 产出本轮被「触碰」过的专辑（有曲目新增、变更或缺失）。
// 未被触碰的专辑无需重算，这是本阶段的主要剪枝手段。
func (p *phaseRefreshAlbums) produce(put func(album *model.Album)) error {
	count := 0
	for _, lib := range p.state.libraries {
		cursor, err := p.ds.Album(p.ctx).GetTouchedAlbums(lib.ID)
		if err != nil {
			return fmt.Errorf("loading touched albums: %w", err)
		}
		log.Debug(p.ctx, "Scanner: Checking albums that may need refresh", "libraryId", lib.ID, "libraryName", lib.Name)
		for album, err := range cursor {
			if err != nil {
				return fmt.Errorf("loading touched albums: %w", err)
			}
			count++
			put(&album)
		}
	}
	if count == 0 {
		log.Debug(p.ctx, "Scanner: No albums needing refresh")
	} else {
		log.Debug(p.ctx, "Scanner: Found albums that may need refreshing", "count", count)
	}
	return nil
}

// stages 定义两个环节：并发比对筛选、串行写库。
func (p *phaseRefreshAlbums) stages() []ppl.Stage[*model.Album] {
	return []ppl.Stage[*model.Album]{
		ppl.NewStage(p.filterUnmodified, ppl.Name("filter unmodified"), ppl.Concurrency(5)),
		ppl.NewStage(p.refreshAlbum, ppl.Name("refresh albums")),
	}
}

// filterUnmodified 重建专辑信息并与库中现状比对，无变化则丢弃。
//
// 「被触碰」不等于「有变化」——例如曲目文件被改写但专辑级字段不变。
// 此处比对可避免无谓的写入及其引发的 updated_at 变动
// （后者会干扰「最近更新」类视图）。
//
// 无曲目的专辑同样跳过：交由收尾阶段的 GC 统一清理。
func (p *phaseRefreshAlbums) filterUnmodified(album *model.Album) (*model.Album, error) {
	mfs, err := p.ds.MediaFile(p.ctx).GetAll(model.QueryOptions{Filters: squirrel.Eq{"album_id": album.ID}})
	if err != nil {
		log.Error(p.ctx, "Error loading media files for album", "album_id", album.ID, err)
		return nil, err
	}
	if len(mfs) == 0 {
		log.Debug(p.ctx, "Scanner: album has no media files. Skipping", "album_id", album.ID,
			"name", album.Name, "songCount", album.SongCount, "updatedAt", album.UpdatedAt)
		p.skipped.Add(1)
		return nil, nil
	}

	newAlbum := mfs.ToAlbum()
	if album.Equals(newAlbum) {
		log.Trace("Scanner: album is up to date. Skipping", "album_id", album.ID,
			"name", album.Name, "songCount", album.SongCount, "updatedAt", album.UpdatedAt)
		p.skipped.Add(1)
		return nil, nil
	}
	return &newAlbum, nil
}

// refreshAlbum 写入更新后的专辑。
// album 为 nil 表示上一环节已判定无需更新。
func (p *phaseRefreshAlbums) refreshAlbum(album *model.Album) (*model.Album, error) {
	if album == nil {
		return nil, nil
	}
	start := time.Now()
	err := p.ds.Album(p.ctx).Put(album)
	log.Debug(p.ctx, "Scanner: refreshing album", "album_id", album.ID, "name", album.Name, "songCount", album.SongCount, "elapsed", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("refreshing album %s: %w", album.ID, err)
	}
	p.refreshed.Add(1)
	p.state.changesDetected.Store(true)
	return album, nil
}

// finalize 重算专辑与艺人的播放统计。
//
// 必须放在此处：曲目的播放次数可能因移动、合并而重新归属，
// 需要在专辑数据稳定后按曲目重新聚合。
func (p *phaseRefreshAlbums) finalize(err error) error {
	if err != nil {
		return err
	}
	logF := log.Info
	refreshed := p.refreshed.Load()
	skipped := p.skipped.Load()
	if refreshed == 0 {
		logF = log.Debug
	}
	logF(p.ctx, "Scanner: Finished refreshing albums", "refreshed", refreshed, "skipped", skipped, err)
	if !p.state.changesDetected.Load() {
		log.Debug(p.ctx, "Scanner: No changes detected, skipping refreshing annotations")
		return nil
	}
	// Refresh album annotations
	start := time.Now()
	cnt, err := p.ds.Album(p.ctx).RefreshPlayCounts()
	if err != nil {
		return fmt.Errorf("refreshing album annotations: %w", err)
	}
	log.Debug(p.ctx, "Scanner: Refreshed album annotations", "albums", cnt, "elapsed", time.Since(start))

	// Refresh artist annotations
	start = time.Now()
	cnt, err = p.ds.Artist(p.ctx).RefreshPlayCounts()
	if err != nil {
		return fmt.Errorf("refreshing artist annotations: %w", err)
	}
	log.Debug(p.ctx, "Scanner: Refreshed artist annotations", "artists", cnt, "elapsed", time.Since(start))
	p.state.changesDetected.Store(true)
	return nil
}
