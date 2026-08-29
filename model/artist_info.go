package model

// ArtistInfo 是从外部元数据源（Last.fm / Spotify / Deezer 等）获取的艺人信息。
// 它是各 agent 返回结果的统一载体，之后再合并进 Artist 实体。
type ArtistInfo struct {
	ID             string
	Name           string
	MBID           string // MusicBrainz 艺人 ID
	Biography      string
	SmallImageUrl  string
	MediumImageUrl string
	LargeImageUrl  string
	LastFMUrl      string
	SimilarArtists Artists // 相似艺人，用于"电台"与推荐功能
}
