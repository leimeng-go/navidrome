package scanner

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	ppl "github.com/google/go-pipeline/pkg/pipeline"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
)

// phasePlaylists 是扫描的第四阶段：从磁盘导入 M3U / NSP 播放列表文件。
// 与阶段 3 并行执行，二者操作的数据互不重叠。
type phasePlaylists struct {
	ctx       context.Context
	scanState *scanState
	ds        model.DataStore
	pls       core.Playlists
	cw        artwork.CacheWarmer
	refreshed atomic.Uint32
}

func createPhasePlaylists(ctx context.Context, scanState *scanState, ds model.DataStore, pls core.Playlists, cw artwork.CacheWarmer) *phasePlaylists {
	return &phasePlaylists{
		ctx:       ctx,
		scanState: scanState,
		ds:        ds,
		pls:       pls,
		cw:        cw,
	}
}

func (p *phasePlaylists) description() string {
	return "Import/update playlists"
}

func (p *phasePlaylists) producer() ppl.Producer[*model.Folder] {
	return ppl.NewProducer(p.produce, ppl.Name("load folders with playlists from db"))
}

// produce 产出本轮有变动且含播放列表文件的目录。
//
// 两种情况直接跳过：配置关闭了自动导入；
// 或系统尚无管理员——导入的列表必须有归属者，
// 此时导入会导致列表无主，故宁可延后。
func (p *phasePlaylists) produce(put func(entry *model.Folder)) error {
	if !conf.Server.AutoImportPlaylists {
		log.Info(p.ctx, "Playlists will not be imported, AutoImportPlaylists is set to false")
		return nil
	}
	u, _ := request.UserFrom(p.ctx)
	if !u.IsAdmin {
		log.Warn(p.ctx, "Playlists will not be imported, as there are no admin users yet, "+
			"Please create an admin user first, and then update the playlists for them to be imported")
		return nil
	}

	count := 0
	cursor, err := p.ds.Folder(p.ctx).GetTouchedWithPlaylists()
	if err != nil {
		return fmt.Errorf("loading touched folders: %w", err)
	}
	log.Debug(p.ctx, "Scanner: Checking playlists that may need refresh")
	for folder, err := range cursor {
		if err != nil {
			return fmt.Errorf("loading touched folder: %w", err)
		}
		count++
		put(&folder)
	}
	if count == 0 {
		log.Debug(p.ctx, "Scanner: No playlists need refreshing")
	} else {
		log.Debug(p.ctx, "Scanner: Found folders with playlists that may need refreshing", "count", count)
	}

	return nil
}

// stages 只有一个环节，并发度 3：导入需读文件并写库，过高并发意义不大。
func (p *phasePlaylists) stages() []ppl.Stage[*model.Folder] {
	return []ppl.Stage[*model.Folder]{
		ppl.NewStage(p.processPlaylistsInFolder, ppl.Name("process playlists in folder"), ppl.Concurrency(3)),
	}
}

// processPlaylistsInFolder 导入目录内的所有播放列表文件。
// 跳过隐藏文件与非播放列表文件；
// 单个列表导入失败只跳过该文件，不影响同目录其他列表。
func (p *phasePlaylists) processPlaylistsInFolder(folder *model.Folder) (*model.Folder, error) {
	files, err := os.ReadDir(folder.AbsolutePath())
	if err != nil {
		log.Error(p.ctx, "Scanner: Error reading files", "folder", folder, err)
		p.scanState.sendWarning(err.Error())
		return folder, nil
	}
	for _, f := range files {
		started := time.Now()
		if strings.HasPrefix(f.Name(), ".") {
			continue
		}
		if !model.IsValidPlaylist(f.Name()) {
			continue
		}
		// BFR: Check if playlist needs to be refreshed (timestamp, sync flag, etc)
		pls, err := p.pls.ImportFile(p.ctx, folder, f.Name())
		if err != nil {
			continue
		}
		if pls.IsSmartPlaylist() {
			log.Debug("Scanner: Imported smart playlist", "name", pls.Name, "lastUpdated", pls.UpdatedAt, "path", pls.Path, "elapsed", time.Since(started))
		} else {
			log.Debug("Scanner: Imported playlist", "name", pls.Name, "lastUpdated", pls.UpdatedAt, "path", pls.Path, "numTracks", len(pls.Tracks), "elapsed", time.Since(started))
		}
		p.cw.PreCache(pls.CoverArtID())
		p.refreshed.Add(1)
	}
	return folder, nil
}

// finalize 汇报导入结果。有列表更新即视为发生变化，
// 以触发收尾阶段的 GC 与统计刷新。
func (p *phasePlaylists) finalize(err error) error {
	refreshed := p.refreshed.Load()
	logF := log.Info
	if refreshed == 0 {
		logF = log.Debug
	} else {
		p.scanState.changesDetected.Store(true)
	}
	logF(p.ctx, "Scanner: Finished refreshing playlists", "refreshed", refreshed, err)
	return err
}

var _ phase[*model.Folder] = (*phasePlaylists)(nil)
