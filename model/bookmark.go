package model

import "time"

// Bookmarkable 是书签能力的内嵌结构体，为实体提供"记住播放位置"的字段。
// 主要用于有声书与播客等长音频，可从上次中断处继续播放。
type Bookmarkable struct {
	BookmarkPosition int64 `structs:"-" json:"bookmarkPosition"` // 播放位置（毫秒），由 JOIN 得到
}

// BookmarkableRepository 是支持书签的仓储所具备的能力。
// 用户信息取自 context，因此各用户的书签互相隔离。
type BookmarkableRepository interface {
	// AddBookmark 新增或更新书签
	AddBookmark(id, comment string, position int64) error
	DeleteBookmark(id string) error
	GetBookmarks() (Bookmarks, error)
}

// Bookmark 是一条书签记录。
type Bookmark struct {
	Item      MediaFile `structs:"item" json:"item"`
	Comment   string    `structs:"comment" json:"comment"`
	Position  int64     `structs:"position" json:"position"`     // 播放位置（毫秒）
	ChangedBy string    `structs:"changed_by" json:"changed_by"` // 最后修改的客户端名
	CreatedAt time.Time `structs:"created_at" json:"createdAt"`
	UpdatedAt time.Time `structs:"updated_at" json:"updatedAt"`
}

type Bookmarks []Bookmark
