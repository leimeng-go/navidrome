package scrobbler

import (
	"context"
	"errors"
	"time"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// newBufferedScrobbler 为 scrobbler 套一层持久化缓冲，并启动后台发送协程。
func newBufferedScrobbler(ds model.DataStore, s Scrobbler, service string) *bufferedScrobbler {
	ctx, cancel := context.WithCancel(context.Background())
	b := &bufferedScrobbler{
		ds:         ds,
		wrapped:    s,
		service:    service,
		wakeSignal: make(chan struct{}, 1),
		ctx:        ctx,
		cancel:     cancel,
	}
	go b.run(ctx)
	return b
}

// bufferedScrobbler 为 Scrobbler 增加持久化缓冲：
// 播放记录先落库再异步发送，外部服务不可用或进程重启都不会丢记录。
// NowPlaying 不缓冲——它有强时效性，过期重发毫无意义。
type bufferedScrobbler struct {
	ds         model.DataStore
	wrapped    Scrobbler
	service    string
	wakeSignal chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
}

// Stop 停止后台发送协程，插件被卸载时调用。
func (b *bufferedScrobbler) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
}

func (b *bufferedScrobbler) IsAuthorized(ctx context.Context, userId string) bool {
	return b.wrapped.IsAuthorized(ctx, userId)
}

// NowPlaying 直接透传，不进缓冲。
func (b *bufferedScrobbler) NowPlaying(ctx context.Context, userId string, track *model.MediaFile, position int) error {
	return b.wrapped.NowPlaying(ctx, userId, track, position)
}

// Scrobble 只把记录写入缓冲表并唤醒后台协程，立即返回，不等待外部服务。
func (b *bufferedScrobbler) Scrobble(ctx context.Context, userId string, s Scrobble) error {
	err := b.ds.ScrobbleBuffer(ctx).Enqueue(b.service, userId, s.ID, s.TimeStamp)
	if err != nil {
		return err
	}

	b.sendWakeSignal()
	return nil
}

// sendWakeSignal 唤醒后台协程，通道容量 1，已有信号时直接丢弃。
func (b *bufferedScrobbler) sendWakeSignal() {
	// Don't block if the previous signal was not read yet
	select {
	case b.wakeSignal <- struct{}{}:
	default:
	}
}

// run 后台发送循环。
// 处理失败（多为外部服务暂时不可用）时安排 5 秒后自唤醒重试，
// 期间协程阻塞在 select 上，不占用 CPU。
func (b *bufferedScrobbler) run(ctx context.Context) {
	for {
		if !b.processQueue(ctx) {
			time.AfterFunc(5*time.Second, func() {
				b.sendWakeSignal()
			})
		}
		select {
		case <-b.wakeSignal:
			continue
		case <-ctx.Done():
			return
		}
	}
}

// processQueue 逐个用户处理缓冲队列。
// 按用户分组是因为每个用户在外部服务有各自的授权凭据。
// 任一用户处理失败即返回 false 以触发整体重试。
func (b *bufferedScrobbler) processQueue(ctx context.Context) bool {
	buffer := b.ds.ScrobbleBuffer(ctx)
	userIds, err := buffer.UserIDs(b.service)
	if err != nil {
		log.Error(ctx, "Error retrieving userIds from scrobble buffer", "scrobbler", b.service, err)
		return false
	}
	result := true
	for _, userId := range userIds {
		if !b.processUserQueue(ctx, userId) {
			result = false
		}
	}
	return result
}

// processUserQueue 按顺序发送某用户的缓冲记录，直到队列清空。
//
// 错误分两类：ErrRetryLater（网络故障、限流）保留记录并稍后重试；
// 其他错误视为永久失败，记录日志后出队丢弃，
// 否则一条无效记录会永久阻塞后续队列。
func (b *bufferedScrobbler) processUserQueue(ctx context.Context, userId string) bool {
	buffer := b.ds.ScrobbleBuffer(ctx)
	for {
		entry, err := buffer.Next(b.service, userId)
		if err != nil {
			log.Error(ctx, "Error reading from scrobble buffer", "scrobbler", b.service, err)
			return false
		}
		if entry == nil {
			return true
		}
		log.Debug(ctx, "Sending scrobble", "scrobbler", b.service, "track", entry.Title, "artist", entry.Artist)
		err = b.wrapped.Scrobble(ctx, entry.UserID, Scrobble{
			MediaFile: entry.MediaFile,
			TimeStamp: entry.PlayTime,
		})
		if errors.Is(err, ErrRetryLater) {
			log.Warn(ctx, "Could not send scrobble. Will be retried", "userId", entry.UserID,
				"track", entry.Title, "artist", entry.Artist, "scrobbler", b.service, err)
			return false
		}
		if err != nil {
			log.Error(ctx, "Error sending scrobble to service. Discarding", "scrobbler", b.service,
				"userId", entry.UserID, "artist", entry.Artist, "track", entry.Title, err)
		}
		err = buffer.Dequeue(entry)
		if err != nil {
			log.Error(ctx, "Error removing entry from scrobble buffer", "userId", entry.UserID,
				"track", entry.Title, "artist", entry.Artist, "scrobbler", b.service, err)
			return false
		}
	}
}

var _ Scrobbler = (*bufferedScrobbler)(nil)
