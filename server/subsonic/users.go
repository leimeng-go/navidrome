package subsonic

import (
	"net/http"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
)

// buildUserResponse creates a User response object from a User model
// buildUserResponse 构造用户响应。
// Navidrome 没有 Subsonic 那套细粒度角色，权限由全局配置推导：
// 下载/分享跟随服务端开关，Jukebox 还要看是否限管理员。
func buildUserResponse(user model.User) responses.User {
	userResponse := responses.User{
		Username:          user.UserName,
		AdminRole:         user.IsAdmin,
		Email:             user.Email,
		StreamRole:        true,
		ScrobblingEnabled: true,
		DownloadRole:      conf.Server.EnableDownloads,
		ShareRole:         conf.Server.EnableSharing,
		Folder:            slice.Map(user.Libraries, func(lib model.Library) int32 { return int32(lib.ID) }),
	}

	if conf.Server.Jukebox.Enabled {
		userResponse.JukeboxRole = !conf.Server.Jukebox.AdminOnly || user.IsAdmin
	}

	return userResponse
}

// GetUser 返回用户信息。只允许查询自己，避免泄露他人账号信息。
func (api *Router) GetUser(r *http.Request) (*responses.Subsonic, error) {
	loggedUser, ok := request.UserFrom(r.Context())
	if !ok {
		return nil, newError(responses.ErrorGeneric, "Internal error")
	}
	username, err := req.Params(r).String("username")
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(username, loggedUser.UserName) {
		return nil, newError(responses.ErrorAuthorizationFail)
	}
	response := newResponse()
	user := buildUserResponse(loggedUser)
	response.User = &user
	return response, nil
}

// GetUsers 返回用户列表。出于同样的隐私考虑，只返回当前登录用户。
func (api *Router) GetUsers(r *http.Request) (*responses.Subsonic, error) {
	loggedUser, ok := request.UserFrom(r.Context())
	if !ok {
		return nil, newError(responses.ErrorGeneric, "Internal error")
	}

	user := buildUserResponse(loggedUser)
	response := newResponse()
	response.Users = &responses.Users{User: []responses.User{user}}
	return response, nil
}
