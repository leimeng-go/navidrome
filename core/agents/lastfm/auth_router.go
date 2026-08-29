package lastfm

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"net/http"
	"time"

	"github.com/deluan/rest"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/utils/req"
)

//go:embed token_received.html
var tokenReceivedPage []byte

// Router 提供 Last.fm 账号关联相关的 HTTP 接口。
type Router struct {
	http.Handler
	ds          model.DataStore
	sessionKeys *agents.SessionKeys
	client      *client
	apiKey      string
	secret      string
}

// NewRouter 创建关联路由。
func NewRouter(ds model.DataStore) *Router {
	r := &Router{
		ds:          ds,
		apiKey:      conf.Server.LastFM.ApiKey,
		secret:      conf.Server.LastFM.Secret,
		sessionKeys: &agents.SessionKeys{DataStore: ds, KeyName: sessionKeyProperty},
	}
	r.Handler = r.routes()
	hc := &http.Client{
		Timeout: consts.DefaultHttpClientTimeOut,
	}
	r.client = newClient(r.apiKey, r.secret, "en", hc)
	return r
}

// routes 注册路由。
// 查询与解绑需要登录；回调由 Last.fm 跳转触发，不能要求认证，
// 故用 URL 中的 uid 参数标识用户。
func (s *Router) routes() http.Handler {
	r := chi.NewRouter()

	r.Group(func(r chi.Router) {
		r.Use(server.Authenticator(s.ds))
		r.Use(server.JWTRefresher)

		r.Get("/link", s.getLinkStatus)
		r.Delete("/link", s.unlink)
	})

	r.Get("/link/callback", s.callback)

	return r
}

// getLinkStatus 返回当前用户是否已关联，并附带 apiKey 供前端拼装授权链接。
func (s *Router) getLinkStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]interface{}{
		"apiKey": s.apiKey,
	}
	u, _ := request.UserFrom(r.Context())
	key, err := s.sessionKeys.Get(r.Context(), u.ID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		resp["error"] = err
		resp["status"] = false
		_ = rest.RespondWithJSON(w, http.StatusInternalServerError, resp)
		return
	}
	resp["status"] = key != ""
	_ = rest.RespondWithJSON(w, http.StatusOK, resp)
}

// unlink 解除关联，删除保存的 session key。
func (s *Router) unlink(w http.ResponseWriter, r *http.Request) {
	u, _ := request.UserFrom(r.Context())
	err := s.sessionKeys.Delete(r.Context(), u.ID)
	if err != nil {
		_ = rest.RespondWithError(w, http.StatusInternalServerError, err.Error())
	} else {
		_ = rest.RespondWithJSON(w, http.StatusOK, map[string]string{})
	}
}

// callback 处理 Last.fm 授权回调，用令牌换取 session key 并落库。
func (s *Router) callback(w http.ResponseWriter, r *http.Request) {
	p := req.Params(r)
	token, err := p.String("token")
	if err != nil {
		_ = rest.RespondWithError(w, http.StatusBadRequest, "token not received")
		return
	}
	uid, err := p.String("uid")
	if err != nil {
		_ = rest.RespondWithError(w, http.StatusBadRequest, "uid not received")
		return
	}

	// Need to add user to context, as this is a non-authenticated endpoint, so it does not
	// automatically contain any user info
	ctx := request.WithUser(r.Context(), model.User{ID: uid})
	err = s.fetchSessionKey(ctx, uid, token)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("An error occurred while authorizing with Last.fm. \n\nRequest ID: " + middleware.GetReqID(ctx)))
		return
	}

	http.ServeContent(w, r, "response", time.Now(), bytes.NewReader(tokenReceivedPage))
}

// fetchSessionKey 换取并保存 session key。
// 出错日志带上 requestId，便于与返回给用户的错误页对应排查。
func (s *Router) fetchSessionKey(ctx context.Context, uid, token string) error {
	sessionKey, err := s.client.getSession(ctx, token)
	if err != nil {
		log.Error(ctx, "Could not fetch LastFM session key", "userId", uid, "token", token,
			"requestId", middleware.GetReqID(ctx), err)
		return err
	}
	err = s.sessionKeys.Put(ctx, uid, sessionKey)
	if err != nil {
		log.Error("Could not save LastFM session key", "userId", uid, "requestId", middleware.GetReqID(ctx), err)
	}
	return err
}
