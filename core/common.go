package core

import (
	"context"
	"path/filepath"

	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
)

// userName 取上下文中的用户名，无用户时返回 "UNKNOWN"，主要用于日志。
func userName(ctx context.Context) string {
	if user, ok := request.UserFrom(ctx); !ok {
		return "UNKNOWN"
	} else {
		return user.UserName
	}
}

// BFR We should only access files through the `storage.Storage` interface. This will require changing how
// TagLib and ffmpeg access files
//
// AbsolutePath 把库内相对路径拼成绝对路径。
// 声明为变量而非函数是为了便于测试中替换。
// 取不到库路径时原样返回，避免拼出错误路径。
var AbsolutePath = func(ctx context.Context, ds model.DataStore, libId int, path string) string {
	libPath, err := ds.Library(ctx).GetPath(libId)
	if err != nil {
		return path
	}
	return filepath.Join(libPath, path)
}
