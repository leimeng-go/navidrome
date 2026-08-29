package local

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/djherbis/times"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model/metadata"
)

// localStorage implements a Storage that reads the files from the local filesystem and uses registered extractors
// to extract the metadata and tags from the files.
//
// localStorage 是本地文件系统的存储实现。
// resolvedPath 保存软链接解析后的真实路径：
// 监听器上报的是真实路径，需要据此换算回配置路径。
type localStorage struct {
	u            url.URL
	extractor    Extractor
	resolvedPath string
	watching     atomic.Bool
}

// newLocalStorage 构造本地存储。
// 提取器未注册时直接 Fatal——配置错误应尽早暴露而非运行时才失败。
// Windows 路径（如 C:\music）会被 url.Parse 把盘符解析成 Host，需要拼回 Path。
func newLocalStorage(u url.URL) storage.Storage {
	newExtractor, ok := extractors[conf.Server.Scanner.Extractor]
	if !ok || newExtractor == nil {
		log.Fatal("Extractor not found", "path", conf.Server.Scanner.Extractor)
	}
	isWindowsPath := filepath.VolumeName(u.Host) != ""
	if u.Scheme == storage.LocalSchemaID && isWindowsPath {
		u.Path = filepath.Join(u.Host, u.Path)
	}
	resolvedPath, err := filepath.EvalSymlinks(u.Path)
	if err != nil {
		log.Warn("Error resolving path", "path", u.Path, "err", err)
		resolvedPath = u.Path
	}
	return &localStorage{u: u, extractor: newExtractor(os.DirFS(u.Path), u.Path), resolvedPath: resolvedPath}
}

// FS 返回以库根目录为根的文件系统，路径不存在时报错。
func (s *localStorage) FS() (storage.MusicFS, error) {
	path := s.u.Path
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %s", err, path)
	}
	return &localFS{FS: os.DirFS(path), extractor: s.extractor}, nil
}

// localFS 在标准文件系统之上增加标签读取能力。
type localFS struct {
	fs.FS
	extractor Extractor
}

// ReadTags 批量读取标签。
// 提取器未提供文件信息时补一次 Stat——
// 扫描器依赖修改时间判断文件是否变化，该字段不能缺失。
func (lfs *localFS) ReadTags(path ...string) (map[string]metadata.Info, error) {
	res, err := lfs.extractor.Parse(path...)
	if err != nil {
		return nil, err
	}
	for path, v := range res {
		if v.FileInfo == nil {
			info, err := fs.Stat(lfs, path)
			if err != nil {
				return nil, err
			}
			v.FileInfo = localFileInfo{info}
			res[path] = v
		}
	}
	return res, nil
}

// localFileInfo is a wrapper around fs.FileInfo that adds a BirthTime method, to make it compatible
// with metadata.FileInfo
//
// localFileInfo 为 fs.FileInfo 补上创建时间。
type localFileInfo struct {
	fs.FileInfo
}

// BirthTime 返回文件创建时间。
// 部分文件系统不记录创建时间，此时退回当前时间
// （曲目的「添加时间」会因此不准，但不影响功能）。
func (lfi localFileInfo) BirthTime() time.Time {
	if ts := times.Get(lfi.FileInfo); ts.HasBirthTime() {
		return ts.BirthTime()
	}
	return time.Now()
}

func init() {
	storage.Register(storage.LocalSchemaID, newLocalStorage)
}
