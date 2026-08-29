package artwork

import (
	"context"
	"errors"
	_ "image/gif"
	"io"
	"time"

	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/external"
	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/resources"
	"github.com/navidrome/navidrome/utils/cache"
	_ "golang.org/x/image/webp"
)

// ErrUnavailable 表示找不到可用的封面图。
var ErrUnavailable = errors.New("artwork unavailable")

// Artwork 提供封面图读取，涵盖专辑、艺术家、曲目与播放列表。
type Artwork interface {
	Get(ctx context.Context, artID model.ArtworkID, size int, square bool) (io.ReadCloser, time.Time, error)
	GetOrPlaceholder(ctx context.Context, id string, size int, square bool) (io.ReadCloser, time.Time, error)
}

// NewArtwork 创建封面服务。
func NewArtwork(ds model.DataStore, cache cache.FileCache, ffmpeg ffmpeg.FFmpeg, provider external.Provider) Artwork {
	return &artwork{ds: ds, cache: cache, ffmpeg: ffmpeg, provider: provider}
}

type artwork struct {
	ds       model.DataStore
	cache    cache.FileCache
	ffmpeg   ffmpeg.FFmpeg
	provider external.Provider
}

// artworkReader 是封面数据源的抽象，同时充当缓存条目。
// 各类实体（专辑/艺术家/曲目/播放列表）与缩放器都实现该接口，
// 从而共用同一套缓存机制。
type artworkReader interface {
	cache.Item
	LastUpdated() time.Time
	Reader(ctx context.Context) (io.ReadCloser, string, error)
}

// GetOrPlaceholder 取封面，取不到时返回内置占位图。
// 占位图的时间戳统一取服务启动时间，便于客户端缓存。
func (a *artwork) GetOrPlaceholder(ctx context.Context, id string, size int, square bool) (reader io.ReadCloser, lastUpdate time.Time, err error) {
	artID, err := a.getArtworkId(ctx, id)
	if err == nil {
		reader, lastUpdate, err = a.Get(ctx, artID, size, square)
	}
	if errors.Is(err, ErrUnavailable) {
		if artID.Kind == model.KindArtistArtwork {
			reader, _ = resources.FS().Open(consts.PlaceholderArtistArt)
		} else {
			reader, _ = resources.FS().Open(consts.PlaceholderAlbumArt)
		}
		return reader, consts.ServerStart, nil
	}
	return reader, lastUpdate, err
}

// Get 取指定封面，结果经文件缓存。
// 请求取消与「无封面」属预期情况，不记为错误日志。
func (a *artwork) Get(ctx context.Context, artID model.ArtworkID, size int, square bool) (reader io.ReadCloser, lastUpdate time.Time, err error) {
	artReader, err := a.getArtworkReader(ctx, artID, size, square)
	if err != nil {
		return nil, time.Time{}, err
	}

	r, err := a.cache.Get(ctx, artReader)
	if err != nil {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrUnavailable) {
			log.Error(ctx, "Error accessing image cache", "id", artID, "size", size, err)
		}
		return nil, time.Time{}, err
	}
	return r, artReader.LastUpdated(), nil
}

// coverArtGetter 由能提供封面 ID 的实体实现。
type coverArtGetter interface {
	CoverArtID() model.ArtworkID
}

// getArtworkId 把请求中的 ID 解析为 ArtworkID。
// 优先按封面 ID 格式解析；失败则当作实体 ID 反查实体，
// 兼容 Subsonic 客户端直接传实体 ID 请求封面的用法。
func (a *artwork) getArtworkId(ctx context.Context, id string) (model.ArtworkID, error) {
	if id == "" {
		return model.ArtworkID{}, ErrUnavailable
	}
	artID, err := model.ParseArtworkID(id)
	if err == nil {
		return artID, nil
	}

	log.Trace(ctx, "ArtworkID invalid. Trying to figure out kind based on the ID", "id", id)
	entity, err := model.GetEntityByID(ctx, a.ds, id)
	if err != nil {
		return model.ArtworkID{}, err
	}
	if e, ok := entity.(coverArtGetter); ok {
		artID = e.CoverArtID()
	}
	switch e := entity.(type) {
	case *model.Artist:
		log.Trace(ctx, "ID is for an Artist", "id", id, "name", e.Name, "artist", e.Name)
	case *model.Album:
		log.Trace(ctx, "ID is for an Album", "id", id, "name", e.Name, "artist", e.AlbumArtist)
	case *model.MediaFile:
		log.Trace(ctx, "ID is for a MediaFile", "id", id, "title", e.Title, "album", e.Album)
	case *model.Playlist:
		log.Trace(ctx, "ID is for a Playlist", "id", id, "name", e.Name)
	}
	return artID, nil
}

// getArtworkReader 按需求挑选封面数据源。
// 指定了尺寸或需要方图时套一层缩放器，由它再去取原图；
// 否则按实体类型分派到对应的读取器。
func (a *artwork) getArtworkReader(ctx context.Context, artID model.ArtworkID, size int, square bool) (artworkReader, error) {
	var artReader artworkReader
	var err error
	if size > 0 || square {
		artReader, err = resizedFromOriginal(ctx, a, artID, size, square)
	} else {
		switch artID.Kind {
		case model.KindArtistArtwork:
			artReader, err = newArtistArtworkReader(ctx, a, artID, a.provider)
		case model.KindAlbumArtwork:
			artReader, err = newAlbumArtworkReader(ctx, a, artID, a.provider)
		case model.KindMediaFileArtwork:
			artReader, err = newMediafileArtworkReader(ctx, a, artID)
		case model.KindPlaylistArtwork:
			artReader, err = newPlaylistArtworkReader(ctx, a, artID)
		default:
			return nil, ErrUnavailable
		}
	}
	return artReader, err
}
