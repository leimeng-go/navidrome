package scanner

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Masterminds/squirrel"
	ppl "github.com/google/go-pipeline/pkg/pipeline"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/metadata"
	"github.com/navidrome/navidrome/utils"
	"github.com/navidrome/navidrome/utils/pl"
	"github.com/navidrome/navidrome/utils/slice"
)

// createPhaseFolders 为每个音乐库创建扫描任务并组装阶段 1。
// 单个库初始化失败只发警告并跳过，不影响其他库。
func createPhaseFolders(ctx context.Context, state *scanState, ds model.DataStore, cw artwork.CacheWarmer) *phaseFolders {
	var jobs []*scanJob

	// Create scan jobs for all libraries
	for _, lib := range state.libraries {
		// Get target folders for this library if selective scan
		var targetFolders []string
		if state.isSelectiveScan() {
			targetFolders = state.targets[lib.ID]
		}

		job, err := newScanJob(ctx, ds, cw, lib, state.fullScan, targetFolders)
		if err != nil {
			log.Error(ctx, "Scanner: Error creating scan context", "lib", lib.Name, err)
			state.sendWarning(err.Error())
			continue
		}
		jobs = append(jobs, job)
	}

	return &phaseFolders{jobs: jobs, ctx: ctx, ds: ds, state: state}
}

// scanJob 是单个音乐库的扫描任务，持有该库的文件系统与目录更新信息。
type scanJob struct {
	lib           model.Library
	fs            storage.MusicFS
	cw            artwork.CacheWarmer
	lastUpdates   map[string]model.FolderUpdateInfo // Holds last update info for all (DB) folders in this library
	targetFolders []string                          // Specific folders to scan (including all descendants)
	lock          sync.Mutex                        // 保护 lastUpdates，遍历过程并发访问
	numFolders    atomic.Int64
}

// newScanJob 创建库扫描任务。
// 预先一次性载入全库目录的更新信息，遍历时逐个比对，
// 避免每个目录都查一次数据库。
func newScanJob(ctx context.Context, ds model.DataStore, cw artwork.CacheWarmer, lib model.Library, fullScan bool, targetFolders []string) (*scanJob, error) {
	// Get folder updates, optionally filtered to specific target folders
	lastUpdates, err := ds.Folder(ctx).GetFolderUpdateInfo(lib, targetFolders...)
	if err != nil {
		return nil, fmt.Errorf("getting last updates: %w", err)
	}

	fileStore, err := storage.For(lib.Path)
	if err != nil {
		log.Error(ctx, "Error getting storage for library", "library", lib.Name, "path", lib.Path, err)
		return nil, fmt.Errorf("getting storage for library: %w", err)
	}
	fsys, err := fileStore.FS()
	if err != nil {
		log.Error(ctx, "Error getting fs for library", "library", lib.Name, "path", lib.Path, err)
		return nil, fmt.Errorf("getting fs for library: %w", err)
	}

	// Ensure FullScanInProgress reflects the current scan request.
	// This is important when resuming an interrupted quick scan as a full scan:
	// the DB may have FullScanInProgress=false, but we need it true for isOutdated() to work correctly.
	lib.FullScanInProgress = lib.FullScanInProgress || fullScan

	return &scanJob{
		lib:           lib,
		fs:            fsys,
		cw:            cw,
		lastUpdates:   lastUpdates,
		targetFolders: targetFolders,
	}, nil
}

// popLastUpdate retrieves and removes the last update info for the given folder ID
// This is used to track which folders have been found during the walk_dir_tree
//
// popLastUpdate 取出并移除指定目录的更新信息。
// 「取出即删除」是关键设计：遍历结束后 lastUpdates 中残留的条目，
// 就是数据库中有、磁盘上却不存在的目录，即已被删除的目录。
func (j *scanJob) popLastUpdate(folderID string) model.FolderUpdateInfo {
	j.lock.Lock()
	defer j.lock.Unlock()

	lastUpdate := j.lastUpdates[folderID]
	delete(j.lastUpdates, folderID)
	return lastUpdate
}

// createFolderEntry creates a new folderEntry for the given path, using the last update info from the job
// to populate the previous update time and hash. It also removes the folder from the job's lastUpdates map.
// This is used to track which folders have been found during the walk_dir_tree.
func (j *scanJob) createFolderEntry(path string) *folderEntry {
	id := model.FolderID(j.lib, path)
	info := j.popLastUpdate(id)
	return newFolderEntry(j, id, path, info.UpdatedAt, info.Hash)
}

// phaseFolders represents the first phase of the scanning process, which is responsible
// for scanning all libraries and importing new or updated files. This phase involves
// traversing the directory tree of each library, identifying new or modified media files,
// and updating the database with the relevant information.
//
// The phaseFolders struct holds the context, data store, and jobs required for the scanning
// process. Each job represents a library being scanned, and contains information about the
// library, file system, and the last updates of the folders.
//
// The phaseFolders struct implements the phase interface, providing methods to produce
// folder entries, process folders, persist changes to the database, and log the results.
//
// phaseFolders 是扫描的第一阶段：遍历目录树，读取元数据，
// 将新增/变更的曲目、专辑、艺人写入数据库。
type phaseFolders struct {
	jobs             []*scanJob
	ds               model.DataStore
	ctx              context.Context
	state            *scanState
	prevAlbumPIDConf string // 上次扫描使用的专辑 PID 规则，用于识别 ID 变更
}

func (p *phaseFolders) description() string {
	return "Scan all libraries and import new/updated files"
}

// producer 遍历各库目录树，只把「过期」的目录送入流水线。
//
// 判定过期的依据是目录修改时间与内容哈希（见 folderEntry.isOutdated）；
// 未变化的目录直接跳过，这是增量扫描高效的关键。
//
// 增量扫描下，新出现但没有任何文件的空目录也会跳过——
// 它们通常是用户刚建的临时目录，入库徒增噪声。
func (p *phaseFolders) producer() ppl.Producer[*folderEntry] {
	return ppl.NewProducer(func(put func(entry *folderEntry)) error {
		var err error
		p.prevAlbumPIDConf, err = p.ds.Property(p.ctx).DefaultGet(consts.PIDAlbumKey, "")
		if err != nil {
			return fmt.Errorf("getting album PID conf: %w", err)
		}

		// TODO Parallelize multiple job when we have multiple libraries
		var total int64
		var totalChanged int64
		for _, job := range p.jobs {
			if utils.IsCtxDone(p.ctx) {
				break
			}

			outputChan, err := walkDirTree(p.ctx, job, job.targetFolders...)
			if err != nil {
				log.Warn(p.ctx, "Scanner: Error scanning library", "lib", job.lib.Name, err)
			}
			for folder := range pl.ReadOrDone(p.ctx, outputChan) {
				job.numFolders.Add(1)
				p.state.sendProgress(&ProgressInfo{
					LibID:     job.lib.ID,
					FileCount: uint32(len(folder.audioFiles)),
					Path:      folder.path,
					Phase:     "1",
				})

				// Log folder info
				log.Trace(p.ctx, "Scanner: Checking folder state", " folder", folder.path, "_updTime", folder.updTime,
					"_modTime", folder.modTime, "_lastScanStartedAt", folder.job.lib.LastScanStartedAt,
					"numAudioFiles", len(folder.audioFiles), "numImageFiles", len(folder.imageFiles),
					"numPlaylists", folder.numPlaylists, "numSubfolders", folder.numSubFolders)

				// Check if folder is outdated
				if folder.isOutdated() {
					if !p.state.fullScan {
						if folder.hasNoFiles() && folder.isNew() {
							log.Trace(p.ctx, "Scanner: Skipping new folder with no files", "folder", folder.path, "lib", job.lib.Name)
							continue
						}
						log.Debug(p.ctx, "Scanner: Detected changes in folder", "folder", folder.path, "lastUpdate", folder.modTime, "lib", job.lib.Name)
					}
					totalChanged++
					folder.elapsed.Stop()
					put(folder)
				} else {
					log.Trace(p.ctx, "Scanner: Skipping up-to-date folder", "folder", folder.path, "lastUpdate", folder.modTime, "lib", job.lib.Name)
				}
			}
			total += job.numFolders.Load()
		}
		log.Debug(p.ctx, "Scanner: Finished loading all folders", "numFolders", total, "numChanged", totalChanged)
		return nil
	}, ppl.Name("traverse filesystem"))
}

// measure 记录该目录在当前环节的耗时，用于性能统计。
func (p *phaseFolders) measure(entry *folderEntry) func() time.Duration {
	entry.elapsed.Start()
	return func() time.Duration { return entry.elapsed.Stop() }
}

// stages 定义阶段 1 的流水线环节。
// 只有元数据读取环节并发执行（IO 密集）；
// 入库环节串行，避免大量并发写事务争抢 SQLite 写锁。
func (p *phaseFolders) stages() []ppl.Stage[*folderEntry] {
	return []ppl.Stage[*folderEntry]{
		ppl.NewStage(p.processFolder, ppl.Name("process folder"), ppl.Concurrency(conf.Server.DevScannerThreads)),
		ppl.NewStage(p.persistChanges, ppl.Name("persist changes")),
		ppl.NewStage(p.logFolder, ppl.Name("log results")),
	}
}

// processFolder 处理单个目录：比对磁盘与数据库，读取需导入文件的元数据。
//
// 通过「从 dbTracks 中逐个删除已在磁盘上找到的曲目」来求差集，
// 剩余项即为磁盘上已消失的曲目，标记为缺失交由阶段 2 判断是否只是移动。
//
// 增量扫描下，仅当文件修改时间晚于数据库记录、或该曲目此前被标记缺失时才重读元数据。
// 元数据读取失败只发警告并返回，不中断整轮扫描——
// 单个损坏文件不应导致整个库无法扫描。
func (p *phaseFolders) processFolder(entry *folderEntry) (*folderEntry, error) {
	defer p.measure(entry)()

	// Load children mediafiles from DB
	cursor, err := p.ds.MediaFile(p.ctx).GetCursor(model.QueryOptions{
		Filters: squirrel.And{squirrel.Eq{"folder_id": entry.id}},
	})
	if err != nil {
		log.Error(p.ctx, "Scanner: Error loading mediafiles from DB", "folder", entry.path, err)
		return entry, err
	}
	dbTracks := make(map[string]*model.MediaFile)
	for mf, err := range cursor {
		if err != nil {
			log.Error(p.ctx, "Scanner: Error loading mediafiles from DB", "folder", entry.path, err)
			return entry, err
		}
		dbTracks[mf.Path] = &mf
	}

	// Get list of files to import, based on modtime (or all if fullScan),
	// leave in dbTracks only tracks that are missing (not found in the FS)
	filesToImport := make(map[string]*model.MediaFile, len(entry.audioFiles))
	for afPath, af := range entry.audioFiles {
		fullPath := path.Join(entry.path, afPath)
		dbTrack, foundInDB := dbTracks[fullPath]
		if !foundInDB || p.state.fullScan {
			filesToImport[fullPath] = dbTrack
		} else {
			info, err := af.Info()
			if err != nil {
				log.Warn(p.ctx, "Scanner: Error getting file info", "folder", entry.path, "file", af.Name(), err)
				p.state.sendWarning(fmt.Sprintf("Error getting file info for %s/%s: %v", entry.path, af.Name(), err))
				return entry, nil
			}
			if info.ModTime().After(dbTrack.UpdatedAt) || dbTrack.Missing {
				filesToImport[fullPath] = dbTrack
			}
		}
		delete(dbTracks, fullPath)
	}

	// Remaining dbTracks are tracks that were not found in the FS, so they should be marked as missing
	entry.missingTracks = slices.Collect(maps.Values(dbTracks))

	// Load metadata from files that need to be imported
	if len(filesToImport) > 0 {
		err = p.loadTagsFromFiles(entry, filesToImport)
		if err != nil {
			log.Warn(p.ctx, "Scanner: Error loading tags from files. Skipping", "folder", entry.path, err)
			p.state.sendWarning(fmt.Sprintf("Error loading tags from files in %s: %v", entry.path, err))
			return entry, nil
		}

		p.createAlbumsFromMediaFiles(entry)
		p.createArtistsFromMediaFiles(entry)
	}

	return entry, nil
}

// 每批读取的文件数，控制单次内存占用
const filesBatchSize = 200

// loadTagsFromFiles reads metadata from the files in the given list and populates
// the entry's tracks and tags with the results.
//
// loadTagsFromFiles 批量读取文件标签并转换为曲目与标签集合。
//
// 同时追踪专辑 ID 的变化：当 PID 生成规则调整或专辑元数据被修改时，
// 同一张专辑会得到新的 ID，需记录「新 ID → 旧 ID」的映射，
// 以便入库时把评分、播放次数等标注迁移过去。
// 旧 ID 优先取数据库中的既有值；文件是新增的则用上次的 PID 规则现场推算。
func (p *phaseFolders) loadTagsFromFiles(entry *folderEntry, toImport map[string]*model.MediaFile) error {
	tracks := make([]model.MediaFile, 0, len(toImport))
	uniqueTags := make(map[string]model.Tag, len(toImport))
	for chunk := range slice.CollectChunks(maps.Keys(toImport), filesBatchSize) {
		allInfo, err := entry.job.fs.ReadTags(chunk...)
		if err != nil {
			log.Warn(p.ctx, "Scanner: Error extracting metadata from files. Skipping", "folder", entry.path, err)
			return err
		}
		for filePath, info := range allInfo {
			md := metadata.New(filePath, info)
			track := md.ToMediaFile(entry.job.lib.ID, entry.id)
			tracks = append(tracks, track)
			for _, t := range track.Tags.FlattenAll() {
				uniqueTags[t.ID] = t
			}

			// Keep track of any album ID changes, to reassign annotations later
			prevAlbumID := ""
			if prev := toImport[filePath]; prev != nil {
				prevAlbumID = prev.AlbumID
			} else {
				prevAlbumID = md.AlbumID(track, p.prevAlbumPIDConf)
			}
			_, ok := entry.albumIDMap[track.AlbumID]
			if prevAlbumID != track.AlbumID && !ok {
				entry.albumIDMap[track.AlbumID] = prevAlbumID
			}
		}
	}
	entry.tracks = tracks
	entry.tags = slices.Collect(maps.Values(uniqueTags))
	return nil
}

// createAlbumsFromMediaFiles groups the entry's tracks by album ID and creates albums
// createAlbumsFromMediaFiles 按专辑 ID 分组曲目并生成专辑记录。
// 此处得到的专辑信息可能不完整（只涵盖本目录内的曲目），
// 跨目录的完整信息由阶段 3 汇总。
func (p *phaseFolders) createAlbumsFromMediaFiles(entry *folderEntry) {
	grouped := slice.Group(entry.tracks, func(mf model.MediaFile) string { return mf.AlbumID })
	albums := make(model.Albums, 0, len(grouped))
	for _, group := range grouped {
		songs := model.MediaFiles(group)
		album := songs.ToAlbum()
		albums = append(albums, album)
	}
	entry.albums = albums
}

// createArtistsFromMediaFiles creates artists from the entry's tracks
// createArtistsFromMediaFiles 合并各曲目的参与者，得到本目录涉及的全部艺人。
func (p *phaseFolders) createArtistsFromMediaFiles(entry *folderEntry) {
	participants := make(model.Participants, len(entry.tracks)*3) // preallocate ~3 artists per track
	for _, track := range entry.tracks {
		participants.Merge(track.Participants)
	}
	entry.artists = participants.AllArtists()
}

// persistChanges 在单个事务内写入本目录的所有变更。
//
// 以目录为事务边界：粒度太细会因频繁提交拖慢速度，
// 太粗则中断后需要重做的工作量过大。
//
// 艺人与专辑此时只写入部分字段，信息尚不完整——
// 跨目录的完整数据要等阶段 3 汇总后才能确定。
//
// 封面预热延迟到事务提交后进行：
// 它会读取数据库，在事务内触发可能造成自我阻塞。
func (p *phaseFolders) persistChanges(entry *folderEntry) (*folderEntry, error) {
	defer p.measure(entry)()
	p.state.changesDetected.Store(true)

	// Collect artwork IDs to pre-cache after the transaction commits
	var artworkIDs []model.ArtworkID

	err := p.ds.WithTx(func(tx model.DataStore) error {
		// Instantiate all repositories just once per folder
		folderRepo := tx.Folder(p.ctx)
		tagRepo := tx.Tag(p.ctx)
		artistRepo := tx.Artist(p.ctx)
		libraryRepo := tx.Library(p.ctx)
		albumRepo := tx.Album(p.ctx)
		mfRepo := tx.MediaFile(p.ctx)

		// Save folder to DB
		folder := entry.toFolder()
		err := folderRepo.Put(folder)
		if err != nil {
			log.Error(p.ctx, "Scanner: Error persisting folder to DB", "folder", entry.path, err)
			return err
		}

		// Save all tags to DB
		err = tagRepo.Add(entry.job.lib.ID, entry.tags...)
		if err != nil {
			log.Error(p.ctx, "Scanner: Error persisting tags to DB", "folder", entry.path, err)
			return err
		}

		// Save all new/modified artists to DB. Their information will be incomplete, but they will be refreshed later
		for i := range entry.artists {
			err = artistRepo.Put(&entry.artists[i], "name",
				"mbz_artist_id", "sort_artist_name", "order_artist_name", "full_text", "updated_at")
			if err != nil {
				log.Error(p.ctx, "Scanner: Error persisting artist to DB", "folder", entry.path, "artist", entry.artists[i].Name, err)
				return err
			}
			err = libraryRepo.AddArtist(entry.job.lib.ID, entry.artists[i].ID)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error adding artist to library", "lib", entry.job.lib.ID, "artist", entry.artists[i].Name, err)
				return err
			}
			if entry.artists[i].Name != consts.UnknownArtist && entry.artists[i].Name != consts.VariousArtists {
				artworkIDs = append(artworkIDs, entry.artists[i].CoverArtID())
			}
		}

		// Save all new/modified albums to DB. Their information will be incomplete, but they will be refreshed later
		for i := range entry.albums {
			err = p.persistAlbum(albumRepo, &entry.albums[i], entry.albumIDMap)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error persisting album to DB", "folder", entry.path, "album", entry.albums[i], err)
				return err
			}
			if entry.albums[i].Name != consts.UnknownAlbum {
				artworkIDs = append(artworkIDs, entry.albums[i].CoverArtID())
			}
		}

		// Save all tracks to DB
		for i := range entry.tracks {
			err = mfRepo.Put(&entry.tracks[i])
			if err != nil {
				log.Error(p.ctx, "Scanner: Error persisting mediafile to DB", "folder", entry.path, "track", entry.tracks[i], err)
				return err
			}
		}

		// Mark all missing tracks as not available
		// 标记缺失曲目。这里只标记不删除：
		// 阶段 2 还要用它们判断文件是否只是被移动了位置
		if len(entry.missingTracks) > 0 {
			err = mfRepo.MarkMissing(true, entry.missingTracks...)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error marking missing tracks", "folder", entry.path, err)
				return err
			}

			// Touch all albums that have missing tracks, so they get refreshed in later phases
			groupedMissingTracks := slice.ToMap(entry.missingTracks, func(mf *model.MediaFile) (string, struct{}) {
				return mf.AlbumID, struct{}{}
			})
			albumsToUpdate := slices.Collect(maps.Keys(groupedMissingTracks))
			err = albumRepo.Touch(albumsToUpdate...)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error touching album", "folder", entry.path, "albums", albumsToUpdate, err)
				return err
			}
		}
		return nil
	}, "scanner: persist changes")
	if err != nil {
		log.Error(p.ctx, "Scanner: Error persisting changes to DB", "folder", entry.path, err)
	}

	// Pre-cache artwork after the transaction commits successfully
	if err == nil {
		for _, artID := range artworkIDs {
			entry.job.cw.PreCache(artID)
		}
	}

	return entry, err
}

// persistAlbum persists the given album to the database, and reassigns annotations from the previous album ID
//
// persistAlbum 写入专辑，并在专辑 ID 发生变化时迁移历史数据。
//
// 需要迁移两类信息：
//   - 用户标注（评分、星标、播放次数）——丢失会直接影响用户体验；
//   - created_at 字段——保持「最近添加」列表的排序稳定，
//     否则修改元数据会让老专辑跳到列表最前。
//
// 迁移失败只记警告不中断：专辑本身已写入成功，
// 丢失标注虽不理想，但比整个扫描失败要好。
func (p *phaseFolders) persistAlbum(repo model.AlbumRepository, a *model.Album, idMap map[string]string) error {
	prevID := idMap[a.ID]
	log.Trace(p.ctx, "Persisting album", "album", a.Name, "albumArtist", a.AlbumArtist, "id", a.ID, "prevID", cmp.Or(prevID, "nil"))
	if err := repo.Put(a); err != nil {
		return fmt.Errorf("persisting album %s: %w", a.ID, err)
	}
	if prevID == "" {
		return nil
	}

	// Reassign annotation from previous album to new album
	log.Trace(p.ctx, "Reassigning album annotations", "from", prevID, "to", a.ID, "album", a.Name)
	if err := repo.ReassignAnnotation(prevID, a.ID); err != nil {
		log.Warn(p.ctx, "Scanner: Could not reassign annotations", "from", prevID, "to", a.ID, "album", a.Name, err)
		p.state.sendWarning(fmt.Sprintf("Could not reassign annotations from %s to %s ('%s'): %v", prevID, a.ID, a.Name, err))
	}

	// Keep created_at field from previous instance of the album
	if err := repo.CopyAttributes(prevID, a.ID, "created_at"); err != nil {
		// Silently ignore when the previous album is not found
		if !errors.Is(err, model.ErrNotFound) {
			log.Warn(p.ctx, "Scanner: Could not copy fields", "from", prevID, "to", a.ID, "album", a.Name, err)
			p.state.sendWarning(fmt.Sprintf("Could not copy fields from %s to %s ('%s'): %v", prevID, a.ID, a.Name, err))
		}
	}
	// Don't keep track of this mapping anymore
	delete(idMap, a.ID)
	return nil
}

// logFolder 输出目录处理结果。空目录降级为 Trace，避免刷屏。
func (p *phaseFolders) logFolder(entry *folderEntry) (*folderEntry, error) {
	logCall := log.Info
	if entry.isEmpty() {
		logCall = log.Trace
	}
	logCall(p.ctx, "Scanner: Completed processing folder",
		"audioCount", len(entry.audioFiles), "imageCount", len(entry.imageFiles), "plsCount", entry.numPlaylists,
		"elapsed", entry.elapsed.Elapsed(), "tracksMissing", len(entry.missingTracks),
		"tracksImported", len(entry.tracks), "library", entry.job.lib.Name, consts.Zwsp+"folder", entry.path)
	return entry, nil
}

// finalize 处理磁盘上已消失的目录。
//
// 遍历过程中每找到一个目录就从 lastUpdates 移除，
// 故此时残留的键就是数据库中有、磁盘上已不存在的目录，
// 将其及其下曲目一并标记为缺失，并触碰相关专辑以便后续阶段刷新。
//
// 无论前序是否出错都要执行，故用 errors.Join 合并两处错误。
func (p *phaseFolders) finalize(err error) error {
	errF := p.ds.WithTx(func(tx model.DataStore) error {
		for _, job := range p.jobs {
			// Mark all folders that were not updated as missing
			if len(job.lastUpdates) == 0 {
				continue
			}
			folderIDs := slices.Collect(maps.Keys(job.lastUpdates))
			err := tx.Folder(p.ctx).MarkMissing(true, folderIDs...)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error marking missing folders", "lib", job.lib.Name, err)
				return err
			}
			err = tx.MediaFile(p.ctx).MarkMissingByFolder(true, folderIDs...)
			if err != nil {
				log.Error(p.ctx, "Scanner: Error marking tracks in missing folders", "lib", job.lib.Name, err)
				return err
			}
			// Touch all albums that have missing folders, so they get refreshed in later phases
			_, err = tx.Album(p.ctx).TouchByMissingFolder()
			if err != nil {
				log.Error(p.ctx, "Scanner: Error touching albums with missing folders", "lib", job.lib.Name, err)
				return err
			}
		}
		return nil
	}, "scanner: finalize phaseFolders")
	return errors.Join(err, errF)
}

var _ phase[*folderEntry] = (*phaseFolders)(nil)
