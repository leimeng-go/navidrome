package subsonic

import (
	"net/http"

	"github.com/navidrome/navidrome/server/subsonic/responses"
)

// GetOpenSubsonicExtensions 声明本服务端支持的 OpenSubsonic 扩展，供客户端按能力协商。
func (api *Router) GetOpenSubsonicExtensions(_ *http.Request) (*responses.Subsonic, error) {
	response := newResponse()
	response.OpenSubsonicExtensions = &responses.OpenSubsonicExtensions{
		{Name: "transcodeOffset", Versions: []int32{1}},
		{Name: "formPost", Versions: []int32{1}},
		{Name: "songLyrics", Versions: []int32{1}},
		{Name: "indexBasedQueue", Versions: []int32{1}},
	}
	return response, nil
}
