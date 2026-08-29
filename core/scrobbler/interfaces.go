package scrobbler

import (
	"context"
	"errors"
	"time"

	"github.com/navidrome/navidrome/model"
)

// Scrobble 是一条待上报的播放记录：曲目 + 播放时刻。
type Scrobble struct {
	model.MediaFile
	TimeStamp time.Time
}

var (
	// ErrNotAuthorized 表示用户未授权该外部服务。
	ErrNotAuthorized = errors.New("not authorized")
	// ErrRetryLater 表示临时故障，记录应保留并稍后重试。
	ErrRetryLater = errors.New("retry later")
	// ErrUnrecoverable 表示永久性失败，记录应直接丢弃。
	ErrUnrecoverable = errors.New("unrecoverable")
)

// Scrobbler 是外部播放记录上报服务的接口（Last.fm、ListenBrainz 等）。
type Scrobbler interface {
	IsAuthorized(ctx context.Context, userId string) bool
	NowPlaying(ctx context.Context, userId string, track *model.MediaFile, position int) error
	Scrobble(ctx context.Context, userId string, s Scrobble) error
}

// Constructor 是 Scrobbler 的构造函数，配置缺失时返回 nil。
type Constructor func(ds model.DataStore) Scrobbler
