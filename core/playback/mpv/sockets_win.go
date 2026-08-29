//go:build windows

package mpv

import (
	"path/filepath"

	"github.com/navidrome/navidrome/model/id"
)

// socketName 生成命名管道路径。
func socketName(prefix, suffix string) string {
	// Windows needs to use a named pipe for the socket
	// see https://mpv.io/manual/master#using-mpv-from-other-programs-or-scripts
	// Windows 不支持 Unix socket，mpv 在此平台使用命名管道。
	return filepath.Join(`\\.\pipe\mpvsocket`, prefix+id.NewRandom()+suffix)
}

// removeSocket 在 Windows 上无需处理。
func removeSocket(string) {
	// Windows automatically handles cleaning up named pipe
	// 命名管道由系统在进程退出时自动回收。
}
