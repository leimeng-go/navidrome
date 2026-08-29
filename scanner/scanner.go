package scanner

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync/atomic"
	"time"

	ppl "github.com/google/go-pipeline/pkg/pipeline"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/db"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/run"
	"github.com/navidrome/navidrome/utils/slice"
)

// scannerImpl 是扫描器的进程内实现，负责编排四阶段流水线。
// 另有 scannerExternal（external.go）以子进程方式运行同一套逻辑。
type scannerImpl struct {
	ds  model.DataStore
	cw  artwork.CacheWarmer
	pls core.Playlists
}

// scanState holds the state of an in-progress scan, to be passed to the various phases
// scanState 保存一次扫描的共享状态，贯穿各个阶段。
type scanState struct {
	progress        chan<- *ProgressInfo
	fullScan        bool
	changesDetected atomic.Bool      // 原子类型：各阶段可能并发写入
	libraries       model.Libraries  // Store libraries list for consistency across phases
	targets         map[int][]string // Optional: map[libraryID][]folderPaths for selective scans
}

// sendProgress 上报进度。progress 为 nil 时静默丢弃，
// 使各阶段无需关心是否有人在监听。
func (s *scanState) sendProgress(info *ProgressInfo) {
	if s.progress != nil {
		s.progress <- info
	}
}

// isSelectiveScan 判断是否为指定目录的定向扫描（而非全库扫描）。
func (s *scanState) isSelectiveScan() bool {
	return len(s.targets) > 0
}

func (s *scanState) sendWarning(msg string) {
	s.sendProgress(&ProgressInfo{Warning: msg})
}

func (s *scanState) sendError(err error) {
	s.sendProgress(&ProgressInfo{Error: err.Error()})
}

// scanFolders 执行一次完整的扫描流程。
//
// targets 非空时为定向扫描：只处理指定库中的指定目录，
// 用于文件系统监听器捕捉到局部变更的场景。
//
// 全量扫描直接把 changesDetected 置真，
// 确保 GC、统计刷新等收尾操作一定执行（即使本次未发现文件变化）。
func (s *scannerImpl) scanFolders(ctx context.Context, fullScan bool, targets []model.ScanTarget, progress chan<- *ProgressInfo) {
	startTime := time.Now()

	state := scanState{
		progress:        progress,
		fullScan:        fullScan,
		changesDetected: atomic.Bool{},
	}

	// Set changesDetected to true for full scans to ensure all maintenance operations run
	if fullScan {
		state.changesDetected.Store(true)
	}

	// Get libraries and optionally filter by targets
	allLibs, err := s.ds.Library(ctx).GetAll()
	if err != nil {
		state.sendWarning(fmt.Sprintf("getting libraries: %s", err))
		return
	}

	if len(targets) > 0 {
		// Selective scan: filter libraries and build targets map
		state.targets = make(map[int][]string)

		for _, target := range targets {
			folderPath := target.FolderPath
			if folderPath == "" {
				folderPath = "."
			}
			state.targets[target.LibraryID] = append(state.targets[target.LibraryID], folderPath)
		}

		// Filter libraries to only those in targets
		state.libraries = slice.Filter(allLibs, func(lib model.Library) bool {
			return len(state.targets[lib.ID]) > 0
		})

		log.Info(ctx, "Scanner: Starting selective scan", "fullScan", state.fullScan, "numLibraries", len(state.libraries), "numTargets", len(targets))
	} else {
		// Full library scan
		state.libraries = allLibs
		log.Info(ctx, "Scanner: Starting scan", "fullScan", state.fullScan, "numLibraries", len(state.libraries))
	}

	// Store scan type and start time
	scanType := "quick"
	if state.fullScan {
		scanType = "full"
	}
	if state.isSelectiveScan() {
		scanType += "-selective"
	}
	_ = s.ds.Property(ctx).Put(consts.LastScanTypeKey, scanType)
	_ = s.ds.Property(ctx).Put(consts.LastScanStartTimeKey, startTime.Format(time.RFC3339))

	// if there was a full scan in progress, force a full scan
	// 上次全量扫描被中断时强制再做一次全量：
	// 中断意味着部分目录未处理，增量扫描无法补齐这些遗漏
	if !state.fullScan {
		for _, lib := range state.libraries {
			if lib.FullScanInProgress {
				log.Info(ctx, "Scanner: Interrupted full scan detected", "lib", lib.Name)
				state.fullScan = true
				if state.isSelectiveScan() {
					_ = s.ds.Property(ctx).Put(consts.LastScanTypeKey, "full-selective")
				} else {
					_ = s.ds.Property(ctx).Put(consts.LastScanTypeKey, "full")
				}
				break
			}
		}
	}

	// Prepare libraries for scanning (initialize LastScanStartedAt if needed)
	err = s.prepareLibrariesForScan(ctx, &state)
	if err != nil {
		log.Error(ctx, "Scanner: Error preparing libraries for scan", err)
		state.sendError(err)
		return
	}

	err = run.Sequentially(
		// Phase 1: Scan all libraries and import new/updated files
		// 阶段 1：遍历目录，导入新增/变更的文件
		runPhase[*folderEntry](ctx, 1, createPhaseFolders(ctx, &state, s.ds, s.cw)),

		// Phase 2: Process missing files, checking for moves
		// 阶段 2：处理缺失文件，识别「移动」而非「删除」
		// 必须在阶段 1 之后：需要先知道有哪些新文件才能配对
		runPhase[*missingTracks](ctx, 2, createPhaseMissingTracks(ctx, &state, s.ds)),

		// Phases 3 and 4 can be run in parallel
		// 阶段 3 与 4 互不依赖，可并行
		run.Parallel(
			// Phase 3: Refresh all new/changed albums and update artists
			// 阶段 3：依据曲目元数据刷新专辑与艺人
			runPhase[*model.Album](ctx, 3, createPhaseRefreshAlbums(ctx, &state, s.ds)),

			// Phase 4: Import/update playlists
			// 阶段 4：导入/更新播放列表文件
			runPhase[*model.Folder](ctx, 4, createPhasePlaylists(ctx, &state, s.ds, s.pls, s.cw)),
		),

		// Final Steps (cannot be parallelized):
		// 收尾步骤，彼此有依赖，必须串行

		// Run GC if there were any changes (Remove dangling tracks, empty albums and artists, and orphan annotations)
		s.runGC(ctx, &state),

		// Refresh artist and tags stats
		s.runRefreshStats(ctx, &state),

		// Update last_scan_completed_at for all libraries
		s.runUpdateLibraries(ctx, &state),

		// Optimize DB
		s.runOptimize(ctx),
	)
	if err != nil {
		log.Error(ctx, "Scanner: Finished with error", "duration", time.Since(startTime), err)
		_ = s.ds.Property(ctx).Put(consts.LastScanErrorKey, err.Error())
		state.sendError(err)
		return
	}

	_ = s.ds.Property(ctx).Put(consts.LastScanErrorKey, "")

	if state.changesDetected.Load() {
		state.sendProgress(&ProgressInfo{ChangesDetected: true})
	}

	if state.isSelectiveScan() {
		log.Info(ctx, "Scanner: Finished scanning selected folders", "duration", time.Since(startTime), "numTargets", len(targets))
	} else {
		log.Info(ctx, "Scanner: Finished scanning all libraries", "duration", time.Since(startTime))
	}
}

// prepareLibrariesForScan initializes the scan for all libraries in the state.
// It calls ScanBegin for libraries that haven't started scanning yet (LastScanStartedAt is zero),
// reloads them to get the updated state, and filters out any libraries that fail to initialize.
//
// prepareLibrariesForScan 为各音乐库标记扫描开始。
// LastScanStartedAt 非零表示上次扫描被中断，本次视为续扫，不重置开始时间——
// 各阶段用该时间判断哪些记录是本轮扫描触及的。
// 初始化失败的库只发警告并跳过，不影响其他库；全部失败才返回错误。
func (s *scannerImpl) prepareLibrariesForScan(ctx context.Context, state *scanState) error {
	var successfulLibs []model.Library

	for _, lib := range state.libraries {
		if lib.LastScanStartedAt.IsZero() {
			// This is a new scan - mark it as started
			err := s.ds.Library(ctx).ScanBegin(lib.ID, state.fullScan)
			if err != nil {
				log.Error(ctx, "Scanner: Error marking scan start", "lib", lib.Name, err)
				state.sendWarning(err.Error())
				continue
			}

			// Reload library to get updated state (timestamps, etc.)
			reloadedLib, err := s.ds.Library(ctx).Get(lib.ID)
			if err != nil {
				log.Error(ctx, "Scanner: Error reloading library", "lib", lib.Name, err)
				state.sendWarning(err.Error())
				continue
			}
			lib = *reloadedLib
		} else {
			// This is a resumed scan
			log.Debug(ctx, "Scanner: Resuming previous scan", "lib", lib.Name,
				"lastScanStartedAt", lib.LastScanStartedAt, "fullScan", lib.FullScanInProgress)
		}

		successfulLibs = append(successfulLibs, lib)
	}

	if len(successfulLibs) == 0 {
		return fmt.Errorf("no libraries available for scanning")
	}

	// Update state with only successfully initialized libraries
	state.libraries = successfulLibs
	return nil
}

// runGC 清理孤儿数据：无文件的曲目、空专辑、无作品的艺人、悬空标注。
// 无变化时跳过——GC 开销不小且此时必然无可清理对象。
// 定向扫描只清理涉及的库，避免误删其他库中因故暂时不可见的数据。
func (s *scannerImpl) runGC(ctx context.Context, state *scanState) func() error {
	return func() error {
		state.sendProgress(&ProgressInfo{ForceUpdate: true})
		return s.ds.WithTx(func(tx model.DataStore) error {
			if state.changesDetected.Load() {
				start := time.Now()

				// For selective scans, extract library IDs to scope GC operations
				var libraryIDs []int
				if state.isSelectiveScan() {
					libraryIDs = slices.Collect(maps.Keys(state.targets))
					log.Debug(ctx, "Scanner: Running selective GC", "libraryIDs", libraryIDs)
				}

				err := tx.GC(ctx, libraryIDs...)
				if err != nil {
					log.Error(ctx, "Scanner: Error running GC", err)
					return fmt.Errorf("running GC: %w", err)
				}
				log.Debug(ctx, "Scanner: GC completed", "elapsed", time.Since(start))
			} else {
				log.Debug(ctx, "Scanner: No changes detected, skipping GC")
			}
			return nil
		}, "scanner: GC")
	}
}

// runRefreshStats 重算艺人统计与标签计数。
// 这两项依赖 GC 后的最终数据，故放在收尾阶段。
func (s *scannerImpl) runRefreshStats(ctx context.Context, state *scanState) func() error {
	return func() error {
		if !state.changesDetected.Load() {
			log.Debug(ctx, "Scanner: No changes detected, skipping refreshing stats")
			return nil
		}
		start := time.Now()
		stats, err := s.ds.Artist(ctx).RefreshStats(state.fullScan)
		if err != nil {
			log.Error(ctx, "Scanner: Error refreshing artists stats", err)
			return fmt.Errorf("refreshing artists stats: %w", err)
		}
		log.Debug(ctx, "Scanner: Refreshed artist stats", "stats", stats, "elapsed", time.Since(start))

		start = time.Now()
		err = s.ds.Tag(ctx).UpdateCounts()
		if err != nil {
			log.Error(ctx, "Scanner: Error updating tag counts", err)
			return fmt.Errorf("updating tag counts: %w", err)
		}
		log.Debug(ctx, "Scanner: Updated tag counts", "elapsed", time.Since(start))
		return nil
	}
}

// runOptimize 执行数据库维护（重整索引、回收空间）。
func (s *scannerImpl) runOptimize(ctx context.Context) func() error {
	return func() error {
		start := time.Now()
		db.Optimize(ctx)
		log.Debug(ctx, "Scanner: Optimized DB", "elapsed", time.Since(start))
		return nil
	}
}

// runUpdateLibraries 标记各库扫描完成，并记录本次使用的 PID 生成规则。
//
// 记录 PID 规则的目的：规则一旦变更，所有曲目/专辑的持久化 ID 都会改变，
// 启动时对比即可判断是否需要强制全量重扫。
// 整个过程在单个事务内完成，避免出现「已标记完成但统计未更新」的中间态。
func (s *scannerImpl) runUpdateLibraries(ctx context.Context, state *scanState) func() error {
	return func() error {
		start := time.Now()
		return s.ds.WithTx(func(tx model.DataStore) error {
			for _, lib := range state.libraries {
				err := tx.Library(ctx).ScanEnd(lib.ID)
				if err != nil {
					log.Error(ctx, "Scanner: Error updating last scan completed", "lib", lib.Name, err)
					return fmt.Errorf("updating last scan completed: %w", err)
				}
				err = tx.Property(ctx).Put(consts.PIDTrackKey, conf.Server.PID.Track)
				if err != nil {
					log.Error(ctx, "Scanner: Error updating track PID conf", err)
					return fmt.Errorf("updating track PID conf: %w", err)
				}
				err = tx.Property(ctx).Put(consts.PIDAlbumKey, conf.Server.PID.Album)
				if err != nil {
					log.Error(ctx, "Scanner: Error updating album PID conf", err)
					return fmt.Errorf("updating album PID conf: %w", err)
				}
				if state.changesDetected.Load() {
					log.Debug(ctx, "Scanner: Refreshing library stats", "lib", lib.Name)
					if err := tx.Library(ctx).RefreshStats(lib.ID); err != nil {
						log.Error(ctx, "Scanner: Error refreshing library stats", "lib", lib.Name, err)
						return fmt.Errorf("refreshing library stats: %w", err)
					}
				} else {
					log.Debug(ctx, "Scanner: No changes detected, skipping library stats refresh", "lib", lib.Name)
				}
			}
			log.Debug(ctx, "Scanner: Updated libraries after scan", "elapsed", time.Since(start), "numLibraries", len(state.libraries))
			return nil
		}, "scanner: update libraries")
	}
}

// phase 是扫描阶段的统一抽象：
// 每个阶段提供一个数据源（producer）与若干处理环节（stages），
// 由流水线框架驱动，泛型参数 T 是该阶段流转的数据类型。
type phase[T any] interface {
	producer() ppl.Producer[T]
	stages() []ppl.Stage[T]
	finalize(error) error
	description() string
}

// runPhase 把一个阶段包装成可被 run.Sequentially 调度的函数。
//
// 在流水线最前面插入计数环节以统计处理量。
// Debug 级别下改用 Measure 收集各环节耗时，便于定位性能瓶颈。
// 无论成败都调用 finalize：阶段需要在此做收尾（如标记缺失记录），
// 且要能感知前序错误以决定是否跳过收尾动作。
func runPhase[T any](ctx context.Context, phaseNum int, phase phase[T]) func() error {
	return func() error {
		log.Debug(ctx, fmt.Sprintf("Scanner: Starting phase %d: %s", phaseNum, phase.description()))
		start := time.Now()

		producer := phase.producer()
		stages := phase.stages()

		// Prepend a counter stage to the phase's pipeline
		counter, countStageFn := countTasks[T]()
		stages = append([]ppl.Stage[T]{ppl.NewStage(countStageFn, ppl.Name("count tasks"))}, stages...)

		var err error
		if log.IsGreaterOrEqualTo(log.LevelDebug) {
			var m *ppl.Metrics
			m, err = ppl.Measure(producer, stages...)
			log.Info(ctx, "Scanner: "+m.String(), err)
		} else {
			err = ppl.Do(producer, stages...)
		}

		err = phase.finalize(err)

		if err != nil {
			log.Error(ctx, fmt.Sprintf("Scanner: Error processing libraries in phase %d", phaseNum), "elapsed", time.Since(start), err)
		} else {
			log.Debug(ctx, fmt.Sprintf("Scanner: Finished phase %d", phaseNum), "elapsed", time.Since(start), "totalTasks", counter.Load())
		}

		return err
	}
}

// countTasks 返回一个计数器与配套的流水线环节，
// 该环节原样透传数据、只做计数（环节可能并发执行，故用原子计数）。
func countTasks[T any]() (*atomic.Int64, func(T) (T, error)) {
	counter := atomic.Int64{}
	return &counter, func(in T) (T, error) {
		counter.Add(1)
		return in, nil
	}
}

var _ scanner = (*scannerImpl)(nil)
