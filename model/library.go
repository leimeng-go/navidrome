package model

import (
	"time"

	"github.com/navidrome/navidrome/utils/slice"
)

// Library 代表一个音乐库（一个被扫描的根目录）。
// Navidrome 支持多库，用户可被授权访问其中的若干个。
type Library struct {
	ID   int    `json:"id" db:"id"`
	Name string `json:"name" db:"name"`
	// Path 是库在服务端的绝对路径
	Path string `json:"path" db:"path"`
	// RemotePath 用于路径映射：当客户端（如 Jukebox 或外部播放器）看到的
	// 路径前缀与服务端不同时，用它做替换
	RemotePath string `json:"remotePath" db:"remote_path"`
	// LastScanAt 上次扫描完成时间；LastScanStartedAt 上次扫描开始时间。
	// 两者配合 FullScanInProgress 可判断上一次扫描是否被中断
	LastScanAt        time.Time `json:"lastScanAt" db:"last_scan_at"`
	LastScanStartedAt time.Time `json:"lastScanStartedAt" db:"last_scan_started_at"`
	// FullScanInProgress 标记正在进行全量扫描；若进程异常退出，
	// 下次启动据此判定需要重做全量扫描
	FullScanInProgress bool      `json:"fullScanInProgress" db:"full_scan_in_progress"`
	UpdatedAt          time.Time `json:"updatedAt" db:"updated_at"`
	CreatedAt          time.Time `json:"createdAt" db:"created_at"`
	// Total* 为该库的汇总统计，由 RefreshStats 在扫描收尾时重算
	TotalSongs        int     `json:"totalSongs" db:"total_songs"`
	TotalAlbums       int     `json:"totalAlbums" db:"total_albums"`
	TotalArtists      int     `json:"totalArtists" db:"total_artists"`
	TotalFolders      int     `json:"totalFolders" db:"total_folders"`
	TotalFiles        int     `json:"totalFiles" db:"total_files"`
	TotalMissingFiles int     `json:"totalMissingFiles" db:"total_missing_files"`
	TotalSize         int64   `json:"totalSize" db:"total_size"`
	TotalDuration     float64 `json:"totalDuration" db:"total_duration"`
	// DefaultNewUsers 为 true 时，新建用户默认获得该库的访问权限
	DefaultNewUsers bool `json:"defaultNewUsers" db:"default_new_users"`
}

const (
	// DefaultLibraryID 是首次启动时自动创建的默认库 ID，
	// 单库部署与旧版本升级场景均依赖这个固定值
	DefaultLibraryID   = 1
	DefaultLibraryName = "Music Library"
)

type Libraries []Library

// IDs 提取集合中所有库的 ID，便于作为查询过滤条件传递。
func (l Libraries) IDs() []int {
	return slice.Map(l, func(lib Library) int { return lib.ID })
}

// LibraryRepository 是音乐库仓储接口。
type LibraryRepository interface {
	Get(id int) (*Library, error)
	// GetPath returns the path of the library with the given ID.
	// Its implementation must be optimized to avoid unnecessary queries.
	// GetPath 返回指定库的路径。该方法在扫描与流媒体路径拼接中被高频调用，
	// 实现必须做缓存优化，避免每次都查库。
	GetPath(id int) (string, error)
	GetAll(...QueryOptions) (Libraries, error)
	CountAll(...QueryOptions) (int64, error)
	Put(*Library) error
	Delete(id int) error
	// StoreMusicFolder 把配置文件中的 MusicFolder 同步为默认库的路径
	StoreMusicFolder() error
	// AddArtist 建立库与艺人的关联，用于按库过滤艺人列表
	AddArtist(id int, artistID string) error

	// User-library association methods
	// 用户与库的关联查询
	GetUsersWithLibraryAccess(libraryID int) (Users, error)

	// TODO These methods should be moved to a core service
	// TODO 以下扫描状态管理方法应迁移到 core 服务层，仓储不应承担业务流程职责
	// ScanBegin 记录扫描开始，并标记是否为全量扫描
	ScanBegin(id int, fullScan bool) error
	// ScanEnd 记录扫描结束，清除进行中标记
	ScanEnd(id int) error
	// ScanInProgress 判断是否有任意库正在扫描（用于串行化扫描请求）
	ScanInProgress() (bool, error)
	// RefreshStats 重算该库的汇总统计
	RefreshStats(id int) error
}
