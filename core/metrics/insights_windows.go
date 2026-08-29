package metrics

import (
	"os/exec"
	"regexp"

	"golang.org/x/sys/windows"
)

// Ex: Microsoft Windows [Version 10.0.26100.1742]
var winVerRegex = regexp.MustCompile(`Microsoft Windows \[.+\s([\d\.]+)\]`)

// getOSVersion 执行 `cmd /c ver` 并提取版本号，无法匹配时返回原始输出。
func getOSVersion() (version string, _ string) {
	cmd := exec.Command("cmd", "/c", "ver")

	output, err := cmd.Output()
	if err != nil {
		return "", ""
	}

	matches := winVerRegex.FindStringSubmatch(string(output))
	if len(matches) != 2 {
		return string(output), ""
	}
	return matches[1], ""
}

// getFilesystemType 通过 GetVolumeInformation 获取卷的文件系统名（如 NTFS）。
func getFilesystemType(path string) (string, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}

	var volumeName, filesystemName [windows.MAX_PATH + 1]uint16
	var serialNumber uint32
	var maxComponentLen, filesystemFlags uint32

	err = windows.GetVolumeInformation(
		pathPtr,
		&volumeName[0],
		windows.MAX_PATH,
		&serialNumber,
		&maxComponentLen,
		&filesystemFlags,
		&filesystemName[0],
		windows.MAX_PATH)

	if err != nil {
		return "", err
	}

	return windows.UTF16ToString(filesystemName[:]), nil
}
