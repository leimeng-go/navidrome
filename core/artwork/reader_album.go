package artwork

import (
	"cmp"
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/maruel/natural"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/external"
	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/model"
)

// albumArtworkReader 读取专辑封面。
// 一张专辑可能横跨多个目录（多碟分目录），故收集所有目录下的图片文件。
type albumArtworkReader struct {
	cacheKey
	a          *artwork
	provider   external.Provider
	album      model.Album
	updatedAt  *time.Time
	imgFiles   []string
	rootFolder string
}

// newAlbumArtworkReader 构造专辑封面读取器。
// 缓存时间取专辑更新时间与目录图片更新时间的较晚者，
// 使外部图片文件变更也能让缓存失效。
func newAlbumArtworkReader(ctx context.Context, artwork *artwork, artID model.ArtworkID, provider external.Provider) (*albumArtworkReader, error) {
	al, err := artwork.ds.Album(ctx).Get(artID.ID)
	if err != nil {
		return nil, err
	}
	_, imgFiles, imagesUpdateAt, err := loadAlbumFoldersPaths(ctx, artwork.ds, *al)
	if err != nil {
		return nil, err
	}
	a := &albumArtworkReader{
		a:          artwork,
		provider:   provider,
		album:      *al,
		updatedAt:  imagesUpdateAt,
		imgFiles:   imgFiles,
		rootFolder: core.AbsolutePath(ctx, artwork.ds, al.LibraryID, ""),
	}
	a.cacheKey.artID = artID
	if a.updatedAt != nil && a.updatedAt.After(al.UpdatedAt) {
		a.cacheKey.lastUpdate = *a.updatedAt
	} else {
		a.cacheKey.lastUpdate = al.UpdatedAt
	}
	return a, nil
}

// Key 生成缓存键。
// 键中混入代理配置与封面优先级的摘要：配置一改，缓存即失效，
// 否则改了优先级仍会返回旧来源的图片。
func (a *albumArtworkReader) Key() string {
	var hash [16]byte
	if conf.Server.EnableExternalServices {
		hash = md5.Sum([]byte(conf.Server.Agents + conf.Server.CoverArtPriority))
	}
	return fmt.Sprintf(
		"%s.%x.%t",
		a.cacheKey.Key(),
		hash,
		conf.Server.EnableExternalServices,
	)
}

func (a *albumArtworkReader) LastUpdated() time.Time {
	return a.album.UpdatedAt
}

// Reader 按配置的优先级依次尝试各封面来源。
func (a *albumArtworkReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	var ff = a.fromCoverArtPriority(ctx, a.a.ffmpeg, conf.Server.CoverArtPriority)
	return selectImageReader(ctx, a.artID, ff...)
}

// fromCoverArtPriority 把 CoverArtPriority 配置解析为有序的来源列表。
// 配置形如 "embedded,cover.*,folder.*,external"：
// embedded 取内嵌图，external 走外部代理，其余当作文件名通配符。
func (a *albumArtworkReader) fromCoverArtPriority(ctx context.Context, ffmpeg ffmpeg.FFmpeg, priority string) []sourceFunc {
	var ff []sourceFunc
	for _, pattern := range strings.Split(strings.ToLower(priority), ",") {
		pattern = strings.TrimSpace(pattern)
		switch {
		case pattern == "embedded":
			embedArtPath := filepath.Join(a.rootFolder, a.album.EmbedArtPath)
			ff = append(ff, fromTag(ctx, embedArtPath), fromFFmpegTag(ctx, ffmpeg, embedArtPath))
		case pattern == "external":
			ff = append(ff, fromAlbumExternalSource(ctx, a.album, a.provider))
		case len(a.imgFiles) > 0:
			ff = append(ff, fromExternalFile(ctx, a.imgFiles, pattern))
		}
	}
	return ff
}

// loadAlbumFoldersPaths 收集专辑所在各目录的路径与图片文件，
// 并取其中最晚的图片更新时间。缺失的目录会被排除。
func loadAlbumFoldersPaths(ctx context.Context, ds model.DataStore, albums ...model.Album) ([]string, []string, *time.Time, error) {
	var folderIDs []string
	for _, album := range albums {
		folderIDs = append(folderIDs, album.FolderIDs...)
	}
	folders, err := ds.Folder(ctx).GetAll(model.QueryOptions{Filters: squirrel.Eq{"folder.id": folderIDs, "missing": false}})
	if err != nil {
		return nil, nil, nil, err
	}
	var paths []string
	var imgFiles []string
	var updatedAt time.Time
	for _, f := range folders {
		path := f.AbsolutePath()
		paths = append(paths, path)
		if f.ImagesUpdatedAt.After(updatedAt) {
			updatedAt = f.ImagesUpdatedAt
		}
		for _, img := range f.ImageFiles {
			imgFiles = append(imgFiles, filepath.Join(path, img))
		}
	}

	// Sort image files to ensure consistent selection of cover art
	// This prioritizes files without numeric suffixes (e.g., cover.jpg over cover.1.jpg)
	// by comparing base filenames without extensions
	slices.SortFunc(imgFiles, compareImageFiles)

	return paths, imgFiles, &updatedAt, nil
}

// compareImageFiles compares two image file paths for sorting.
// It extracts the base filename (without extension) and compares case-insensitively.
// This ensures that "cover.jpg" sorts before "cover.1.jpg" since "cover" < "cover.1".
// Note: This function is called O(n log n) times during sorting, but in practice albums
// typically have only 1-20 image files, making the repeated string operations negligible.
//
// compareImageFiles 排序图片文件，确保每次都选中同一张封面。
// 先比较去扩展名的基础文件名，使 "cover.jpg" 排在 "cover.1.jpg" 之前；
// 用自然序比较，让 img2 排在 img10 之前。
func compareImageFiles(a, b string) int {
	// Case-insensitive comparison
	a = strings.ToLower(a)
	b = strings.ToLower(b)

	// Extract base filenames without extensions
	baseA := strings.TrimSuffix(filepath.Base(a), filepath.Ext(a))
	baseB := strings.TrimSuffix(filepath.Base(b), filepath.Ext(b))

	// Compare base names first, then full paths if equal
	return cmp.Or(
		natural.Compare(baseA, baseB),
		natural.Compare(a, b),
	)
}
