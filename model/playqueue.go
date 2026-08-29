package model

import (
	"time"
)

// PlayQueue 是用户的播放队列，支持跨设备同步：
// 在手机上暂停后可在网页端接着播放。每个用户只保留一条队列记录。
type PlayQueue struct {
	ID     string `structs:"id" json:"id"`
	UserID string `structs:"user_id" json:"userId"`
	// Current 是当前曲目在 Items 中的下标；Position 是该曲目内的播放位置（毫秒）
	Current  int   `structs:"current" json:"current"`
	Position int64 `structs:"position" json:"position"`
	// ChangedBy 记录最后修改队列的客户端名，便于该客户端识别"是我自己改的"
	// 从而避免不必要的同步回写
	ChangedBy string     `structs:"changed_by" json:"changedBy"`
	Items     MediaFiles `structs:"-" json:"items,omitempty"`
	CreatedAt time.Time  `structs:"created_at" json:"createdAt"`
	UpdatedAt time.Time  `structs:"updated_at" json:"updatedAt"`
}

type PlayQueues []PlayQueue

// PlayQueueRepository 是播放队列仓储接口。
type PlayQueueRepository interface {
	// Store 保存队列；传入 colNames 可限定只更新指定列
	Store(queue *PlayQueue, colNames ...string) error
	// Retrieve returns the playqueue without loading the full MediaFiles
	// (Items only contain IDs)
	// Retrieve 返回队列但不载入完整曲目信息，Items 中仅有 ID。
	// 适用于只需知道队列构成的轻量场景
	Retrieve(userId string) (*PlayQueue, error)
	// RetrieveWithMediaFiles returns the playqueue with full MediaFiles loaded
	// RetrieveWithMediaFiles 返回队列并载入完整曲目信息，代价更高
	RetrieveWithMediaFiles(userId string) (*PlayQueue, error)
	Clear(userId string) error
}
