package core

import (
	"path/filepath"

	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/metadata"
	. "github.com/navidrome/navidrome/utils/gg"
)

// InspectOutput 是单个文件的标签检查结果：
// RawTags 为文件中的原始标签，MappedTags 为按映射规则解析后的媒体文件对象。
type InspectOutput struct {
	File       string           `json:"file"`
	RawTags    model.RawTags    `json:"rawTags"`
	MappedTags *model.MediaFile `json:"mappedTags,omitempty"`
}

// Inspect 读取单个音频文件的标签，同时给出原始值与映射结果。
// 主要供 CLI 调试标签映射规则使用，可直观看出标签是如何被解析的。
func Inspect(filePath string, libraryId int, folderId string) (*InspectOutput, error) {
	path, file := filepath.Split(filePath)

	s, err := storage.For(path)
	if err != nil {
		return nil, err
	}

	fs, err := s.FS()
	if err != nil {
		return nil, err
	}

	tags, err := fs.ReadTags(file)
	if err != nil {
		return nil, err
	}

	tag, ok := tags[file]
	if !ok {
		log.Error("Could not get tags for path", "path", filePath)
		return nil, model.ErrNotFound
	}

	md := metadata.New(path, tag)
	result := &InspectOutput{
		File:       filePath,
		RawTags:    tags[file].Tags,
		MappedTags: P(md.ToMediaFile(libraryId, folderId)),
	}

	return result, nil
}
