package artwork

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
)

// mediafileArtworkReader 读取单曲封面。
// 同时持有所属专辑，以便单曲无内嵌图时回退到专辑封面。
type mediafileArtworkReader struct {
	cacheKey
	a         *artwork
	mediafile model.MediaFile
	album     model.Album
}

// newMediafileArtworkReader 构造单曲封面读取器。
// 缓存时间取单曲与专辑更新时间的较晚者，因为两者任一变化都可能影响结果。
func newMediafileArtworkReader(ctx context.Context, artwork *artwork, artID model.ArtworkID) (*mediafileArtworkReader, error) {
	mf, err := artwork.ds.MediaFile(ctx).Get(artID.ID)
	if err != nil {
		return nil, err
	}
	al, err := artwork.ds.Album(ctx).Get(mf.AlbumID)
	if err != nil {
		return nil, err
	}
	a := &mediafileArtworkReader{
		a:         artwork,
		mediafile: *mf,
		album:     *al,
	}
	a.cacheKey.artID = artID
	if al.UpdatedAt.After(mf.UpdatedAt) {
		a.cacheKey.lastUpdate = al.UpdatedAt
	} else {
		a.cacheKey.lastUpdate = mf.UpdatedAt
	}
	return a, nil
}

// Key 生成缓存键，混入「是否启用单曲封面」配置。
func (a *mediafileArtworkReader) Key() string {
	return fmt.Sprintf(
		"%s.%t",
		a.cacheKey.Key(),
		conf.Server.EnableMediaFileCoverArt,
	)
}

func (a *mediafileArtworkReader) LastUpdated() time.Time {
	return a.lastUpdate
}

// Reader 优先取单曲内嵌图，取不到再回退到专辑封面。
// 仅当该单曲确实拥有独立封面时才尝试读取内嵌图。
func (a *mediafileArtworkReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	var ff []sourceFunc
	if a.mediafile.CoverArtID().Kind == model.KindMediaFileArtwork {
		path := a.mediafile.AbsolutePath()
		ff = []sourceFunc{
			fromTag(ctx, path),
			fromFFmpegTag(ctx, a.a.ffmpeg, path),
		}
	}
	ff = append(ff, fromAlbum(ctx, a.a, a.mediafile.AlbumCoverArtID()))
	return selectImageReader(ctx, a.artID, ff...)
}
