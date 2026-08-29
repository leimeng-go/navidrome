package metadata

import (
	"cmp"
	"crypto/md5"
	"fmt"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/model"
)

// These are the legacy ID functions that were used in the original Navidrome ID generation.
// They are kept here for backwards compatibility with existing databases.
//
// 这些是早期 Navidrome 生成 ID 的算法，保留以兼容既有数据库。
// 用户可通过把 PID 配置设为 track_legacy / album_legacy 继续使用旧规则，
// 从而在升级后不丢失播放次数、收藏等关联到 ID 的用户数据。
// 注意它们固定使用 MD5，不能改用新的哈希函数，否则 ID 会变化。

// legacyTrackID 基于文件路径生成曲目 ID。
// 缺点是文件移动后 ID 就变了——这正是新版改用内容特征的原因。
// 默认音乐库不加库 ID 前缀，以保持与单库时代生成的 ID 一致。
func legacyTrackID(mf model.MediaFile, prependLibId bool) string {
	id := mf.Path
	if prependLibId && mf.LibraryID != model.DefaultLibraryID {
		id = fmt.Sprintf("%d\\%s", mf.LibraryID, id)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(id)))
}

// legacyAlbumID 基于「专辑艺人 + 专辑名」生成专辑 ID。
// 未开启合并多版本发行时，额外拼入发行日期，
// 使同一专辑的不同版本（如重制版）被视为不同专辑。
func legacyAlbumID(mf model.MediaFile, md Metadata, prependLibId bool) string {
	_, _, releaseDate := md.mapDates()
	albumPath := strings.ToLower(fmt.Sprintf("%s\\%s", legacyMapAlbumArtistName(md), legacyMapAlbumName(md)))
	if !conf.Server.Scanner.GroupAlbumReleases {
		if len(releaseDate) != 0 {
			albumPath = fmt.Sprintf("%s\\%s", albumPath, releaseDate)
		}
	}
	if prependLibId && mf.LibraryID != model.DefaultLibraryID {
		albumPath = fmt.Sprintf("%d\\%s", mf.LibraryID, albumPath)
	}
	return fmt.Sprintf("%x", md5.Sum([]byte(albumPath)))
}

// legacyMapAlbumArtistName 按旧规则决定专辑艺人名，
// 优先级为：专辑艺人 → 群星（仅合辑）→ 曲目艺人 → 未知艺人。
// 用切片配合 cmp.Or 表达优先级，中间留空槽位以便按条件填入「群星」。
func legacyMapAlbumArtistName(md Metadata) string {
	values := []string{
		md.String(model.TagAlbumArtist),
		"", // 合辑时填入「群星」，否则该槽位为空被跳过
		md.String(model.TagTrackArtist),
		consts.UnknownArtist,
	}
	if md.Bool(model.TagCompilation) {
		values[1] = consts.VariousArtists
	}
	return cmp.Or(values...)
}

// legacyMapAlbumName 按旧规则取专辑名，缺失时为「未知专辑」。
func legacyMapAlbumName(md Metadata) string {
	return cmp.Or(
		md.String(model.TagAlbum),
		consts.UnknownAlbum,
	)
}
