package artwork

import (
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/cache"
	"github.com/navidrome/navidrome/utils/pl"
)

// CacheWarmer 在后台预热封面缓存。
type CacheWarmer interface {
	PreCache(artID model.ArtworkID)
}

// NewCacheWarmer creates a new CacheWarmer instance. The CacheWarmer will pre-cache Artwork images in the background
// to speed up the response time when the image is requested by the UI. The cache is pre-populated with the original
// image size, as well as the size defined in the UICoverArtSize constant.
//
// NewCacheWarmer 创建缓存预热器。
// 扫描时提前生成 UI 常用尺寸的缩略图，用户浏览时即可直接命中缓存。
// 缓存被禁用时返回空实现，让调用方无需到处判断开关。
//
// 后台上下文中注入虚拟管理员：预热播放列表封面需要访问权限，
// 而后台任务并没有真实的登录用户。
func NewCacheWarmer(artwork Artwork, cache cache.FileCache) CacheWarmer {
	// If image cache is disabled, return a NOOP implementation
	if conf.Server.ImageCacheSize == "0" || !conf.Server.EnableArtworkPrecache {
		return &noopCacheWarmer{}
	}

	// If the file cache is disabled, return a NOOP implementation
	if cache.Disabled(context.Background()) {
		log.Debug("Image cache disabled. Cache warmer will not run")
		return &noopCacheWarmer{}
	}

	a := &cacheWarmer{
		artwork:    artwork,
		cache:      cache,
		buffer:     make(map[model.ArtworkID]struct{}),
		wakeSignal: make(chan struct{}, 1),
	}

	// Create a context with a fake admin user, to be able to pre-cache Playlist CoverArts
	ctx := request.WithUser(context.TODO(), model.User{IsAdmin: true})
	go a.run(ctx)
	return a
}

// cacheWarmer 用 map 缓冲待预热的封面 ID，天然去重，由后台协程分批处理。
type cacheWarmer struct {
	artwork    Artwork
	buffer     map[model.ArtworkID]struct{}
	mutex      sync.Mutex
	cache      cache.FileCache
	wakeSignal chan struct{}
}

// PreCache 把封面 ID 加入预热队列，立即返回，不做实际取图。
func (a *cacheWarmer) PreCache(artID model.ArtworkID) {
	if a.cache.Disabled(context.Background()) {
		return
	}
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.buffer[artID] = struct{}{}
	a.sendWakeSignal()
}

// sendWakeSignal 唤醒后台协程，通道容量 1，不阻塞调用方。
func (a *cacheWarmer) sendWakeSignal() {
	// Don't block if the previous signal was not read yet
	select {
	case a.wakeSignal <- struct{}{}:
	default:
	}
}

// run 后台预热循环，由信号或 10 秒超时驱动。
//
// 缓存被禁用则清空缓冲并退出；
// 缓存暂不可用（如仍在初始化）则保留缓冲继续等待。
// 每轮整体换出缓冲后再解锁处理，期间新请求可继续入队。
func (a *cacheWarmer) run(ctx context.Context) {
	for {
		a.waitSignal(ctx, 10*time.Second)
		if ctx.Err() != nil {
			break
		}

		if a.cache.Disabled(ctx) {
			a.mutex.Lock()
			pending := len(a.buffer)
			a.buffer = make(map[model.ArtworkID]struct{})
			a.mutex.Unlock()
			if pending > 0 {
				log.Trace(ctx, "Cache disabled, discarding precache buffer", "bufferLen", pending)
			}
			return
		}

		// If cache not available, keep waiting
		if !a.cache.Available(ctx) {
			a.mutex.Lock()
			bufferLen := len(a.buffer)
			a.mutex.Unlock()
			if bufferLen > 0 {
				log.Trace(ctx, "Cache not available, buffering precache request", "bufferLen", bufferLen)
			}
			continue
		}

		a.mutex.Lock()

		// If there's nothing to send, keep waiting
		if len(a.buffer) == 0 {
			a.mutex.Unlock()
			continue
		}

		batch := slices.Collect(maps.Keys(a.buffer))
		a.buffer = make(map[model.ArtworkID]struct{})
		a.mutex.Unlock()

		a.processBatch(ctx, batch)
	}
}

// waitSignal 等待唤醒信号、超时或上下文取消，三者任一即返回。
func (a *cacheWarmer) waitSignal(ctx context.Context, timeout time.Duration) {
	select {
	case <-time.After(timeout):
	case <-a.wakeSignal:
	case <-ctx.Done():
	}
}

// processBatch 以 2 路并发处理一批预热请求。
// 并发度刻意压低：预热是后台低优先级任务，不应与用户请求争抢资源。
func (a *cacheWarmer) processBatch(ctx context.Context, batch []model.ArtworkID) {
	log.Trace(ctx, "PreCaching a new batch of artwork", "batchSize", len(batch))
	input := pl.FromSlice(ctx, batch)
	errs := pl.Sink(ctx, 2, input, a.doCacheImage)
	for err := range errs {
		log.Debug(ctx, "Error warming cache", err)
	}
}

// doCacheImage 取一次图以触发缓存写入，数据本身丢弃。
// 单张限时 10 秒，避免个别慢图拖住整批预热。
func (a *cacheWarmer) doCacheImage(ctx context.Context, id model.ArtworkID) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	r, _, err := a.artwork.Get(ctx, id, consts.UICoverArtSize, true)
	if err != nil {
		return fmt.Errorf("caching id='%s': %w", id, err)
	}
	defer r.Close()
	_, err = io.Copy(io.Discard, r)
	if err != nil {
		return err
	}
	return nil
}

// NoopCacheWarmer 返回空实现，用于测试或禁用预热的场景。
func NoopCacheWarmer() CacheWarmer {
	return &noopCacheWarmer{}
}

// noopCacheWarmer 是不做任何事的预热器。
type noopCacheWarmer struct{}

func (a *noopCacheWarmer) PreCache(model.ArtworkID) {}
