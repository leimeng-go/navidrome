package model

// Genre 是流派。它本质上是 TagName 为 "genre" 的标签的视图，
// 独立成类型是为了兼容 Subsonic API 的流派相关接口。
type Genre struct {
	ID   string `structs:"id" json:"id,omitempty" toml:"id,omitempty" yaml:"id,omitempty"`
	Name string `structs:"name" json:"name"`
	// SongCount/AlbumCount 为查询时计算的引用计数，不入库也不参与序列化
	SongCount  int `structs:"-" json:"-" toml:"-" yaml:"-"`
	AlbumCount int `structs:"-" json:"-" toml:"-" yaml:"-"`
}

type Genres []Genre

// GenreRepository 是流派仓储接口（只读，流派数据由标签派生）。
type GenreRepository interface {
	GetAll(...QueryOptions) (Genres, error)
}
