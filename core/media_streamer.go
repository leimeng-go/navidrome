package core

import (
	"context"
	"fmt"
	"io"
	"mime"
	"os"
	"sync"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/cache"
)

// MediaStreamer 负责生成音频流，按需进行转码。
type MediaStreamer interface {
	NewStream(ctx context.Context, id string, reqFormat string, reqBitRate int, offset int) (*Stream, error)
	DoStream(ctx context.Context, mf *model.MediaFile, reqFormat string, reqBitRate int, reqOffset int) (*Stream, error)
}

// TranscodingCache 是转码结果的文件缓存，相同参数的重复请求可直接命中。
type TranscodingCache cache.FileCache

// NewMediaStreamer 创建流媒体服务。
func NewMediaStreamer(ds model.DataStore, t ffmpeg.FFmpeg, cache TranscodingCache) MediaStreamer {
	return &mediaStreamer{ds: ds, transcoder: t, cache: cache}
}

type mediaStreamer struct {
	ds         model.DataStore
	transcoder ffmpeg.FFmpeg
	cache      cache.FileCache
}

// streamJob 是一次转码任务，同时充当缓存条目。
type streamJob struct {
	ms       *mediaStreamer
	mf       *model.MediaFile
	filePath string
	format   string
	bitRate  int
	offset   int
}

// Key 生成缓存键。
// 纳入 UpdatedAt 是关键：文件被替换后键随之变化，自动失效旧的转码结果。
func (j *streamJob) Key() string {
	return fmt.Sprintf("%s.%s.%d.%s.%d", j.mf.ID, j.mf.UpdatedAt.Format(time.RFC3339Nano), j.bitRate, j.format, j.offset)
}

// NewStream 按媒体文件 ID 创建音频流。
func (ms *mediaStreamer) NewStream(ctx context.Context, id string, reqFormat string, reqBitRate int, reqOffset int) (*Stream, error) {
	mf, err := ms.ds.MediaFile(ctx).Get(id)
	if err != nil {
		return nil, err
	}

	return ms.DoStream(ctx, mf, reqFormat, reqBitRate, reqOffset)
}

// DoStream 生成音频流。
//
// 分两条路径：format 为 "raw" 时直接打开原文件，可随机定位（支持 seek）；
// 否则走转码缓存，缓存命中时同样可 seek，实时转码的流则只能顺序读取。
func (ms *mediaStreamer) DoStream(ctx context.Context, mf *model.MediaFile, reqFormat string, reqBitRate int, reqOffset int) (*Stream, error) {
	var format string
	var bitRate int
	var cached bool
	defer func() {
		log.Info(ctx, "Streaming file", "title", mf.Title, "artist", mf.Artist, "format", format, "cached", cached,
			"bitRate", bitRate, "user", userName(ctx), "transcoding", format != "raw",
			"originalFormat", mf.Suffix, "originalBitRate", mf.BitRate)
	}()

	format, bitRate = selectTranscodingOptions(ctx, ms.ds, mf, reqFormat, reqBitRate)
	s := &Stream{ctx: ctx, mf: mf, format: format, bitRate: bitRate}
	filePath := mf.AbsolutePath()

	if format == "raw" {
		log.Debug(ctx, "Streaming RAW file", "id", mf.ID, "path", filePath,
			"requestBitrate", reqBitRate, "requestFormat", reqFormat, "requestOffset", reqOffset,
			"originalBitrate", mf.BitRate, "originalFormat", mf.Suffix,
			"selectedBitrate", bitRate, "selectedFormat", format)
		f, err := os.Open(filePath)
		if err != nil {
			return nil, err
		}
		s.ReadCloser = f
		s.Seeker = f
		s.format = mf.Suffix
		return s, nil
	}

	job := &streamJob{
		ms:       ms,
		mf:       mf,
		filePath: filePath,
		format:   format,
		bitRate:  bitRate,
		offset:   reqOffset,
	}
	r, err := ms.cache.Get(ctx, job)
	if err != nil {
		log.Error(ctx, "Error accessing transcoding cache", "id", mf.ID, err)
		return nil, err
	}
	cached = r.Cached

	s.ReadCloser = r
	s.Seeker = r.Seeker

	log.Debug(ctx, "Streaming TRANSCODED file", "id", mf.ID, "path", filePath,
		"requestBitrate", reqBitRate, "requestFormat", reqFormat, "requestOffset", reqOffset,
		"originalBitrate", mf.BitRate, "originalFormat", mf.Suffix,
		"selectedBitrate", bitRate, "selectedFormat", format, "cached", cached, "seekable", s.Seekable())

	return s, nil
}

// Stream 是一路音频流，附带 HTTP 响应所需的元信息。
// Seeker 可能为 nil（实时转码），此时不支持范围请求。
type Stream struct {
	ctx     context.Context
	mf      *model.MediaFile
	bitRate int
	format  string
	io.ReadCloser
	io.Seeker
}

func (s *Stream) Seekable() bool      { return s.Seeker != nil }
func (s *Stream) Duration() float32   { return s.mf.Duration }
func (s *Stream) ContentType() string { return mime.TypeByExtension("." + s.format) }
func (s *Stream) Name() string        { return s.mf.Title + "." + s.format }
func (s *Stream) ModTime() time.Time  { return s.mf.UpdatedAt }

// EstimatedContentLength 由时长与比特率估算字节数。
// 转码流长度事先未知，只能给出估计值供客户端显示进度。
func (s *Stream) EstimatedContentLength() int {
	return int(s.mf.Duration * float32(s.bitRate) / 8 * 1024)
}

// TODO This function deserves some love (refactoring)
//
// selectTranscodingOptions 决定实际使用的输出格式与比特率。
//
// 优先级：显式请求的格式 > 播放器绑定的转码配置 > 默认降采样格式。
// 播放器若设置了 MaxBitRate，会覆盖转码配置的默认比特率。
// 默认降采样仅在请求比特率低于原始比特率时启用——升采样毫无意义。
//
// 最后有一道兜底：若目标格式与原格式相同且比特率不低于原值，
// 则退回 "raw" 直接推流，避免无谓的转码开销。
func selectTranscodingOptions(ctx context.Context, ds model.DataStore, mf *model.MediaFile, reqFormat string, reqBitRate int) (format string, bitRate int) {
	format = "raw"
	if reqFormat == "raw" {
		return format, 0
	}
	if reqFormat == mf.Suffix && reqBitRate == 0 {
		bitRate = mf.BitRate
		return format, bitRate
	}
	trc, hasDefault := request.TranscodingFrom(ctx)
	var cFormat string
	var cBitRate int
	if reqFormat != "" {
		cFormat = reqFormat
	} else {
		if hasDefault {
			cFormat = trc.TargetFormat
			cBitRate = trc.DefaultBitRate
			if p, ok := request.PlayerFrom(ctx); ok {
				cBitRate = p.MaxBitRate
			}
		} else if reqBitRate > 0 && reqBitRate < mf.BitRate && conf.Server.DefaultDownsamplingFormat != "" {
			// If no format is specified and no transcoding associated to the player, but a bitrate is specified,
			// and there is no transcoding set for the player, we use the default downsampling format.
			// But only if the requested bitRate is lower than the original bitRate.
			log.Debug("Default Downsampling", "Using default downsampling format", conf.Server.DefaultDownsamplingFormat)
			cFormat = conf.Server.DefaultDownsamplingFormat
		}
	}
	if reqBitRate > 0 {
		cBitRate = reqBitRate
	}
	if cBitRate == 0 && cFormat == "" {
		return format, bitRate
	}
	t, err := ds.Transcoding(ctx).FindByFormat(cFormat)
	if err == nil {
		format = t.TargetFormat
		if cBitRate != 0 {
			bitRate = cBitRate
		} else {
			bitRate = t.DefaultBitRate
		}
	}
	if format == mf.Suffix && bitRate >= mf.BitRate {
		format = "raw"
		bitRate = 0
	}
	return format, bitRate
}

var (
	onceTranscodingCache     sync.Once
	instanceTranscodingCache TranscodingCache
)

// GetTranscodingCache 返回转码缓存单例。
func GetTranscodingCache() TranscodingCache {
	onceTranscodingCache.Do(func() {
		instanceTranscodingCache = NewTranscodingCache()
	})
	return instanceTranscodingCache
}

// NewTranscodingCache 创建转码文件缓存，缓存未命中时调用 ffmpeg 实时转码。
//
// 上下文的选择很关键：
// 开启取消功能时直接用请求上下文，客户端断开即终止 ffmpeg，节省 CPU；
// 关闭时改用 background 上下文并保留请求值，
// 让转码跑完并完整写入缓存——用户断开后重连即可直接命中，
// 代价是可能为无人收听的流白白转码。
func NewTranscodingCache() TranscodingCache {
	return cache.NewFileCache("Transcoding", conf.Server.TranscodingCacheSize,
		consts.TranscodingCacheDir, consts.DefaultTranscodingCacheMaxItems,
		func(ctx context.Context, arg cache.Item) (io.Reader, error) {
			job := arg.(*streamJob)
			t, err := job.ms.ds.Transcoding(ctx).FindByFormat(job.format)
			if err != nil {
				log.Error(ctx, "Error loading transcoding command", "format", job.format, err)
				return nil, os.ErrInvalid
			}

			// Choose the appropriate context based on EnableTranscodingCancellation configuration.
			// This is where we decide whether transcoding processes should be cancellable or not.
			var transcodingCtx context.Context
			if conf.Server.EnableTranscodingCancellation {
				// Use the request context directly, allowing cancellation when client disconnects
				transcodingCtx = ctx
			} else {
				// Use background context with request values preserved.
				// This prevents cancellation but maintains request metadata (user, client, etc.)
				transcodingCtx = request.AddValues(context.Background(), ctx)
			}

			out, err := job.ms.transcoder.Transcode(transcodingCtx, t.Command, job.filePath, job.bitRate, job.offset)
			if err != nil {
				log.Error(ctx, "Error starting transcoder", "id", job.mf.ID, err)
				return nil, os.ErrInvalid
			}
			return out, nil
		})
}
