package lastfm

// Response 是 Last.fm 各接口的统一响应结构。
// Error 非 0 表示业务错误，此时其余字段无意义。
type Response struct {
	Artist         Artist         `json:"artist"`
	SimilarArtists SimilarArtists `json:"similarartists"`
	TopTracks      TopTracks      `json:"toptracks"`
	Album          Album          `json:"album"`
	Error          int            `json:"error"`
	Message        string         `json:"message"`
	Token          string         `json:"token"`
	Session        Session        `json:"session"`
	NowPlaying     NowPlaying     `json:"nowplaying"`
	Scrobbles      Scrobbles      `json:"scrobbles"`
}

// Album 是专辑信息。
type Album struct {
	Name        string          `json:"name"`
	MBID        string          `json:"mbid"`
	URL         string          `json:"url"`
	Image       []ExternalImage `json:"image"`
	Description Description     `json:"wiki"`
}

// Artist 是艺人信息。
type Artist struct {
	Name  string          `json:"name"`
	MBID  string          `json:"mbid"`
	URL   string          `json:"url"`
	Image []ExternalImage `json:"image"`
	Bio   Description     `json:"bio"`
}

// SimilarArtists 是相似艺人列表。
type SimilarArtists struct {
	Artists []Artist `json:"artist"`
	Attr    Attr     `json:"@attr"`
}

// Attr 是响应中的 @attr 附加属性。
type Attr struct {
	Artist string `json:"artist"`
}

// ExternalImage 是图片条目，Size 为 small/medium/large 等文字描述。
type ExternalImage struct {
	URL  string `json:"#text"`
	Size string `json:"size"`
}

// Description 对应 wiki/bio 字段。
type Description struct {
	Published string `json:"published"`
	Summary   string `json:"summary"`
	Content   string `json:"content"`
}

// Track 是曲目信息。
type Track struct {
	Name string `json:"name"`
	MBID string `json:"mbid"`
}

// TopTracks 是热门曲目列表。
type TopTracks struct {
	Track []Track `json:"track"`
	Attr  Attr    `json:"@attr"`
}

// Session 是授权会话，Key 为长期有效的 session key。
type Session struct {
	Name       string `json:"name"`
	Key        string `json:"key"`
	Subscriber int    `json:"subscriber"`
}

// NowPlaying 是「正在播放」上报的响应。
// Corrected 表示 Last.fm 对提交内容做了自动纠正。
type NowPlaying struct {
	Artist struct {
		Corrected string `json:"corrected"`
		Text      string `json:"#text"`
	} `json:"artist"`
	IgnoredMessage struct {
		Code string `json:"code"`
		Text string `json:"#text"`
	} `json:"ignoredMessage"`
	Album struct {
		Corrected string `json:"corrected"`
		Text      string `json:"#text"`
	} `json:"album"`
	AlbumArtist struct {
		Corrected string `json:"corrected"`
		Text      string `json:"#text"`
	} `json:"albumArtist"`
	Track struct {
		Corrected string `json:"corrected"`
		Text      string `json:"#text"`
	} `json:"track"`
}

// Scrobbles 是播放上报的响应，Accepted/Ignored 表示被接受与被忽略的条数。
type Scrobbles struct {
	Attr struct {
		Accepted int `json:"accepted"`
		Ignored  int `json:"ignored"`
	} `json:"@attr"`
	Scrobble struct {
		Artist struct {
			Corrected string `json:"corrected"`
			Text      string `json:"#text"`
		} `json:"artist"`
		IgnoredMessage struct {
			Code string `json:"code"`
			Text string `json:"#text"`
		} `json:"ignoredMessage"`
		AlbumArtist struct {
			Corrected string `json:"corrected"`
			Text      string `json:"#text"`
		} `json:"albumArtist"`
		Timestamp string `json:"timestamp"`
		Album     struct {
			Corrected string `json:"corrected"`
			Text      string `json:"#text"`
		} `json:"album"`
		Track struct {
			Corrected string `json:"corrected"`
			Text      string `json:"#text"`
		} `json:"track"`
	} `json:"scrobble"`
}
