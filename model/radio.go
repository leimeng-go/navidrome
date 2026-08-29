package model

import "time"

// Radio 是一个网络电台条目。它不属于音乐库，仅保存外部流地址，
// 由客户端直接拉流播放，服务端不做转码或缓存。
type Radio struct {
	ID          string    `structs:"id"            json:"id"`
	StreamUrl   string    `structs:"stream_url"    json:"streamUrl"` // 电台流地址
	Name        string    `structs:"name"          json:"name"`
	HomePageUrl string    `structs:"home_page_url" json:"homePageUrl"`
	CreatedAt   time.Time `structs:"created_at"    json:"createdAt"`
	UpdatedAt   time.Time `structs:"updated_at"    json:"updatedAt"`
}

type Radios []Radio

// RadioRepository 是网络电台仓储接口。
type RadioRepository interface {
	ResourceRepository
	CountAll(options ...QueryOptions) (int64, error)
	Delete(id string) error
	Get(id string) (*Radio, error)
	GetAll(options ...QueryOptions) (Radios, error)
	Put(u *Radio) error
}
