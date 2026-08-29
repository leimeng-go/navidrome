package spotify

// SearchResults 是搜索接口的响应。
type SearchResults struct {
	Artists ArtistsResult `json:"artists"`
}

// ArtistsResult 是搜索结果中的艺人部分。
type ArtistsResult struct {
	HRef  string   `json:"href"`
	Items []Artist `json:"items"`
}

// Artist 是艺人信息，Popularity 为 0~100 的热度值。
type Artist struct {
	Genres     []string `json:"genres"`
	HRef       string   `json:"href"`
	ID         string   `json:"id"`
	Popularity int      `json:"popularity"`
	Images     []Image  `json:"images"`
	Name       string   `json:"name"`
}

// Image 是图片条目，含具体像素尺寸。
type Image struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// Error 是 Spotify 的错误响应。
type Error struct {
	Code    string `json:"error"`
	Message string `json:"error_description"`
}
