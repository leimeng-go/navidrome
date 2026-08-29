package model

import (
	"mime"
	"path/filepath"
	"slices"
	"strings"
)

// excludeAudioType 列出需要排除的 MIME 类型：
// 这些类型虽以 audio/ 开头，实际是播放列表文件（m3u、pls），不应当作音轨处理。
var excludeAudioType = []string{
	"audio/mpegurl",
	"audio/x-mpegurl",
	"audio/x-scpls",
}

// IsAudioFile 依据扩展名推导的 MIME 类型判断是否为音频文件，
// 并剔除伪装成音频类型的播放列表文件。
func IsAudioFile(filePath string) bool {
	extension := filepath.Ext(filePath)
	mimeType := mime.TypeByExtension(extension)
	return !slices.Contains(excludeAudioType, mimeType) && strings.HasPrefix(mimeType, "audio/")
}

// IsImageFile 判断是否为图片文件，用于识别目录中的封面候选。
func IsImageFile(filePath string) bool {
	extension := filepath.Ext(filePath)
	return strings.HasPrefix(mime.TypeByExtension(extension), "image/")
}

// IsValidPlaylist 判断是否为受支持的播放列表文件：
// m3u/m3u8 为普通列表，nsp 为 Navidrome 智能播放列表（JSON 规则）。
func IsValidPlaylist(filePath string) bool {
	extension := strings.ToLower(filepath.Ext(filePath))
	return extension == ".m3u" || extension == ".m3u8" || extension == ".nsp"
}
