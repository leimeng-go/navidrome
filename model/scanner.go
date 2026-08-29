package model

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ScanTarget represents a specific folder within a library to be scanned.
// NOTE: This struct is used as a map key, so it should only contain comparable types.
// ScanTarget 指定一个待扫描的目标：某个库下的某个目录。
// 注意：该结构体会被用作 map 的键，因此只能包含可比较类型，不可加入切片或 map 字段。
type ScanTarget struct {
	LibraryID  int
	FolderPath string // Relative path within the library, or "" for entire library
	// FolderPath 为库内相对路径；为空表示扫描整个库
}

func (st ScanTarget) String() string {
	return fmt.Sprintf("%d:%s", st.LibraryID, st.FolderPath)
}

// ScannerStatus holds information about the current scan status
// ScannerStatus 是扫描进度快照，通过 SSE 推送给前端实时展示。
type ScannerStatus struct {
	Scanning    bool          // 是否正在扫描
	LastScan    time.Time     // 上次扫描完成时间
	Count       uint32        // 已处理的文件数
	FolderCount uint32        // 已处理的目录数
	LastError   string        // 上次扫描的错误信息
	ScanType    string        // 扫描类型：full 或 incremental
	ElapsedTime time.Duration // 本次已耗时
}

// Scanner 是扫描器对外接口，由 scanner.controller 实现。
type Scanner interface {
	// ScanAll starts a scan of all libraries. This is a blocking operation.
	// ScanAll 扫描全部库，阻塞直到完成。fullScan 为 true 时忽略目录哈希强制全量重扫
	ScanAll(ctx context.Context, fullScan bool) (warnings []string, err error)
	// ScanFolders scans specific library/folder pairs, recursing into subdirectories.
	// If targets is nil, it scans all libraries. This is a blocking operation.
	// ScanFolders 只扫描指定的库/目录（会递归子目录），targets 为 nil 时等价于 ScanAll。
	// 供文件监听器在检测到局部变更时做最小范围扫描
	ScanFolders(ctx context.Context, fullScan bool, targets []ScanTarget) (warnings []string, err error)
	Status(context.Context) (*ScannerStatus, error)
}

// ParseTargets parses scan targets strings into ScanTarget structs.
// Example: []string{"1:Music/Rock", "2:Classical"}
// ParseTargets 把命令行传入的 "库ID:目录路径" 字符串解析为 ScanTarget。
// 只按第一个冒号切分，因为目录路径本身可能包含冒号。
// 空字符串项会被跳过；解析结果为空时返回错误。
func ParseTargets(libFolders []string) ([]ScanTarget, error) {
	targets := make([]ScanTarget, 0, len(libFolders))

	for _, part := range libFolders {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		// Split by the first colon
		colonIdx := strings.Index(part, ":")
		if colonIdx == -1 {
			return nil, fmt.Errorf("invalid target format: %q (expected libraryID:folderPath)", part)
		}

		libIDStr := part[:colonIdx]
		folderPath := part[colonIdx+1:]

		libID, err := strconv.Atoi(libIDStr)
		if err != nil {
			return nil, fmt.Errorf("invalid library ID %q: %w", libIDStr, err)
		}
		if libID <= 0 {
			return nil, fmt.Errorf("invalid library ID %q", libIDStr)
		}

		targets = append(targets, ScanTarget{
			LibraryID:  libID,
			FolderPath: folderPath,
		})
	}

	if len(targets) == 0 {
		return nil, fmt.Errorf("no valid targets found")
	}

	return targets, nil
}
