package metrics

import (
	"os/exec"
	"strings"
	"syscall"
)

// getOSVersion 调用 sw_vers 获取 macOS 版本号。
func getOSVersion() (string, string) {
	cmd := exec.Command("sw_vers", "-productVersion")

	output, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	return strings.TrimSpace(string(output)), ""
}

// getFilesystemType 从 statfs 结果中读取文件系统名。
// macOS 直接返回名称字符串（C 风格定长数组），需在首个 0 字节处截断。
func getFilesystemType(path string) (string, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	if err != nil {
		return "", err
	}

	// Convert the filesystem type name from [16]int8 to string
	fsType := make([]byte, 0, 16)
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		fsType = append(fsType, byte(c))
	}

	return string(fsType), nil
}
