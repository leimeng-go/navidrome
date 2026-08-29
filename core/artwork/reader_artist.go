package artwork

import (
	"context"
	"crypto/md5"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/external"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/str"
)

const (
	// maxArtistFolderTraversalDepth defines how many directory levels to search
	// when looking for artist images (artist folder + parent directories)
	maxArtistFolderTraversalDepth = 3
)

// artistReader 读取艺术家图片。
// 艺术家在文件系统中没有专属记录，其目录由旗下专辑目录的公共前缀推断得出。
type artistReader struct {
	cacheKey
	a            *artwork
	provider     external.Provider
	artist       model.Artist
	artistFolder string
	imgFiles     []string
}

// newArtistArtworkReader 构造艺术家图片读取器。
//
// 只统计该艺术家为唯一专辑艺术家的专辑：
// 合辑中的专辑目录属于合辑而非某位艺术家，用它推断目录会得出错误结果。
func newArtistArtworkReader(ctx context.Context, artwork *artwork, artID model.ArtworkID, provider external.Provider) (*artistReader, error) {
	ar, err := artwork.ds.Artist(ctx).Get(artID.ID)
	if err != nil {
		return nil, err
	}
	// Only consider albums where the artist is the sole album artist.
	als, err := artwork.ds.Album(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.And{
			squirrel.Eq{"album_artist_id": artID.ID},
			squirrel.Eq{"json_array_length(participants, '$.albumartist')": 1},
		},
	})
	if err != nil {
		return nil, err
	}
	albumPaths, imgFiles, imagesUpdatedAt, err := loadAlbumFoldersPaths(ctx, artwork.ds, als...)
	if err != nil {
		return nil, err
	}
	artistFolder, artistFolderLastUpdate, err := loadArtistFolder(ctx, artwork.ds, als, albumPaths)
	if err != nil {
		return nil, err
	}
	a := &artistReader{
		a:            artwork,
		provider:     provider,
		artist:       *ar,
		artistFolder: artistFolder,
		imgFiles:     imgFiles,
	}
	// TODO Find a way to factor in the ExternalUpdateInfoAt in the cache key. Problem is that it can
	// change _after_ retrieving from external sources, making the key invalid
	//a.cacheKey.lastUpdate = ar.ExternalInfoUpdatedAt

	a.cacheKey.lastUpdate = *imagesUpdatedAt
	if artistFolderLastUpdate.After(a.cacheKey.lastUpdate) {
		a.cacheKey.lastUpdate = artistFolderLastUpdate
	}
	a.cacheKey.artID = artID
	return a, nil
}

// Key 生成缓存键，混入代理与 Spotify 配置的摘要，使配置变更后缓存自动失效。
func (a *artistReader) Key() string {
	hash := md5.Sum([]byte(conf.Server.Agents + conf.Server.Spotify.ID))
	return fmt.Sprintf(
		"%s.%t.%x",
		a.cacheKey.Key(),
		conf.Server.EnableExternalServices,
		hash,
	)
}

func (a *artistReader) LastUpdated() time.Time {
	return a.lastUpdate
}

// Reader 按 ArtistArtPriority 配置依次尝试各来源。
func (a *artistReader) Reader(ctx context.Context) (io.ReadCloser, string, error) {
	var ff = a.fromArtistArtPriority(ctx, conf.Server.ArtistArtPriority)
	return selectImageReader(ctx, a.artID, ff...)
}

// fromArtistArtPriority 解析艺术家图片优先级配置。
// external 走外部代理；「album/」前缀表示在专辑目录下找；
// 其余在艺术家目录（含上层目录）中按通配符查找。
func (a *artistReader) fromArtistArtPriority(ctx context.Context, priority string) []sourceFunc {
	var ff []sourceFunc
	for _, pattern := range strings.Split(strings.ToLower(priority), ",") {
		pattern = strings.TrimSpace(pattern)
		switch {
		case pattern == "external":
			ff = append(ff, fromArtistExternalSource(ctx, a.artist, a.provider))
		case strings.HasPrefix(pattern, "album/"):
			ff = append(ff, fromExternalFile(ctx, a.imgFiles, strings.TrimPrefix(pattern, "album/")))
		default:
			ff = append(ff, fromArtistFolder(ctx, a.artistFolder, pattern))
		}
	}
	return ff
}

// fromArtistFolder 在艺术家目录及其上层目录中查找图片，最多上溯 3 层。
// 上溯是因为图片常被放在更上层（如「艺术家/」或流派目录）。
func fromArtistFolder(ctx context.Context, artistFolder string, pattern string) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		current := artistFolder
		for i := 0; i < maxArtistFolderTraversalDepth; i++ {
			if reader, path, err := findImageInFolder(ctx, current, pattern); err == nil {
				return reader, path, nil
			}

			parent := filepath.Dir(current)
			if parent == current {
				break
			}
			current = parent
		}
		return nil, "", fmt.Errorf(`no matches for '%s' in '%s' or its parent directories`, pattern, artistFolder)
	}
}

// findImageInFolder 在单个目录中按通配符查找图片，
// 过滤出真正的图片文件后排序，逐个尝试打开直到成功。
func findImageInFolder(ctx context.Context, folder, pattern string) (io.ReadCloser, string, error) {
	log.Trace(ctx, "looking for artist image", "pattern", pattern, "folder", folder)
	fsys := os.DirFS(folder)
	matches, err := fs.Glob(fsys, pattern)
	if err != nil {
		log.Warn(ctx, "Error matching artist image pattern", "pattern", pattern, "folder", folder, err)
		return nil, "", err
	}

	// Filter to valid image files
	var imagePaths []string
	for _, m := range matches {
		if !model.IsImageFile(m) {
			continue
		}
		imagePaths = append(imagePaths, m)
	}

	// Sort image files by prioritizing base filenames without numeric
	// suffixes (e.g., artist.jpg before artist.1.jpg)
	slices.SortFunc(imagePaths, compareImageFiles)

	// Try to open files in sorted order
	for _, p := range imagePaths {
		filePath := filepath.Join(folder, p)
		f, err := os.Open(filePath)
		if err != nil {
			log.Warn(ctx, "Could not open cover art file", "file", filePath, err)
			continue
		}
		return f, filePath, nil
	}

	return nil, "", fmt.Errorf(`no matches for '%s' in '%s'`, pattern, folder)
}

// loadArtistFolder 推断艺术家目录：取旗下所有专辑目录的最长公共前缀再上溯一级。
// 例如专辑在 /music/Beatles/Abbey Road 与 /music/Beatles/Revolver，
// 则推断艺术家目录为 /music/Beatles。
// 同时返回该目录的图片更新时间用于缓存失效判断。
func loadArtistFolder(ctx context.Context, ds model.DataStore, albums model.Albums, paths []string) (string, time.Time, error) {
	if len(albums) == 0 {
		return "", time.Time{}, nil
	}
	libID := albums[0].LibraryID // Just need one of the albums, as they should all be in the same Library - for now! TODO: Support multiple libraries

	folderPath := str.LongestCommonPrefix(paths)
	if !strings.HasSuffix(folderPath, string(filepath.Separator)) {
		folderPath, _ = filepath.Split(folderPath)
	}
	folderPath = filepath.Dir(folderPath)

	// Manipulate the path to get the folder ID
	// TODO: This is a bit hacky, but it's the easiest way to get the folder ID, ATM
	libPath := core.AbsolutePath(ctx, ds, libID, "")
	folderID := model.FolderID(model.Library{ID: libID, Path: libPath}, folderPath)

	log.Trace(ctx, "Calculating artist folder details", "folderPath", folderPath, "folderID", folderID,
		"libPath", libPath, "libID", libID, "albumPaths", paths)

	// Get the last update time for the folder
	folders, err := ds.Folder(ctx).GetAll(model.QueryOptions{Filters: squirrel.Eq{"folder.id": folderID, "missing": false}})
	if err != nil || len(folders) == 0 {
		log.Warn(ctx, "Could not find folder for artist", "folderPath", folderPath, "id", folderID,
			"libPath", libPath, "libID", libID, err)
		return "", time.Time{}, err
	}
	return folderPath, folders[0].ImagesUpdatedAt, nil
}
