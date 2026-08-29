//go:build !windows

package mpv

import (
	"os"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/utils"
)

// socketName 生成一个临时文件路径作为 Unix domain socket 名。
func socketName(prefix, suffix string) string {
	return utils.TempFileName(prefix, suffix)
}

// removeSocket 删除 socket 文件。类 Unix 系统不会自动清理，需手动删除。
func removeSocket(socketName string) {
	log.Debug("Removing socketfile", "socketfile", socketName)
	err := os.Remove(socketName)
	if err != nil {
		log.Error("Error cleaning up socketfile", "socketfile", socketName, err)
	}
}
