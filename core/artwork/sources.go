package artwork

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/dhowden/tag"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/external"
	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/resources"
)

// selectImageReader 按给定顺序依次尝试各封面来源，返回首个成功的。
// 顺序即优先级，由各 reader 决定。每步都检查上下文取消，
// 避免请求已断开还继续访问外部服务。
func selectImageReader(ctx context.Context, artID model.ArtworkID, extractFuncs ...sourceFunc) (io.ReadCloser, string, error) {
	for _, f := range extractFuncs {
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		start := time.Now()
		r, path, err := f()
		if r != nil {
			msg := fmt.Sprintf("Found %s artwork", artID.Kind)
			log.Debug(ctx, msg, "artID", artID, "path", path, "source", f, "elapsed", time.Since(start))
			return r, path, nil
		}
		log.Trace(ctx, "Failed trying to extract artwork", "artID", artID, "source", f, "elapsed", time.Since(start), err)
	}
	return nil, "", fmt.Errorf("could not get `%s` cover art for %s: %w", artID.Kind, artID, ErrUnavailable)
}

// sourceFunc 是一个封面来源，返回图片流与其来源路径。
type sourceFunc func() (r io.ReadCloser, path string, err error)

// String 通过反射取出函数名，使日志能显示实际命中的来源，便于排查。
func (f sourceFunc) String() string {
	name := runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
	name = strings.TrimPrefix(name, "github.com/navidrome/navidrome/core/artwork.")
	if _, after, found := strings.Cut(name, ")."); found {
		name = after
	}
	name = strings.TrimSuffix(name, ".func1")
	return name
}

// fromExternalFile 从目录中挑选匹配通配符的图片文件（如 cover.jpg、folder.png）。
// 匹配时统一转小写，兼容各种大小写写法。
func fromExternalFile(ctx context.Context, files []string, pattern string) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		for _, file := range files {
			_, name := filepath.Split(file)
			match, err := filepath.Match(pattern, strings.ToLower(name))
			if err != nil {
				log.Warn(ctx, "Error matching cover art file to pattern", "pattern", pattern, "file", file)
				continue
			}
			if !match {
				continue
			}
			f, err := os.Open(file)
			if err != nil {
				log.Warn(ctx, "Could not open cover art file", "file", file, err)
				continue
			}
			return f, file, err
		}
		return nil, "", fmt.Errorf("pattern '%s' not matched by files %v", pattern, files)
	}
}

// These regexes are used to match the picture type in the file, in the order they are listed.
// 用于识别内嵌图片类型的正则，按优先级排列：
// 先找「封面正面」，再找「正面」，最后找任意「封面」。
var picTypeRegexes = []*regexp.Regexp{
	regexp.MustCompile(`(?i).*cover.*front.*|.*front.*cover.*`),
	regexp.MustCompile(`(?i).*front.*`),
	regexp.MustCompile(`(?i).*cover.*`),
}

// fromTag 读取音频文件内嵌的图片。
// 按上述优先级挑选正面封面，都不匹配时退回第一张图片
// （专辑内嵌图可能包含封底、内页等多张）。
func fromTag(ctx context.Context, path string) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		if path == "" {
			return nil, "", nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil, "", err
		}
		defer f.Close()

		m, err := tag.ReadFrom(f)
		if err != nil {
			return nil, "", err
		}

		types := m.PictureTypes()
		if len(types) == 0 {
			return nil, "", fmt.Errorf("no embedded image found in %s", path)
		}

		var picture *tag.Picture
		for _, regex := range picTypeRegexes {
			for _, t := range types {
				if regex.MatchString(t) {
					log.Trace(ctx, "Found embedded image", "type", t, "path", path)
					picture = m.Pictures(t)
					break
				}
			}
			if picture != nil {
				break
			}
		}
		if picture == nil {
			log.Trace(ctx, "Could not find a front image. Getting the first one", "type", types[0], "path", path)
			picture = m.Picture()
		}
		if picture == nil {
			return nil, "", fmt.Errorf("could not load embedded image from %s", path)
		}
		return io.NopCloser(bytes.NewReader(picture.Data)), path, nil
	}
}

// fromFFmpegTag 用 ffmpeg 提取内嵌封面，
// 作为 tag 库无法解析的格式（如部分视频容器）的兜底手段。
func fromFFmpegTag(ctx context.Context, ffmpeg ffmpeg.FFmpeg, path string) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		if path == "" {
			return nil, "", nil
		}
		r, err := ffmpeg.ExtractImage(ctx, path)
		if err != nil {
			return nil, "", err
		}
		return r, path, nil
	}
}

// fromAlbum 回退到所属专辑的封面，供单曲无独立封面时使用。
func fromAlbum(ctx context.Context, a *artwork, id model.ArtworkID) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		r, _, err := a.Get(ctx, id, 0, false)
		if err != nil {
			return nil, "", err
		}
		return r, id.String(), nil
	}
}

// fromAlbumPlaceholder 返回内置占位图，作为最后兜底，永不失败。
func fromAlbumPlaceholder() sourceFunc {
	return func() (io.ReadCloser, string, error) {
		r, _ := resources.FS().Open(consts.PlaceholderAlbumArt)
		return r, consts.PlaceholderAlbumArt, nil
	}
}

// fromArtistExternalSource 从外部代理获取艺术家图片。
func fromArtistExternalSource(ctx context.Context, ar model.Artist, provider external.Provider) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		imageUrl, err := provider.ArtistImage(ctx, ar.ID)
		if err != nil {
			return nil, "", err
		}

		return fromURL(ctx, imageUrl)
	}
}

// fromAlbumExternalSource 从外部代理获取专辑封面。
func fromAlbumExternalSource(ctx context.Context, al model.Album, provider external.Provider) sourceFunc {
	return func() (io.ReadCloser, string, error) {
		imageUrl, err := provider.AlbumImage(ctx, al.ID)
		if err != nil {
			return nil, "", err
		}

		return fromURL(ctx, imageUrl)
	}
}

// fromURL 下载远程图片，5 秒超时以免慢速外部服务拖住封面请求。
// 返回的 Body 由调用方负责关闭。
func fromURL(ctx context.Context, imageUrl *url.URL) (io.ReadCloser, string, error) {
	hc := http.Client{Timeout: 5 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, imageUrl.String(), nil)
	resp, err := hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, "", fmt.Errorf("error retrieving artwork from %s: %s", imageUrl, resp.Status)
	}
	return resp.Body, imageUrl.String(), nil
}
