package model

import "time"

// ScrobbleEntry 是待上报到外部服务（Last.fm / ListenBrainz 等）的一条听歌记录。
// 采用持久化缓冲队列而非直接上报，以便在外部服务不可用或网络故障时重试，
// 且重启后不丢数据。内嵌 MediaFile 便于上报时直接取用曲目元数据。
type ScrobbleEntry struct {
	ID      string
	Service string // 目标服务标识，如 "lastfm"、"listenbrainz"
	UserID  string
	// PlayTime 实际播放时间（上报给外部服务的时间戳）；
	// EnqueueTime 入队时间，用于队列老化与清理
	PlayTime    time.Time
	EnqueueTime time.Time
	MediaFileID string
	MediaFile
}

type ScrobbleEntries []ScrobbleEntry

// ScrobbleBufferRepository 是上报缓冲队列仓储接口，队列按「服务 + 用户」分片，
// 使各用户各服务的上报互不阻塞。
type ScrobbleBufferRepository interface {
	// UserIDs 返回该服务下有待上报记录的用户列表，供后台协程轮询
	UserIDs(service string) ([]string, error)
	Enqueue(service, userId, mediaFileId string, playTime time.Time) error
	// Next 取出队首待上报记录（不删除），上报成功后再调用 Dequeue
	Next(service string, userId string) (*ScrobbleEntry, error)
	// Dequeue 上报成功后移除该记录
	Dequeue(entry *ScrobbleEntry) error
	// Length 返回队列总长度，用于监控积压情况
	Length() (int64, error)
}
