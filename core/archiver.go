package core

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
)

// Archiver 把专辑、艺术家、分享或播放列表打包为 ZIP 供下载，可选转码。
type Archiver interface {
	ZipAlbum(ctx context.Context, id string, format string, bitrate int, w io.Writer) error
	ZipArtist(ctx context.Context, id string, format string, bitrate int, w io.Writer) error
	ZipShare(ctx context.Context, id string, w io.Writer) error
	ZipPlaylist(ctx context.Context, id string, format string, bitrate int, w io.Writer) error
}

// NewArchiver 创建打包服务。
func NewArchiver(ms MediaStreamer, ds model.DataStore, shares Share) Archiver {
	return &archiver{ds: ds, ms: ms, shares: shares}
}

type archiver struct {
	ds     model.DataStore
	ms     MediaStreamer
	shares Share
}

// ZipAlbum 打包单张专辑。
func (a *archiver) ZipAlbum(ctx context.Context, id string, format string, bitrate int, out io.Writer) error {
	return a.zipAlbums(ctx, id, format, bitrate, out, squirrel.Eq{"album_id": id})
}

// ZipArtist 打包某位艺术家的全部专辑。
func (a *archiver) ZipArtist(ctx context.Context, id string, format string, bitrate int, out io.Writer) error {
	return a.zipAlbums(ctx, id, format, bitrate, out, squirrel.Eq{"album_artist_id": id})
}

// zipAlbums 按专辑分目录打包，多碟专辑再按碟号建子目录。
// 单个文件失败只跳过，保证其余曲目仍能下载。
func (a *archiver) zipAlbums(ctx context.Context, id string, format string, bitrate int, out io.Writer, filters squirrel.Sqlizer) error {
	mfs, err := a.ds.MediaFile(ctx).GetAll(model.QueryOptions{Filters: filters, Sort: "album"})
	if err != nil {
		log.Error(ctx, "Error loading mediafiles from artist", "id", id, err)
		return err
	}

	z := createZipWriter(out, format, bitrate)
	albums := slice.Group(mfs, func(mf model.MediaFile) string {
		return mf.AlbumID
	})
	for _, album := range albums {
		discs := slice.Group(album, func(mf model.MediaFile) int { return mf.DiscNumber })
		isMultiDisc := len(discs) > 1
		log.Debug(ctx, "Zipping album", "name", album[0].Album, "artist", album[0].AlbumArtist,
			"format", format, "bitrate", bitrate, "isMultiDisc", isMultiDisc, "numTracks", len(album))
		for _, mf := range album {
			file := a.albumFilename(mf, format, isMultiDisc)
			_ = a.addFileToZip(ctx, z, mf, format, bitrate, file)
		}
	}
	err = z.Close()
	if err != nil {
		log.Error(ctx, "Error closing zip file", "id", id, err)
	}
	return err
}

// createZipWriter 创建 ZIP 写入器，并在注释中标明来源与转码参数。
func createZipWriter(out io.Writer, format string, bitrate int) *zip.Writer {
	z := zip.NewWriter(out)
	comment := "Downloaded from Navidrome"
	if format != "raw" && format != "" {
		comment = fmt.Sprintf("%s, transcoded to %s %dbps", comment, format, bitrate)
	}
	_ = z.SetComment(comment)
	return z
}

// albumFilename 生成包内路径：「专辑名/[Disc NN/]文件名」。
// 转码时需把扩展名换成目标格式。
func (a *archiver) albumFilename(mf model.MediaFile, format string, isMultiDisc bool) string {
	_, file := filepath.Split(mf.Path)
	if format != "raw" {
		file = strings.TrimSuffix(file, mf.Suffix) + format
	}
	if isMultiDisc {
		file = fmt.Sprintf("Disc %02d/%s", mf.DiscNumber, file)
	}
	return fmt.Sprintf("%s/%s", sanitizeName(mf.Album), file)
}

// ZipShare 打包分享内容。格式与比特率取分享自身的设置，
// 且必须显式开启可下载才允许打包。
func (a *archiver) ZipShare(ctx context.Context, id string, out io.Writer) error {
	s, err := a.shares.Load(ctx, id)
	if err != nil {
		return err
	}
	if !s.Downloadable {
		return model.ErrNotAuthorized
	}
	log.Debug(ctx, "Zipping share", "name", s.ID, "format", s.Format, "bitrate", s.MaxBitRate, "numTracks", len(s.Tracks))
	return a.zipMediaFiles(ctx, id, s.ID, s.Format, s.MaxBitRate, out, s.Tracks, false)
}

// ZipPlaylist 打包播放列表，并附带一个 .m3u 索引文件以保留曲目顺序。
func (a *archiver) ZipPlaylist(ctx context.Context, id string, format string, bitrate int, out io.Writer) error {
	pls, err := a.ds.Playlist(ctx).GetWithTracks(id, true, false)
	if err != nil {
		log.Error(ctx, "Error loading mediafiles from playlist", "id", id, err)
		return err
	}
	mfs := pls.MediaFiles()
	log.Debug(ctx, "Zipping playlist", "name", pls.Name, "format", format, "bitrate", bitrate, "numTracks", len(mfs))
	return a.zipMediaFiles(ctx, id, pls.Name, format, bitrate, out, mfs, true)
}

// zipMediaFiles 打包一组曲目（平铺，不分目录）。
//
// 生成 M3U 前需把每个曲目的 Path 改写为包内文件名，
// 否则 M3U 里会指向服务器上的原始路径，解压后无法播放。
func (a *archiver) zipMediaFiles(ctx context.Context, id, name string, format string, bitrate int, out io.Writer, mfs model.MediaFiles, addM3U bool) error {
	z := createZipWriter(out, format, bitrate)

	zippedMfs := make(model.MediaFiles, len(mfs))
	for idx, mf := range mfs {
		file := a.playlistFilename(mf, format, idx)
		_ = a.addFileToZip(ctx, z, mf, format, bitrate, file)
		mf.Path = file
		zippedMfs[idx] = mf
	}

	// Add M3U file if requested
	if addM3U && len(zippedMfs) > 0 {
		plsName := sanitizeName(name)
		w, err := z.CreateHeader(&zip.FileHeader{
			Name:     plsName + ".m3u",
			Modified: mfs[0].UpdatedAt,
			Method:   zip.Store,
		})
		if err != nil {
			log.Error(ctx, "Error creating playlist zip entry", err)
			return err
		}

		_, err = w.Write([]byte(zippedMfs.ToM3U8(plsName, false)))
		if err != nil {
			log.Error(ctx, "Error writing m3u in zip", err)
			return err
		}
	}

	err := z.Close()
	if err != nil {
		log.Error(ctx, "Error closing zip file", "id", id, err)
	}
	return err
}

// playlistFilename 生成「序号 - 艺术家 - 标题.扩展名」形式的文件名，
// 前缀序号使解压后按文件名排序即为播放顺序。
func (a *archiver) playlistFilename(mf model.MediaFile, format string, idx int) string {
	ext := mf.Suffix
	if format != "" && format != "raw" {
		ext = format
	}
	return fmt.Sprintf("%02d - %s - %s.%s", idx+1, sanitizeName(mf.Artist), sanitizeName(mf.Title), ext)
}

// sanitizeName 把名称中的斜杠替换掉，防止在 ZIP 内产生意外的目录层级。
func sanitizeName(target string) string {
	return strings.ReplaceAll(target, "/", "_")
}

// addFileToZip 向 ZIP 写入一个曲目。
//
// 使用 Store（不压缩）：音频本身已是压缩格式，再压缩几乎无收益却消耗大量 CPU。
// 按需选择原始文件或转码流作为数据源。
func (a *archiver) addFileToZip(ctx context.Context, z *zip.Writer, mf model.MediaFile, format string, bitrate int, filename string) error {
	path := mf.AbsolutePath()
	w, err := z.CreateHeader(&zip.FileHeader{
		Name:     filename,
		Modified: mf.UpdatedAt,
		Method:   zip.Store,
	})
	if err != nil {
		log.Error(ctx, "Error creating zip entry", "file", path, err)
		return err
	}

	var r io.ReadCloser
	if format != "raw" && format != "" {
		r, err = a.ms.DoStream(ctx, &mf, format, bitrate, 0)
	} else {
		r, err = os.Open(path)
	}
	if err != nil {
		log.Error(ctx, "Error opening file for zipping", "file", path, "format", format, err)
		return err
	}

	defer func() {
		if err := r.Close(); err != nil && log.IsGreaterOrEqualTo(log.LevelDebug) {
			log.Error(ctx, "Error closing stream", "id", mf.ID, "file", path, err)
		}
	}()

	_, err = io.Copy(w, r)
	if err != nil {
		log.Error(ctx, "Error zipping file", "file", path, err)
		return err
	}

	return nil
}
