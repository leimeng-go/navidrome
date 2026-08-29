package subsonic

import (
	"net/http"

	"github.com/navidrome/navidrome/server/subsonic/responses"
)

// Ping 连通性与凭据校验探针。
func (api *Router) Ping(_ *http.Request) (*responses.Subsonic, error) {
	return newResponse(), nil
}

// GetLicense 返回授权状态。Navidrome 无授权限制，始终有效。
func (api *Router) GetLicense(_ *http.Request) (*responses.Subsonic, error) {
	response := newResponse()
	response.License = &responses.License{Valid: true}
	return response, nil
}
