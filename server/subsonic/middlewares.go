package subsonic

import (
	"cmp"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	ua "github.com/mileusna/useragent"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/req"
)

// postFormToQueryParams 把 POST 表单参数并入查询串。
// Subsonic 允许以 GET 或 POST 表单传参，统一到一处后下游只需读查询参数。
func postFormToQueryParams(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := r.ParseForm()
		if err != nil {
			sendError(w, r, newError(responses.ErrorGeneric, err.Error()))
		}
		var parts []string
		for key, values := range r.Form {
			for _, v := range values {
				parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(v))
			}
		}
		r.URL.RawQuery = strings.Join(parts, "&")

		next.ServeHTTP(w, r)
	})
}

// fromInternalOrProxyAuth 尝试从内部调用或反向代理头中取用户名。
// 内部调用没有代理 IP，因此一旦命中就不再走反向代理认证，否则必然失败。
func fromInternalOrProxyAuth(r *http.Request) (string, bool) {
	username := server.InternalAuth(r)

	// If the username comes from internal auth, do not also do reverse proxy auth, as
	// the request will have no reverse proxy IP
	if username != "" {
		return username, true
	}

	return server.UsernameFromExtAuthHeader(r), false
}

// checkRequiredParameters 校验 Subsonic 必需参数（用户名、协议版本、客户端名）。
// 已由内部或代理认证确定身份时，无需再要求 u 参数。
func checkRequiredParameters(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requiredParameters []string

		username, _ := fromInternalOrProxyAuth(r)
		if username != "" {
			requiredParameters = []string{"v", "c"}
		} else {
			requiredParameters = []string{"u", "v", "c"}
		}

		p := req.Params(r)
		for _, param := range requiredParameters {
			if _, err := p.String(param); err != nil {
				log.Warn(r, err)
				sendError(w, r, err)
				return
			}
		}

		if username == "" {
			username, _ = p.String("u")
		}
		client, _ := p.String("c")
		version, _ := p.String("v")

		ctx := r.Context()
		ctx = request.WithUsername(ctx, username)
		ctx = request.WithClient(ctx, client)
		ctx = request.WithVersion(ctx, version)
		log.Debug(ctx, "API: New request "+r.URL.Path, "username", username, "client", client, "version", version)

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// authenticate 校验请求身份。
//
// 两条路径：由内部调用或可信代理确定身份时直接查用户；
// 否则走 Subsonic 自身的凭据校验。
// 无论何种失败原因，对外一律返回统一的认证失败错误，避免泄露账号是否存在。
func authenticate(ds model.DataStore) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()

			var usr *model.User
			var err error

			username, isInternalAuth := fromInternalOrProxyAuth(r)
			if username != "" {
				authType := If(isInternalAuth, "internal", "reverse-proxy")
				usr, err = ds.User(ctx).FindByUsername(username)
				if errors.Is(err, context.Canceled) {
					log.Debug(ctx, "API: Request canceled when authenticating", "auth", authType, "username", username, "remoteAddr", r.RemoteAddr, err)
					return
				}
				if errors.Is(err, model.ErrNotFound) {
					log.Warn(ctx, "API: Invalid login", "auth", authType, "username", username, "remoteAddr", r.RemoteAddr, err)
				} else if err != nil {
					log.Error(ctx, "API: Error authenticating username", "auth", authType, "username", username, "remoteAddr", r.RemoteAddr, err)
				}
			} else {
				p := req.Params(r)
				username, _ := p.String("u")
				pass, _ := p.String("p")
				token, _ := p.String("t")
				salt, _ := p.String("s")
				jwt, _ := p.String("jwt")

				usr, err = ds.User(ctx).FindByUsernameWithPassword(username)
				if errors.Is(err, context.Canceled) {
					log.Debug(ctx, "API: Request canceled when authenticating", "auth", "subsonic", "username", username, "remoteAddr", r.RemoteAddr, err)
					return
				}
				switch {
				case errors.Is(err, model.ErrNotFound):
					log.Warn(ctx, "API: Invalid login", "auth", "subsonic", "username", username, "remoteAddr", r.RemoteAddr, err)
				case err != nil:
					log.Error(ctx, "API: Error authenticating username", "auth", "subsonic", "username", username, "remoteAddr", r.RemoteAddr, err)
				default:
					err = validateCredentials(usr, pass, token, salt, jwt)
					if err != nil {
						log.Warn(ctx, "API: Invalid login", "auth", "subsonic", "username", username, "remoteAddr", r.RemoteAddr, err)
					}
				}
			}

			if err != nil {
				sendError(w, r, newError(responses.ErrorAuthenticationFail))
				return
			}

			ctx = request.WithUser(ctx, *usr)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// validateCredentials 校验 Subsonic 凭据。
//
// 支持三种方式：JWT（Web UI 复用）、明文密码（可为 enc: 前缀的十六进制编码）、
// 以及 token+salt 的 md5 挑战式认证。
// 后两者都要求服务端能拿到明文密码，这是 Subsonic 协议的固有限制。
func validateCredentials(user *model.User, pass, token, salt, jwt string) error {
	valid := false

	switch {
	case jwt != "":
		claims, err := auth.Validate(jwt)
		valid = err == nil && claims["sub"] == user.UserName
	case pass != "":
		if strings.HasPrefix(pass, "enc:") {
			if dec, err := hex.DecodeString(pass[4:]); err == nil {
				pass = string(dec)
			}
		}
		valid = pass == user.Password
	case token != "":
		t := fmt.Sprintf("%x", md5.Sum([]byte(user.Password+salt)))
		valid = t == token
	}

	if !valid {
		return model.ErrInvalidAuth
	}
	return nil
}

// getPlayer 识别并登记发起请求的播放器。
//
// 播放器 ID 存于按用户区分的 Cookie，使同一客户端跨请求保持同一播放器身份，
// 从而能记住其转码偏好。登记失败只记日志，不阻断请求。
func getPlayer(players core.Players) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			userName, _ := request.UsernameFrom(ctx)
			client, _ := request.ClientFrom(ctx)
			playerId := playerIDFromCookie(r, userName)
			ip, _, _ := net.SplitHostPort(r.RemoteAddr)
			userAgent := canonicalUserAgent(r)
			player, trc, err := players.Register(ctx, playerId, client, userAgent, ip)
			if err != nil {
				log.Error(ctx, "Could not register player", "username", userName, "client", client, err)
			} else {
				ctx = request.WithPlayer(ctx, *player)
				if trc != nil {
					ctx = request.WithTranscoding(ctx, *trc)
				}
				r = r.WithContext(ctx)

				cookie := &http.Cookie{
					Name:     playerIDCookieName(userName),
					Value:    player.ID,
					MaxAge:   consts.CookieExpiry,
					HttpOnly: true,
					SameSite: http.SameSiteStrictMode,
					Path:     cmp.Or(conf.Server.BasePath, "/"),
				}
				http.SetCookie(w, cookie)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// canonicalUserAgent 把 User-Agent 归一化为「客户端名/系统」，
// 避免版本号差异导致同一客户端被记成多个播放器。
func canonicalUserAgent(r *http.Request) string {
	u := ua.Parse(r.Header.Get("user-agent"))
	userAgent := u.Name
	if u.OS != "" {
		userAgent = userAgent + "/" + u.OS
	}
	return userAgent
}

// playerIDFromCookie 从 Cookie 中读取播放器 ID。
func playerIDFromCookie(r *http.Request, userName string) string {
	cookieName := playerIDCookieName(userName)
	var playerId string
	if c, err := r.Cookie(cookieName); err == nil {
		playerId = c.Value
		log.Trace(r, "playerId found in cookies", "playerId", playerId)
	}
	return playerId
}

// playerIDCookieName 生成按用户区分的 Cookie 名，避免同一浏览器多账号互相覆盖。
func playerIDCookieName(userName string) string {
	cookieName := fmt.Sprintf("nd-player-%x", userName)
	return cookieName
}

// subsonicErrorPointer 是上下文键，用于让响应写出阶段回传真实的业务状态码。
const subsonicErrorPointer = "subsonicErrorPointer"

// recordStats 记录 Subsonic 请求的耗时与结果。
//
// Subsonic 出错时 HTTP 状态仍是 200，故真实结果需由 sendResponse 通过上下文指针回传；
// 未回传（如 501）时才退回使用 HTTP 状态码。
// 路径去掉 .view 后缀，使同一端点的两种写法归并统计。
func recordStats(metrics metrics.Metrics) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		fn := func(w http.ResponseWriter, r *http.Request) {
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

			status := int32(-1)
			contextWithStatus := context.WithValue(r.Context(), subsonicErrorPointer, &status)

			start := time.Now()
			defer func() {
				elapsed := time.Since(start).Milliseconds()

				// We want to get the client name (even if not present for certain endpoints)
				p := req.Params(r)
				client, _ := p.String("c")

				// If there is no Subsonic status (e.g., HTTP 501 not implemented), fallback to HTTP
				if status == -1 {
					status = int32(ww.Status())
				}

				shortPath := strings.Replace(r.URL.Path, ".view", "", 1)

				metrics.RecordRequest(r.Context(), shortPath, r.Method, client, status, elapsed)
			}()

			next.ServeHTTP(ww, r.WithContext(contextWithStatus))
		}
		return http.HandlerFunc(fn)
	}
}
