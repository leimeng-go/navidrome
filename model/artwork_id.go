package model

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Kind 表示封面所属的实体类型。prefix 是序列化到字符串 ID 中的短前缀，
// name 用于日志与缓存路径。字段私有使其成为受控枚举。
type Kind struct {
	prefix string
	name   string
}

func (k Kind) String() string {
	return k.name
}

var (
	KindMediaFileArtwork = Kind{"mf", "media_file"}
	KindArtistArtwork    = Kind{"ar", "artist"}
	KindAlbumArtwork     = Kind{"al", "album"}
	KindPlaylistArtwork  = Kind{"pl", "playlist"}
)

// artworkKindMap 用于从字符串前缀反查 Kind，见 ParseArtworkID。
var artworkKindMap = map[string]Kind{
	KindMediaFileArtwork.prefix: KindMediaFileArtwork,
	KindArtistArtwork.prefix:    KindArtistArtwork,
	KindAlbumArtwork.prefix:     KindAlbumArtwork,
	KindPlaylistArtwork.prefix:  KindPlaylistArtwork,
}

// ArtworkID 唯一标识一张封面图。它可序列化为字符串在 URL 中传递，
// 格式为 "前缀-实体ID_十六进制时间戳"（例如 "al-abc123_65f0a1b2"）。
// 把 LastUpdate 编进 ID 是关键设计：实体更新后 ID 随之变化，
// 从而让浏览器与 CDN 的缓存自然失效，无需手动清理。
type ArtworkID struct {
	Kind       Kind
	ID         string
	LastUpdate time.Time
}

// String 把 ArtworkID 序列化为字符串。ID 为空时返回空串（表示无封面）；
// 无更新时间时以 "_0" 结尾，保证格式统一便于解析。
func (id ArtworkID) String() string {
	if id.ID == "" {
		return ""
	}
	s := fmt.Sprintf("%s-%s", id.Kind.prefix, id.ID)
	if lu := id.LastUpdate.Unix(); lu > 0 {
		return fmt.Sprintf("%s_%x", s, lu)
	}
	return s + "_0"
}

// NewArtworkID 构造封面标识，lastUpdate 可为 nil（表示不带时间戳）。
func NewArtworkID(kind Kind, id string, lastUpdate *time.Time) ArtworkID {
	artID := ArtworkID{kind, id, time.Time{}}
	if lastUpdate != nil {
		artID.LastUpdate = *lastUpdate
	}
	return artID
}

// ParseArtworkID 解析字符串形式的封面标识。
// 先按 "-" 切出类型前缀，再按 "_" 切出时间戳部分（十六进制 Unix 秒）。
// 注意实体 ID 本身可能含 "-"，所以用 SplitN 限制只切一次。
func ParseArtworkID(id string) (ArtworkID, error) {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return ArtworkID{}, errors.New("invalid artwork id")
	}
	kind, ok := artworkKindMap[parts[0]]
	if !ok {
		return ArtworkID{}, errors.New("invalid artwork kind")
	}
	parsedID := ArtworkID{
		Kind: kind,
		ID:   parts[1],
	}
	parts = strings.SplitN(parts[1], "_", 2)
	if len(parts) == 2 {
		if parts[1] != "0" {
			lastUpdate, err := strconv.ParseInt(parts[1], 16, 64)
			if err != nil {
				return ArtworkID{}, err
			}
			parsedID.LastUpdate = time.Unix(lastUpdate, 0)
		}
		parsedID.ID = parts[0]
	}
	return parsedID, nil
}

// MustParseArtworkID 同 ParseArtworkID，但解析失败直接 panic。
// 仅用于常量或测试等确知格式合法的场景。
func MustParseArtworkID(id string) ArtworkID {
	artID, err := ParseArtworkID(id)
	if err != nil {
		panic(artID)
	}
	return artID
}

// 以下 artworkIDFrom* 为各实体生成封面标识。
// 注意艺人不带 LastUpdate：艺人图片来自外部服务而非本地文件，
// 其更新时间与实体的 UpdatedAt 无关，纳入反而会造成无谓的缓存失效。
func artworkIDFromAlbum(al Album) ArtworkID {
	return ArtworkID{
		Kind:       KindAlbumArtwork,
		ID:         al.ID,
		LastUpdate: al.UpdatedAt,
	}
}

func artworkIDFromMediaFile(mf MediaFile) ArtworkID {
	return ArtworkID{
		Kind:       KindMediaFileArtwork,
		ID:         mf.ID,
		LastUpdate: mf.UpdatedAt,
	}
}

func artworkIDFromPlaylist(pls Playlist) ArtworkID {
	return ArtworkID{
		Kind:       KindPlaylistArtwork,
		ID:         pls.ID,
		LastUpdate: pls.UpdatedAt,
	}
}

func artworkIDFromArtist(ar Artist) ArtworkID {
	return ArtworkID{
		Kind: KindArtistArtwork,
		ID:   ar.ID,
	}
}
