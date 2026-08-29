package local

import (
	"io/fs"
	"sync"

	"github.com/navidrome/navidrome/model/metadata"
)

// Extractor is an interface that defines the methods that a tag/metadata extractor must implement
//
// Extractor 是标签提取器接口。Version 参与扫描判断：
// 提取器版本变化时需要重新扫描全部文件。
type Extractor interface {
	Parse(files ...string) (map[string]metadata.Info, error)
	Version() string
}

// extractorConstructor 是提取器构造函数，接收文件系统与其根路径。
type extractorConstructor func(fs.FS, string) Extractor

var (
	extractors = map[string]extractorConstructor{}
	lock       sync.RWMutex
)

// RegisterExtractor registers a new extractor, so it can be used by the local storage. The one to be used is
// defined with the configuration option Scanner.Extractor.
func RegisterExtractor(id string, f extractorConstructor) {
	lock.Lock()
	defer lock.Unlock()
	extractors[id] = f
}
