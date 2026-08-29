package scanner

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"slices"
	"time"

	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/chrono"
)

// newFolderEntry 创建目录条目。
// updTime 与 hash 来自数据库中的既有记录，用于后续判断目录是否变化。
func newFolderEntry(job *scanJob, id, path string, updTime time.Time, hash string) *folderEntry {
	f := &folderEntry{
		id:         id,
		job:        job,
		path:       path,
		audioFiles: make(map[string]fs.DirEntry),
		imageFiles: make(map[string]fs.DirEntry),
		albumIDMap: make(map[string]string),
		updTime:    updTime,
		prevHash:   hash,
	}
	return f
}

// folderEntry 是流经阶段 1 流水线的数据单元，
// 汇聚了某个目录的磁盘现状（文件列表、修改时间）
// 与处理中间产物（曲目、专辑、艺人、标签）。
type folderEntry struct {
	job             *scanJob
	elapsed         chrono.Meter
	path            string    // Full path
	id              string    // DB ID
	modTime         time.Time // From FS
	updTime         time.Time // from DB
	audioFiles      map[string]fs.DirEntry
	imageFiles      map[string]fs.DirEntry
	numPlaylists    int
	numSubFolders   int
	imagesUpdatedAt time.Time
	prevHash        string // Previous hash from DB
	tracks          model.MediaFiles
	albums          model.Albums
	albumIDMap      map[string]string // 新专辑 ID → 旧专辑 ID，用于迁移标注
	artists         model.Artists
	tags            model.TagList
	missingTracks   []*model.MediaFile
}

// hasNoFiles 判断目录内是否没有任何相关文件（可能仍有子目录）。
func (f *folderEntry) hasNoFiles() bool {
	return len(f.audioFiles) == 0 && len(f.imageFiles) == 0 && f.numPlaylists == 0
}

// isEmpty 判断目录是否完全为空（既无文件也无子目录）。
func (f *folderEntry) isEmpty() bool {
	return f.hasNoFiles() && f.numSubFolders == 0
}

// isNew 判断该目录此前是否未入库。
func (f *folderEntry) isNew() bool {
	return f.updTime.IsZero()
}

// isOutdated 判断目录是否需要重新处理。
//
// 全量扫描下，只要该目录在本轮尚未处理过就算过期
// （用「更新时间早于本轮扫描开始时间」判断），
// 这样中断后续扫能跳过已完成的目录。
//
// 其余情况比对内容哈希：它涵盖文件清单、大小与修改时间，
// 比单看目录 mtime 可靠——某些文件系统在文件内容变更时不更新目录 mtime。
func (f *folderEntry) isOutdated() bool {
	if f.job.lib.FullScanInProgress && f.updTime.Before(f.job.lib.LastScanStartedAt) {
		return true
	}
	return f.prevHash != f.hash()
}

// toFolder 转换为可入库的目录模型。
// 播放列表数量只在该目录位于配置的播放列表路径内时才记录，
// 避免把音乐目录里的 m3u 文件误当作待导入列表。
func (f *folderEntry) toFolder() *model.Folder {
	folder := model.NewFolder(f.job.lib, f.path)
	folder.NumAudioFiles = len(f.audioFiles)
	if core.InPlaylistsPath(*folder) {
		folder.NumPlaylists = f.numPlaylists
	}
	folder.ImageFiles = slices.Collect(maps.Keys(f.imageFiles))
	folder.ImagesUpdatedAt = f.imagesUpdatedAt
	folder.Hash = f.hash()
	return folder
}

// hash 计算目录内容指纹，用于增量扫描时判断是否有实质变化。
//
// 指纹涵盖目录自身属性与每个音频/图片文件的名称、大小、修改时间。
// 文件名必须先排序：map 遍历顺序随机，不排序会导致同样内容算出不同哈希。
// 取不到文件信息时跳过该项而非报错——文件可能在遍历期间被删除。
func (f *folderEntry) hash() string {
	h := md5.New()
	_, _ = fmt.Fprintf(
		h,
		"%s:%d:%d:%s",
		f.modTime.UTC(),
		f.numPlaylists,
		f.numSubFolders,
		f.imagesUpdatedAt.UTC(),
	)

	// Sort the keys of audio and image files to ensure consistent hashing
	audioKeys := slices.Collect(maps.Keys(f.audioFiles))
	slices.Sort(audioKeys)
	imageKeys := slices.Collect(maps.Keys(f.imageFiles))
	slices.Sort(imageKeys)

	// Include audio files with their size and modtime
	for _, key := range audioKeys {
		_, _ = io.WriteString(h, key)
		if info, err := f.audioFiles[key].Info(); err == nil {
			_, _ = fmt.Fprintf(h, ":%d:%s", info.Size(), info.ModTime().UTC().String())
		}
	}

	// Include image files with their size and modtime
	for _, key := range imageKeys {
		_, _ = io.WriteString(h, key)
		if info, err := f.imageFiles[key].Info(); err == nil {
			_, _ = fmt.Fprintf(h, ":%d:%s", info.Size(), info.ModTime().UTC().String())
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}
