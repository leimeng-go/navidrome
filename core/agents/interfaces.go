package agents

import (
	"context"
	"errors"

	"github.com/navidrome/navidrome/model"
)

// Constructor 是代理的构造函数，配置缺失时可返回 nil 表示该代理不可用。
type Constructor func(ds model.DataStore) Interface

// Interface 是所有代理的基础接口。
// 具体能力通过实现下方各 Retriever 接口按需声明，
// 调用方以类型断言探测，代理因而只需实现自己支持的部分。
type Interface interface {
	AgentName() string
}

// AlbumInfo contains album metadata (no images)
// AlbumInfo 是专辑的文字元数据（不含图片）。
type AlbumInfo struct {
	Name        string
	MBID        string
	Description string
	URL         string
}

// Artist 是外部代理返回的艺术家标识。
type Artist struct {
	Name string
	MBID string
}

// ExternalImage 是外部图片链接，Size 为像素边长，用于挑选合适尺寸。
type ExternalImage struct {
	URL  string
	Size int
}

// Song 是外部代理返回的单曲标识。
type Song struct {
	Name string
	MBID string
}

var (
	// ErrNotFound 表示所有代理均未查到结果。
	ErrNotFound = errors.New("not found")
)

// AlbumInfoRetriever provides album info (no images)
// AlbumInfoRetriever 提供专辑文字信息。
type AlbumInfoRetriever interface {
	GetAlbumInfo(ctx context.Context, name, artist, mbid string) (*AlbumInfo, error)
}

// AlbumImageRetriever provides album images
// AlbumImageRetriever 提供专辑封面。
type AlbumImageRetriever interface {
	GetAlbumImages(ctx context.Context, name, artist, mbid string) ([]ExternalImage, error)
}

// ArtistMBIDRetriever 提供艺术家的 MusicBrainz ID。
type ArtistMBIDRetriever interface {
	GetArtistMBID(ctx context.Context, id string, name string) (string, error)
}

// ArtistURLRetriever 提供艺术家外部主页链接。
type ArtistURLRetriever interface {
	GetArtistURL(ctx context.Context, id, name, mbid string) (string, error)
}

// ArtistBiographyRetriever 提供艺术家简介。
type ArtistBiographyRetriever interface {
	GetArtistBiography(ctx context.Context, id, name, mbid string) (string, error)
}

// ArtistSimilarRetriever 提供相似艺术家。
type ArtistSimilarRetriever interface {
	GetSimilarArtists(ctx context.Context, id, name, mbid string, limit int) ([]Artist, error)
}

// ArtistImageRetriever 提供艺术家图片。
type ArtistImageRetriever interface {
	GetArtistImages(ctx context.Context, id, name, mbid string) ([]ExternalImage, error)
}

// ArtistTopSongsRetriever 提供艺术家热门单曲。
type ArtistTopSongsRetriever interface {
	GetArtistTopSongs(ctx context.Context, id, artistName, mbid string, count int) ([]Song, error)
}

// Map 是内置代理的注册表，键为代理名。
var Map map[string]Constructor

// Register 注册内置代理，通常在各代理包的 init 中调用。
func Register(name string, init Constructor) {
	if Map == nil {
		Map = make(map[string]Constructor)
	}
	Map[name] = init
}
