package model

import (
	"fmt"
	"iter"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/navidrome/navidrome/model/id"
)

// Folder represents a folder in the library. Its path is relative to the library root.
// ALWAYS use NewFolder to create a new instance.
// Folder 代表库中的一个目录，Path 相对于库根目录。
// 请务必使用 NewFolder 构造实例，因为 ID 与 ParentID 需按固定规则派生，手工赋值易出错。
type Folder struct {
	ID        string `structs:"id"`
	LibraryID int    `structs:"library_id"`
	// LibraryPath 是所属库的绝对根路径，运行时填充，不入库
	LibraryPath string `structs:"-" json:"-" hash:"ignore"`
	// Path 是父目录路径（相对库根），Name 是本级目录名，两者拼接才是完整相对路径
	Path     string `structs:"path"`
	Name     string `structs:"name"`
	ParentID string `structs:"parent_id"`
	// NumAudioFiles/NumPlaylists 记录目录内的音频与播放列表文件数量，
	// 扫描器据此跳过无需处理的目录（如 Phase 4 只看有播放列表的目录）
	NumAudioFiles int      `structs:"num_audio_files"`
	NumPlaylists  int      `structs:"num_playlists"`
	ImageFiles    []string `structs:"image_files"` // 目录内的图片文件名，用作封面候选
	// ImagesUpdatedAt 图片文件的最新修改时间，用于封面缓存失效判断
	ImagesUpdatedAt time.Time `structs:"images_updated_at"`
	// Hash 是目录内容指纹，与上次扫描结果比对即可判断该目录是否需要重新处理
	Hash      string    `structs:"hash"`
	Missing   bool      `structs:"missing"` // 目录已从磁盘消失（软删除标记）
	UpdateAt  time.Time `structs:"updated_at"`
	CreatedAt time.Time `structs:"created_at"`
}

// AbsolutePath 返回目录在磁盘上的绝对路径。
func (f Folder) AbsolutePath() string {
	return filepath.Join(f.LibraryPath, f.Path, f.Name)
}

// String 主要用于日志与调试，返回绝对路径。
func (f Folder) String() string {
	return f.AbsolutePath()
}

// FolderID generates a unique ID for a folder in a library.
// The ID is generated based on the library ID and the folder path relative to the library root.
// Any leading or trailing slashes are removed from the folder path.
// FolderID 由「库 ID + 库内相对路径」哈希生成目录 ID。
// 先剥离库路径前缀与分隔符再 Clean，确保同一目录无论以何种形式传入都得到相同 ID。
func FolderID(lib Library, path string) string {
	path = strings.TrimPrefix(path, lib.Path)
	path = strings.TrimPrefix(path, string(os.PathSeparator))
	path = filepath.Clean(path)
	key := fmt.Sprintf("%d:%s", lib.ID, path)
	return id.NewHash(key)
}

// NewFolder 构造目录实例，自动派生 ID 与父目录 ID。
// 特殊情况：库根目录本身（dir 与 name 均为 "."）没有父目录，ParentID 置空。
func NewFolder(lib Library, folderPath string) *Folder {
	newID := FolderID(lib, folderPath)
	dir, name := path.Split(folderPath)
	dir = path.Clean(dir)
	var parentID string
	if dir == "." && name == "." {
		dir = ""
		parentID = ""
	} else {
		parentID = FolderID(lib, dir)
	}
	return &Folder{
		LibraryID:  lib.ID,
		ID:         newID,
		Path:       dir,
		Name:       name,
		ParentID:   parentID,
		ImageFiles: []string{},
		UpdateAt:   time.Now(),
		CreatedAt:  time.Now(),
	}
}

// FolderCursor 是目录的流式游标，用于逐条处理海量目录而不全量载入内存。
type FolderCursor iter.Seq2[Folder, error]

// FolderUpdateInfo 是目录的轻量变更信息，扫描器只取这两个字段
// 即可判断目录是否需要重新处理，无需载入完整 Folder。
type FolderUpdateInfo struct {
	UpdatedAt time.Time
	Hash      string
}

// FolderRepository 是目录仓储接口。
type FolderRepository interface {
	Get(id string) (*Folder, error)
	GetByPath(lib Library, path string) (*Folder, error)
	GetAll(...QueryOptions) ([]Folder, error)
	CountAll(...QueryOptions) (int64, error)
	// GetFolderUpdateInfo 批量获取指定目录的变更信息，返回值以路径为键。
	// 扫描器用它一次性拿到整批目录的状态，避免逐个查询
	GetFolderUpdateInfo(lib Library, targetPaths ...string) (map[string]FolderUpdateInfo, error)
	Put(*Folder) error
	// MarkMissing 批量设置/取消目录的丢失标记
	MarkMissing(missing bool, ids ...string) error
	// GetTouchedWithPlaylists 返回本次扫描被触及且含播放列表文件的目录，供 Phase 4 消费
	GetTouchedWithPlaylists() (FolderCursor, error)
}
