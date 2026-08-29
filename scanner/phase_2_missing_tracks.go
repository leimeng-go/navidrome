package scanner

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	ppl "github.com/google/go-pipeline/pkg/pipeline"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// missingTracks 是一组共享同一持久化 ID（PID）的曲目：
// missing 为磁盘上已消失的，matched 为本轮新发现的候选。
// 二者配对即可识别出「文件被移动」而非「被删除后新增」。
type missingTracks struct {
	lib     model.Library
	pid     string
	missing model.MediaFiles
	matched model.MediaFiles
}

// phaseMissingTracks is responsible for processing missing media files during the scan process.
// It identifies media files that are marked as missing and attempts to find matching files that
// may have been moved or renamed. This phase helps in maintaining the integrity of the media
// library by ensuring that moved or renamed files are correctly updated in the database.
//
// The phaseMissingTracks phase performs the following steps:
// 1. Loads all libraries and their missing media files from the database.
// 2. For each library, it sorts the missing files by their PID (persistent identifier).
// 3. Groups missing and matched files by their PID and processes them to find exact or equivalent matches.
// 4. Updates the database with the new locations of the matched files and removes the old entries.
// 5. Logs the results and finalizes the phase by reporting the total number of matched files.
//
// phaseMissingTracks 是扫描的第二阶段：识别被移动或重命名的文件。
//
// 意义在于保住用户数据：若把「移动」当作「删除 + 新增」处理，
// 该曲目的播放次数、评分、星标乃至所在播放列表都会丢失。
type phaseMissingTracks struct {
	ctx                       context.Context
	ds                        model.DataStore
	totalMatched              atomic.Uint32
	state                     *scanState
	processedAlbumAnnotations map[string]bool // Track processed album annotation reassignments
	annotationMutex           sync.RWMutex    // Protects processedAlbumAnnotations
}

func createPhaseMissingTracks(ctx context.Context, state *scanState, ds model.DataStore) *phaseMissingTracks {
	return &phaseMissingTracks{
		ctx:                       ctx,
		ds:                        ds,
		state:                     state,
		processedAlbumAnnotations: make(map[string]bool),
	}
}

func (p *phaseMissingTracks) description() string {
	return "Process missing files, checking for moves"
}

func (p *phaseMissingTracks) producer() ppl.Producer[*missingTracks] {
	return ppl.NewProducer(p.produce, ppl.Name("load missing tracks from db"))
}

// produce 按 PID 分组产出待配对的曲目集合。
//
// 数据库游标已按 PID 排序，故只需在 PID 变化时切分分组，
// 无需把全部数据读入内存再分组。
// 只有存在缺失曲目的分组才有配对价值，其余直接丢弃。
func (p *phaseMissingTracks) produce(put func(tracks *missingTracks)) error {
	count := 0
	var putIfMatched = func(mt missingTracks) {
		if mt.pid != "" && len(mt.missing) > 0 {
			log.Trace(p.ctx, "Scanner: Found missing tracks", "pid", mt.pid, "missing", "title", mt.missing[0].Title,
				len(mt.missing), "matched", len(mt.matched), "lib", mt.lib.Name,
			)
			count++
			put(&mt)
		}
	}
	for _, lib := range p.state.libraries {
		log.Debug(p.ctx, "Scanner: Checking missing tracks", "libraryId", lib.ID, "libraryName", lib.Name)
		cursor, err := p.ds.MediaFile(p.ctx).GetMissingAndMatching(lib.ID)
		if err != nil {
			return fmt.Errorf("loading missing tracks for library %s: %w", lib.Name, err)
		}

		// Group missing and matched tracks by PID
		mt := missingTracks{lib: lib}
		for mf, err := range cursor {
			if err != nil {
				return fmt.Errorf("loading missing tracks for library %s: %w", lib.Name, err)
			}
			if mt.pid != mf.PID {
				putIfMatched(mt)
				mt.pid = mf.PID
				mt.missing = nil
				mt.matched = nil
			}
			if mf.Missing {
				mt.missing = append(mt.missing, mf)
			} else {
				mt.matched = append(mt.matched, mf)
			}
		}
		putIfMatched(mt)
		if count == 0 {
			log.Debug(p.ctx, "Scanner: No potential moves found", "libraryId", lib.ID, "libraryName", lib.Name)
		} else {
			log.Debug(p.ctx, "Scanner: Found potential moves", "libraryId", lib.ID, "count", count)
		}
	}

	return nil
}

// stages 定义两级配对：先在库内配对，剩余的再尝试跨库配对。
func (p *phaseMissingTracks) stages() []ppl.Stage[*missingTracks] {
	return []ppl.Stage[*missingTracks]{
		ppl.NewStage(p.processMissingTracks, ppl.Name("process missing tracks")),
		ppl.NewStage(p.processCrossLibraryMoves, ppl.Name("process cross-library moves")),
	}
}

// processMissingTracks 在同一音乐库内为缺失曲目寻找新位置。
//
// 按可信度从高到低尝试三种策略：
//  1. 完全匹配——所有元数据一致，可确信是同一文件；
//  2. 唯一匹配——该 PID 下恰好一缺一新，即便元数据略有出入也几乎必然是同一文件；
//  3. 等价匹配——基础路径或关键元数据相近，作为兜底。
//
// 本组已找到匹配时返回 nil，示意跳过后续的跨库配对环节。
func (p *phaseMissingTracks) processMissingTracks(in *missingTracks) (*missingTracks, error) {
	hasMatches := false

	for _, ms := range in.missing {
		var exactMatch model.MediaFile
		var equivalentMatch model.MediaFile

		// Identify exact and equivalent matches
		for _, mt := range in.matched {
			if ms.Equals(mt) {
				exactMatch = mt
				break // Prioritize exact match
			}
			if ms.IsEquivalent(mt) {
				equivalentMatch = mt
			}
		}

		// Use the exact match if found
		if exactMatch.ID != "" {
			log.Debug(p.ctx, "Scanner: Found missing track in a new place", "missing", ms.Path, "movedTo", exactMatch.Path, "lib", in.lib.Name)
			err := p.moveMatched(exactMatch, ms)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error moving matched track", "missing", ms.Path, "movedTo", exactMatch.Path, "lib", in.lib.Name, err)
				return nil, err
			}
			p.totalMatched.Add(1)
			hasMatches = true
			continue
		}

		// If there is only one missing and one matched track, consider them equivalent (same PID)
		if len(in.missing) == 1 && len(in.matched) == 1 {
			singleMatch := in.matched[0]
			log.Debug(p.ctx, "Scanner: Found track with same persistent ID in a new place", "missing", ms.Path, "movedTo", singleMatch.Path, "lib", in.lib.Name)
			err := p.moveMatched(singleMatch, ms)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error updating matched track", "missing", ms.Path, "movedTo", singleMatch.Path, "lib", in.lib.Name, err)
				return nil, err
			}
			p.totalMatched.Add(1)
			hasMatches = true
			continue
		}

		// Use the equivalent match if no other better match was found
		if equivalentMatch.ID != "" {
			log.Debug(p.ctx, "Scanner: Found missing track with same base path", "missing", ms.Path, "movedTo", equivalentMatch.Path, "lib", in.lib.Name)
			err := p.moveMatched(equivalentMatch, ms)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error updating matched track", "missing", ms.Path, "movedTo", equivalentMatch.Path, "lib", in.lib.Name, err)
				return nil, err
			}
			p.totalMatched.Add(1)
			hasMatches = true
		}
	}

	// If any matches were found in this missingTracks group, return nil
	// This signals the next stage to skip processing this group
	if hasMatches {
		return nil, nil
	}

	// If no matches found, pass through to next stage
	return in, nil
}

// processCrossLibraryMoves processes files that weren't matched within their library
// and attempts to find matches in other libraries
//
// processCrossLibraryMoves 处理跨音乐库的文件移动，
// 覆盖用户把文件从一个库挪到另一个库的场景。
// 单条失败只记录并继续，不影响其余曲目的配对。
func (p *phaseMissingTracks) processCrossLibraryMoves(in *missingTracks) (*missingTracks, error) {
	// Skip if input is nil (meaning previous stage found matches)
	if in == nil {
		return nil, nil
	}

	log.Debug(p.ctx, "Scanner: Processing cross-library moves", "pid", in.pid, "missing", len(in.missing), "lib", in.lib.Name)

	for _, missing := range in.missing {
		found, err := p.findCrossLibraryMatch(missing)
		if err != nil {
			log.Error(p.ctx, "Scanner: Error searching for cross-library matches", "missing", missing.Path, "lib", in.lib.Name, err)
			continue
		}

		if found.ID != "" {
			log.Debug(p.ctx, "Scanner: Found cross-library moved track", "missing", missing.Path, "movedTo", found.Path, "fromLib", in.lib.Name, "toLib", found.LibraryName)
			err := p.moveMatched(found, missing)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error moving cross-library track", "missing", missing.Path, "movedTo", found.Path, err)
				continue
			}
			p.totalMatched.Add(1)
		}
	}

	return in, nil
}

// findCrossLibraryMatch searches for a missing file in other libraries using two-tier matching
//
// findCrossLibraryMatch 跨库查找匹配，分两级：
//  1. 优先用 MusicBrainz 曲目 ID——全球唯一标识，可信度最高；
//  2. 退而用内在属性（标题、大小、扩展名等）。
//
// 两级均只接受「完全匹配」或「候选唯一且等价」，
// 跨库场景下误判代价更高（会把两个库中的不同文件错误合并）。
// 搜索范围限定在缺失文件创建时间之后新增的文件，缩小比对范围。
func (p *phaseMissingTracks) findCrossLibraryMatch(missing model.MediaFile) (model.MediaFile, error) {
	// First tier: Search by MusicBrainz Track ID if available
	if missing.MbzReleaseTrackID != "" {
		matches, err := p.ds.MediaFile(p.ctx).FindRecentFilesByMBZTrackID(missing, missing.CreatedAt)
		if err != nil {
			log.Error(p.ctx, "Scanner: Error searching for recent files by MBZ Track ID", "mbzTrackID", missing.MbzReleaseTrackID, err)
		} else {
			// Apply the same matching logic as within-library matching
			for _, match := range matches {
				if missing.Equals(match) {
					return match, nil // Exact match found
				}
			}

			// If only one match and it's equivalent, use it
			if len(matches) == 1 && missing.IsEquivalent(matches[0]) {
				return matches[0], nil
			}
		}
	}

	// Second tier: Search by intrinsic properties (title, size, suffix, etc.)
	matches, err := p.ds.MediaFile(p.ctx).FindRecentFilesByProperties(missing, missing.CreatedAt)
	if err != nil {
		log.Error(p.ctx, "Scanner: Error searching for recent files by properties", "missing", missing.Path, err)
		return model.MediaFile{}, err
	}

	// Apply the same matching logic as within-library matching
	for _, match := range matches {
		if missing.Equals(match) {
			return match, nil // Exact match found
		}
	}

	// If only one match and it's equivalent, use it
	if len(matches) == 1 && missing.IsEquivalent(matches[0]) {
		return matches[0], nil
	}

	return model.MediaFile{}, nil
}

// moveMatched 把新位置的曲目「改写」为原曲目，实现移动语义。
//
// 关键手法：保留旧记录的 ID，用新位置的内容覆盖它，再删掉新插入的那行。
// 这样所有以 ID 关联的数据（标注、播放列表项、播放历史）自动跟随，
// 无需逐一迁移。
//
// 若移动导致专辑归属变化，还需迁移专辑级标注。
// 多个曲目可能同时移入同一张专辑，用 map 去重避免重复迁移；
// 环节可能并发执行，故先读锁快速判断、再写锁二次确认（双检锁）。
func (p *phaseMissingTracks) moveMatched(target, missing model.MediaFile) error {
	return p.ds.WithTx(func(tx model.DataStore) error {
		discardedID := target.ID
		oldAlbumID := missing.AlbumID
		newAlbumID := target.AlbumID

		// Update the target media file with the missing file's ID. This effectively "moves" the track
		// to the new location while keeping its annotations and references intact.
		target.ID = missing.ID
		err := tx.MediaFile(p.ctx).Put(&target)
		if err != nil {
			return fmt.Errorf("update matched track: %w", err)
		}

		// Discard the new mediafile row (the one that was moved to)
		err = tx.MediaFile(p.ctx).Delete(discardedID)
		if err != nil {
			return fmt.Errorf("delete discarded track: %w", err)
		}

		// Handle album annotation reassignment if AlbumID changed
		if oldAlbumID != newAlbumID {
			// Use newAlbumID as key since we only care about avoiding duplicate reassignments to the same target
			p.annotationMutex.RLock()
			alreadyProcessed := p.processedAlbumAnnotations[newAlbumID]
			p.annotationMutex.RUnlock()

			if !alreadyProcessed {
				p.annotationMutex.Lock()
				// Double-check pattern to avoid race conditions
				if !p.processedAlbumAnnotations[newAlbumID] {
					// Reassign direct album annotations (starred, rating)
					log.Debug(p.ctx, "Scanner: Reassigning album annotations", "from", oldAlbumID, "to", newAlbumID)
					if err := tx.Album(p.ctx).ReassignAnnotation(oldAlbumID, newAlbumID); err != nil {
						log.Warn(p.ctx, "Scanner: Could not reassign album annotations", "from", oldAlbumID, "to", newAlbumID, err)
					}

					// Note: RefreshPlayCounts will be called in later phases, so we don't need to call it here
					p.processedAlbumAnnotations[newAlbumID] = true
				}
				p.annotationMutex.Unlock()
			} else {
				log.Trace(p.ctx, "Scanner: Skipping album annotation reassignment", "from", oldAlbumID, "to", newAlbumID)
			}
		}

		p.state.changesDetected.Store(true)
		return nil
	})
}

// finalize 汇报配对结果，并按配置清理仍处于缺失状态的记录。
// 缺省不清理：文件可能只是临时不可访问（外置硬盘未挂载、网络存储掉线），
// 贸然删除会连带丢失用户标注。
func (p *phaseMissingTracks) finalize(err error) error {
	matched := p.totalMatched.Load()
	if matched > 0 {
		log.Info(p.ctx, "Scanner: Found moved files", "total", matched, err)
	}
	if err != nil {
		return err
	}

	// Check if we should purge missing items
	if conf.Server.Scanner.PurgeMissing == consts.PurgeMissingAlways || (conf.Server.Scanner.PurgeMissing == consts.PurgeMissingFull && p.state.fullScan) {
		if err = p.purgeMissing(); err != nil {
			log.Error(p.ctx, "Scanner: Error purging missing items", err)
		}
	}

	return err
}

// purgeMissing 从数据库彻底删除仍缺失的曲目。
// 有删除发生时置位 changesDetected，确保收尾阶段的 GC 会执行，
// 以清理由此产生的空专辑与孤儿艺人。
func (p *phaseMissingTracks) purgeMissing() error {
	deletedCount, err := p.ds.MediaFile(p.ctx).DeleteAllMissing()
	if err != nil {
		return fmt.Errorf("error deleting missing files: %w", err)
	}

	if deletedCount > 0 {
		log.Info(p.ctx, "Scanner: Purged missing items from the database", "mediaFiles", deletedCount)
		// Set changesDetected to true so that garbage collection will run at the end of the scan process
		p.state.changesDetected.Store(true)
	} else {
		log.Debug(p.ctx, "Scanner: No missing items to purge")
	}

	return nil
}

var _ phase[*missingTracks] = (*phaseMissingTracks)(nil)
