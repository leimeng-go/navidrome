package metadata

import (
	"cmp"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/utils"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/navidrome/navidrome/utils/str"
)

// hashFunc 是把多个字符串折叠为一个哈希值的函数。
type hashFunc = func(...string) string

// createGetPID returns a function that calculates the persistent ID for a given spec, getting the referenced values from the metadata
// The spec is a pipe-separated list of fields, where each field is a comma-separated list of attributes
// Attributes can be either tags or some processed values like folder, albumid, albumartistid, etc.
// For each field, it gets all its attributes values and concatenates them, then hashes the result.
// If a field is empty, it is skipped and the function looks for the next field.
//
// getPIDFunc 依据 spec 计算持久化 ID。
//
// PID（Persistent ID）的作用：文件被移动或标签被修改后仍能识别为同一实体，
// 从而保留播放次数、收藏、评分等用户数据。因此它不能基于文件路径，
// 而要基于内容特征，且计算规则可由用户配置。
//
// spec 语法：竖线分隔的「候选字段组」，每组内用逗号分隔多个属性。
// 从左到右取第一个有值的组，把组内属性值拼接后哈希。
// 这种「优先级回退」设计让标签不全的文件也能算出稳定 ID，
// 例如优先用 MusicBrainz ID，缺失则退回到「专辑+标题」。
type getPIDFunc = func(mf model.MediaFile, md Metadata, spec string, prependLibId bool) string

// createGetPID 构造 PID 计算函数。
//
// 用闭包而非普通函数是因为 getPID 需要递归引用自身：
// spec 中的 albumid 属性会触发按专辑 spec 再算一次 PID。
func createGetPID(hash hashFunc) getPIDFunc {
	var getPID getPIDFunc
	// getAttr 解析单个属性。除下列特殊属性外，其余都按标签名取值
	getAttr := func(mf model.MediaFile, md Metadata, attr string, prependLibId bool) string {
		attr = strings.TrimSpace(strings.ToLower(attr))
		switch attr {
		case "albumid":
			// 递归：按专辑的 spec 计算专辑 PID
			return getPID(mf, md, conf.Server.PID.Album, prependLibId)
		case "folder":
			return filepath.Dir(mf.Path)
		case "albumartistid":
			// str.Clear 去除标点与变音符号，使 "Beyoncé" 与 "Beyonce" 归一
			return hash(str.Clear(strings.ToLower(mf.AlbumArtist)))
		case "title":
			return mf.Title
		case "album":
			return str.Clear(strings.ToLower(md.String(model.TagAlbum)))
		}
		return md.String(model.TagName(attr))
	}
	getPID = func(mf model.MediaFile, md Metadata, spec string, prependLibId bool) string {
		pid := ""
		fields := strings.Split(spec, "|")
		for _, field := range fields {
			attributes := strings.Split(field, ",")
			hasValue := false
			values := slice.Map(attributes, func(attr string) string {
				v := getAttr(mf, md, attr, prependLibId)
				if v != "" {
					hasValue = true
				}
				return v
			})
			// 组内只要有任一属性有值就采用该组（其余属性留空参与拼接），
			// 全空才继续尝试下一组
			if hasValue {
				pid += strings.Join(values, "\\")
				break
			}
		}
		// 加上音乐库 ID，使不同库中同名专辑不会被误判为同一实体
		if prependLibId {
			pid = fmt.Sprintf("%d\\%s", mf.LibraryID, pid)
		}
		return hash(pid)
	}

	// 两个 legacy spec 走独立实现，用于兼容旧版本生成的 ID，
	// 使老用户升级后不丢失已积累的播放数据
	return func(mf model.MediaFile, md Metadata, spec string, prependLibId bool) string {
		switch spec {
		case "track_legacy":
			return legacyTrackID(mf, prependLibId)
		case "album_legacy":
			return legacyAlbumID(mf, md, prependLibId)
		}
		return getPID(mf, md, spec, prependLibId)
	}
}

// trackPID 计算曲目的持久化 ID。
func (md Metadata) trackPID(mf model.MediaFile) string {
	return createGetPID(id.NewHash)(mf, md, conf.Server.PID.Track, true)
}

// albumID 计算专辑 ID，spec 由调用方传入以支持迁移时按旧规则重算。
func (md Metadata) albumID(mf model.MediaFile, pidConf string) string {
	return createGetPID(id.NewHash)(mf, md, pidConf, true)
}

// BFR Must be configurable?
// artistID 计算艺人 ID：仅基于规范化后的名字，故不可配置。
// 不加音乐库 ID，使同一位艺人可跨音乐库共享，避免重复条目。
func (md Metadata) artistID(name string) string {
	mf := model.MediaFile{AlbumArtist: name}
	return createGetPID(id.NewHash)(mf, md, "albumartistid", false)
}

// mapTrackTitle 取曲目标题，标签缺失时退回到文件名（不含扩展名）。
func (md Metadata) mapTrackTitle() string {
	if title := md.String(model.TagTitle); title != "" {
		return title
	}
	return utils.BaseName(md.FilePath())
}

// mapAlbumName 取专辑名，缺失时为「未知专辑」，
// 保证每首曲目都能归入某张专辑。
func (md Metadata) mapAlbumName() string {
	return cmp.Or(
		md.String(model.TagAlbum),
		consts.UnknownAlbum,
	)
}
