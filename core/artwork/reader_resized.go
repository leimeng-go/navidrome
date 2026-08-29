package artwork

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"time"

	"github.com/disintegration/imaging"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// resizedArtworkReader 是缩放装饰器：先取原图（可能命中缓存），再缩放。
// 缩放结果本身也会被缓存，故同一尺寸只需计算一次。
type resizedArtworkReader struct {
	artID      model.ArtworkID
	cacheKey   string
	lastUpdate time.Time
	size       int
	square     bool
	a          *artwork
}

// resizedFromOriginal 构造缩放读取器。
// 缓存键与更新时间沿用原图的，从而原图变化时缩放结果也随之失效。
func resizedFromOriginal(ctx context.Context, a *artwork, artID model.ArtworkID, size int, square bool) (*resizedArtworkReader, error) {
	r := &resizedArtworkReader{a: a}
	r.artID = artID
	r.size = size
	r.square = square

	// Get lastUpdated and cacheKey from original artwork
	original, err := a.getArtworkReader(ctx, artID, 0, false)
	if err != nil {
		return nil, err
	}
	r.cacheKey = original.Key()
	r.lastUpdate = original.LastUpdated()
	return r, nil
}

// Key 生成缓存键。方图输出 PNG 与质量无关，
// 非方图则须带上 JPEG 质量参数，配置调整后缓存才会失效。
func (a *resizedArtworkReader) Key() string {
	baseKey := fmt.Sprintf("%s.%d", a.cacheKey, a.size)
	if a.square {
		return baseKey + ".square"
	}
	return fmt.Sprintf("%s.%d", baseKey, conf.Server.CoverJpegQuality)
}

func (a *resizedArtworkReader) LastUpdated() time.Time {
	return a.lastUpdate
}

// Reader 取原图并缩放。
// 缩放失败或原图本就小于目标尺寸时，直接返回原图——
// 放大只会损失画质而无实际收益。
func (a *resizedArtworkReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	// Get artwork in original size, possibly from cache
	orig, _, err := a.a.Get(ctx, a.artID, 0, false)
	if err != nil {
		return nil, "", err
	}
	defer orig.Close()

	resized, origSize, err := resizeImage(orig, a.size, a.square)
	if resized == nil {
		log.Trace(ctx, "Image smaller than requested size", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	} else {
		log.Trace(ctx, "Resizing artwork", "artID", a.artID, "original", origSize, "resized", a.size, "square", a.square)
	}
	if err != nil {
		log.Warn(ctx, "Could not resize image. Will return image as is", "artID", a.artID, "size", a.size, "square", a.square, err)
	}
	if err != nil || resized == nil {
		// if we couldn't resize the image, return the original
		orig, _, err = a.a.Get(ctx, a.artID, 0, false)
		return orig, "", err
	}
	return io.NopCloser(resized), fmt.Sprintf("%s@%d", a.artID, a.size), nil
}

// resizeImage 缩放图片，返回缩放结果与原图边长。
//
// 原图已小于目标尺寸且不要求方图时返回 nil，表示无需缩放。
// 需要方图时把图片居中叠放到透明画布上，保证输出严格为正方形。
// 输出格式：原图为 PNG 或需要方图时用 PNG（保留透明通道），否则用 JPEG（体积更小）。
func resizeImage(reader io.Reader, size int, square bool) (io.Reader, int, error) {
	original, format, err := image.Decode(reader)
	if err != nil {
		return nil, 0, err
	}

	bounds := original.Bounds()
	originalSize := max(bounds.Max.X, bounds.Max.Y)

	if originalSize <= size && !square {
		return nil, originalSize, nil
	}

	var resized image.Image
	if originalSize >= size {
		resized = imaging.Fit(original, size, size, imaging.Lanczos)
	} else {
		if bounds.Max.Y < bounds.Max.X {
			resized = imaging.Resize(original, size, 0, imaging.Lanczos)
		} else {
			resized = imaging.Resize(original, 0, size, imaging.Lanczos)
		}
	}
	if square {
		bg := image.NewRGBA(image.Rect(0, 0, size, size))
		resized = imaging.OverlayCenter(bg, resized, 1)
	}

	buf := new(bytes.Buffer)
	if format == "png" || square {
		err = png.Encode(buf, resized)
	} else {
		err = jpeg.Encode(buf, resized, &jpeg.Options{Quality: conf.Server.CoverJpegQuality})
	}
	return buf, originalSize, err
}
