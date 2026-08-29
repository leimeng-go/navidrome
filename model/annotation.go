package model

import "time"

// Annotations 是用户维度的标注数据，以内嵌结构体形式复用于
// MediaFile、Album、Artist 等实体。它存放在独立的 annotation 表中，
// 按「用户 + 条目」维度隔离，因此同一首歌在不同用户下有各自的播放次数与评分。
type Annotations struct {
	PlayCount int64      `structs:"play_count" json:"playCount,omitempty"`
	PlayDate  *time.Time `structs:"play_date"  json:"playDate,omitempty" ` // 最近播放时间
	Rating    int        `structs:"rating"     json:"rating,omitempty"   ` // 星级评分 0-5
	RatedAt   *time.Time `structs:"rated_at"   json:"ratedAt,omitempty"  `
	Starred   bool       `structs:"starred"    json:"starred,omitempty"  ` // 是否收藏
	StarredAt *time.Time `structs:"starred_at" json:"starredAt,omitempty"`
}

// AnnotatedRepository 是支持用户标注的仓储所具备的公共能力，
// 由 MediaFile/Album/Artist 等仓储组合使用。
type AnnotatedRepository interface {
	// IncPlayCount 播放次数加一，并把 ts 记为最近播放时间
	IncPlayCount(itemID string, ts time.Time) error
	// SetStar 批量设置或取消收藏
	SetStar(starred bool, itemIDs ...string) error
	SetRating(rating int, itemID string) error
	// ReassignAnnotation 把标注从旧 ID 迁移到新 ID。
	// 当实体 ID 因元数据变化而重新计算时（如专辑 ID 变更），
	// 用它保住用户的播放次数与收藏，避免数据丢失
	ReassignAnnotation(prevID string, newID string) error
}
