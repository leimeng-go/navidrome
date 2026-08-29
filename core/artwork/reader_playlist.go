package artwork

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/draw"
	"image/png"
	"io"
	"time"

	"github.com/disintegration/imaging"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
)

// playlistArtworkReader 生成播放列表封面。
// 播放列表没有自己的图片，故用其中随机 4 张专辑封面拼成一张 2x2 马赛克图。
type playlistArtworkReader struct {
	cacheKey
	a  *artwork
	pl model.Playlist
}

// tileSize 是生成图的边长，每个小格为其一半。
const tileSize = 600

// newPlaylistArtworkReader 构造播放列表封面读取器。
func newPlaylistArtworkReader(ctx context.Context, artwork *artwork, artID model.ArtworkID) (*playlistArtworkReader, error) {
	pl, err := artwork.ds.Playlist(ctx).Get(artID.ID)
	if err != nil {
		return nil, err
	}
	a := &playlistArtworkReader{
		a:  artwork,
		pl: *pl,
	}
	a.cacheKey.artID = artID
	a.cacheKey.lastUpdate = pl.UpdatedAt
	return a, nil
}

func (a *playlistArtworkReader) LastUpdated() time.Time {
	return a.lastUpdate
}

// Reader 尝试生成拼图，失败（如列表为空）则返回占位图。
func (a *playlistArtworkReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	ff := []sourceFunc{
		a.fromGeneratedTiledCover(ctx),
		fromAlbumPlaceholder(),
	}
	return selectImageReader(ctx, a.artID, ff...)
}

// fromGeneratedTiledCover 生成马赛克拼图。
func (a *playlistArtworkReader) fromGeneratedTiledCover(ctx context.Context) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		tiles, err := a.loadTiles(ctx)
		if err != nil {
			return nil, "", err
		}
		r, err := a.createTiledImage(ctx, tiles)
		return r, "", err
	}
}

// toAlbumArtworkIDs 把专辑 ID 转换为对应的封面 ID。
func toAlbumArtworkIDs(albumIDs []string) []model.ArtworkID {
	return slice.Map(albumIDs, func(id string) model.ArtworkID {
		al := model.Album{ID: id}
		return al.CoverArtID()
	})
}

// loadTiles 随机取列表中至多 4 张专辑封面作为拼图素材。
//
// 不足 4 张时复制补齐，以保证拼图始终对称：
// 2 张按 ABBA 排布，3 张则重复第一张。
// 一张都没有时返回错误，交由占位图兜底。
func (a *playlistArtworkReader) loadTiles(ctx context.Context) ([]image.Image, error) {
	tracksRepo := a.a.ds.Playlist(ctx).Tracks(a.pl.ID, false)
	albumIds, err := tracksRepo.GetAlbumIDs(model.QueryOptions{Max: 4, Sort: "random()"})
	if err != nil {
		log.Error(ctx, "Error getting album IDs for playlist", "id", a.pl.ID, "name", a.pl.Name, err)
		return nil, err
	}
	ids := toAlbumArtworkIDs(albumIds)

	var tiles []image.Image
	for _, id := range ids {
		r, _, err := fromAlbum(ctx, a.a, id)()
		if err == nil {
			tile, err := a.createTile(ctx, r)
			if err == nil {
				tiles = append(tiles, tile)
			}
			_ = r.Close()
		}
		if len(tiles) == 4 {
			break
		}
	}
	switch len(tiles) {
	case 0:
		return nil, errors.New("could not find any eligible cover")
	case 2:
		tiles = append(tiles, tiles[1], tiles[0])
	case 3:
		tiles = append(tiles, tiles[0])
	}
	return tiles, nil
}

// createTile 把一张封面裁剪缩放为正方形小格（居中裁剪 + Lanczos 重采样）。
func (a *playlistArtworkReader) createTile(_ context.Context, r io.ReadCloser) (image.Image, error) {
	img, _, err := image.Decode(r)
	if err != nil {
		return nil, err
	}
	return imaging.Fill(img, tileSize/2, tileSize/2, imaging.Center, imaging.Lanczos), nil
}

// createTiledImage 把 4 张小格拼成 2x2 的 PNG。
// 只有一张素材时直接输出该图，不做拼接。
func (a *playlistArtworkReader) createTiledImage(_ context.Context, tiles []image.Image) (io.ReadCloser, error) {
	buf := new(bytes.Buffer)
	var rgba draw.Image
	var err error
	if len(tiles) == 4 {
		rgba = image.NewRGBA(image.Rectangle{Max: image.Point{X: tileSize - 1, Y: tileSize - 1}})
		draw.Draw(rgba, rect(0), tiles[0], image.Point{}, draw.Src)
		draw.Draw(rgba, rect(1), tiles[1], image.Point{}, draw.Src)
		draw.Draw(rgba, rect(2), tiles[2], image.Point{}, draw.Src)
		draw.Draw(rgba, rect(3), tiles[3], image.Point{}, draw.Src)
		err = png.Encode(buf, rgba)
	} else {
		err = png.Encode(buf, tiles[0])
	}
	if err != nil {
		return nil, err
	}
	return io.NopCloser(buf), nil
}

// rect 返回第 pos 个小格在 2x2 布局中的区域：0 左上、1 右上、2 左下、3 右下。
func rect(pos int) image.Rectangle {
	r := image.Rectangle{}
	switch pos {
	case 1:
		r.Min.X = tileSize / 2
	case 2:
		r.Min.Y = tileSize / 2
	case 3:
		r.Min.X = tileSize / 2
		r.Min.Y = tileSize / 2
	}
	r.Max.X = r.Min.X + tileSize/2
	r.Max.Y = r.Min.Y + tileSize/2
	return r
}
