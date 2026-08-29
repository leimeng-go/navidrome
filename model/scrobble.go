package model

import "time"

// Scrobble 是一条本地播放记录（听歌历史），与上报外部服务的缓冲队列
// （见 ScrobbleEntry）区分：这里记录的是"发生过播放"这一事实本身。
type Scrobble struct {
	MediaFileID    string
	UserID         string
	SubmissionTime time.Time
}

// ScrobbleRepository 是播放记录仓储接口。
type ScrobbleRepository interface {
	// RecordScrobble 记录一次播放。用户信息取自 context，无需显式传入
	RecordScrobble(mediaFileID string, submissionTime time.Time) error
}
