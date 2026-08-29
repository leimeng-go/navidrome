package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils"
	"github.com/unrolled/secure"
)

// requestLogger 记录每个请求的方法、地址、耗时与状态。
// 日志级别随状态码分档：5xx 记 Error、4xx 记 Warn、其余记 Debug，
// 避免正常流量淹没错误信息。Trace 级别才输出完整请求头。
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}

		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		status := ww.Status()

		message := fmt.Sprintf("HTTP: %s %s://%s%s", r.Method, scheme, r.Host, r.RequestURI)
		logArgs := []interface{}{
			r.Context(),
			message,
			"remoteAddr", r.RemoteAddr,
			"elapsedTime", time.Since(start),
			"httpStatus", ww.Status(),
			"responseSize", ww.BytesWritten(),
		}
		if log.IsGreaterOrEqualTo(log.LevelTrace) {
			headers, _ := json.Marshal(r.Header)
			logArgs = append(logArgs, "header", string(headers))
		} else if log.IsGreaterOrEqualTo(log.LevelDebug) {
			logArgs = append(logArgs, "userAgent", r.UserAgent())
		}

		switch {
		case status >= 500:
			log.Error(logArgs...)
		case status >= 400:
			log.Warn(logArgs...)
		default:
			log.Debug(logArgs...)
		}
	})
}

// loggerInjector 把 requestId 注入日志上下文，使同一请求的日志可串联。
func loggerInjector(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		ctx = log.NewContext(r.Context(), "requestId", middleware.GetReqID(ctx))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// robotsTXT 拦截任意路径下的 /robots.txt 请求，统一返回内嵌的那一份。
// BasePath 可能带前缀，故按后缀匹配。
func robotsTXT(fs fs.FS) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/robots.txt") {
				r.URL.Path = "/robots.txt"
				http.FileServerFS(fs).ServeHTTP(w, r)
			} else {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// corsHandler 配置跨域策略。
// 不允许携带凭据，因此可安全放开任意来源；
// 额外暴露几个自定义响应头供前端读取。
func corsHandler() func(http.Handler) http.Handler {
	return cors.Handler(cors.Options{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{
			http.MethodHead,
			http.MethodGet,
			http.MethodPost,
			http.MethodPut,
			http.MethodPatch,
			http.MethodDelete,
		},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: false,
		ExposedHeaders:   []string{"x-content-duration", "x-total-count", "x-nd-authorization"},
	})
}

// secureMiddleware 设置安全响应头：禁止 MIME 嗅探、禁止被嵌入 iframe、限制 Referer 与敏感权限。
func secureMiddleware() func(http.Handler) http.Handler {
	sec := secure.New(secure.Options{
		ContentTypeNosniff:      true,
		FrameDeny:               true,
		ReferrerPolicy:          "same-origin",
		PermissionsPolicy:       "autoplay=(), camera=(), microphone=(), usb=()",
		CustomFrameOptionsValue: conf.Server.HTTPHeaders.FrameOptions,
		//ContentSecurityPolicy: "script-src 'self' 'unsafe-inline'",
	})
	return sec.Handler
}

// compressMiddleware 对文本类响应启用压缩。
func compressMiddleware() func(http.Handler) http.Handler {
	return middleware.Compress(
		5,
		"application/xml",
		"application/json",
		"application/javascript",
		"text/html",
		"text/plain",
		"text/css",
		"text/javascript",
		"text/event-stream",
	)
}

// clientUniqueIDMiddleware is a middleware that sets a unique client ID as a cookie if it's provided in the request header.
// If the unique client ID is not in the header but present as a cookie, it adds the ID to the request context.
//
// clientUniqueIDMiddleware 维护客户端唯一标识。
// 请求头带了就写入 Cookie 持久化，没带就从 Cookie 回读，
// 使同一浏览器在多次会话间保持同一个播放器标识。
func clientUniqueIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		clientUniqueId := r.Header.Get(consts.UIClientUniqueIDHeader)

		// If clientUniqueId is found in the header, set it as a cookie
		if clientUniqueId != "" {
			c := &http.Cookie{
				Name:     consts.UIClientUniqueIDHeader,
				Value:    clientUniqueId,
				MaxAge:   consts.CookieExpiry,
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteStrictMode,
				Path:     cmp.Or(conf.Server.BasePath, "/"),
			}
			http.SetCookie(w, c)
		} else {
			// If clientUniqueId is not found in the header, check if it's present as a cookie
			c, err := r.Cookie(consts.UIClientUniqueIDHeader)
			if !errors.Is(err, http.ErrNoCookie) {
				clientUniqueId = c.Value
			}
		}

		// If a valid clientUniqueId is found, add it to the request context
		if clientUniqueId != "" {
			ctx = request.WithClientUniqueId(ctx, clientUniqueId)
			r = r.WithContext(ctx)
		}

		// Call the next middleware or handler in the chain
		next.ServeHTTP(w, r)
	})
}

// realIPMiddleware applies middleware.RealIP, and additionally saves the request's original RemoteAddr to the request's
// context if navidrome is behind a trusted reverse proxy.
//
// realIPMiddleware 解析真实客户端 IP。
// 配置了可信代理时，需在 RealIP 改写之前先把原始 RemoteAddr 存入上下文——
// 那才是代理自身的 IP，用于校验其是否可信。
func realIPMiddleware(next http.Handler) http.Handler {
	if conf.Server.ExtAuth.TrustedSources != "" {
		return chi.Chain(
			reqToCtx(request.ReverseProxyIp, func(r *http.Request) any { return r.RemoteAddr }),
			middleware.RealIP,
		).Handler(next)
	}

	// The middleware is applied without a trusted reverse proxy to support other use-cases such as multiple clients
	// behind a caching proxy. In this case, navidrome only uses the request's RemoteAddr for logging, so the security
	// impact of reading the headers from untrusted sources is limited.
	return middleware.RealIP(next)
}

// reqToCtx creates a middleware that updates the request's context with a value computed from the request. A given key
// can only be set once.
//
// reqToCtx 把从请求算出的值写入上下文。同一 key 只写一次，防止被后续中间件覆盖。
func reqToCtx(key any, fn func(req *http.Request) any) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Context().Value(key) == nil {
				ctx := context.WithValue(r.Context(), key, fn(r))
				r = r.WithContext(ctx)
			}

			next.ServeHTTP(w, r)
		})
	}
}

// serverAddressMiddleware is a middleware function that modifies the request object
// to reflect the address of the server handling the request, as determined by the
// presence of X-Forwarded-* headers or the scheme and host of the request URL.
//
// serverAddressMiddleware 依据 X-Forwarded-* 头还原对外可见的 scheme 与 host，
// 使生成的链接（如分享地址）在反向代理后仍然正确。
func serverAddressMiddleware(h http.Handler) http.Handler {
	// Define a new handler function that will be returned by this middleware function.
	fn := func(w http.ResponseWriter, r *http.Request) {
		// Call the serverAddress function to get the scheme and host of the server
		// handling the request. If a host is found, modify the request object to use
		// that host and scheme instead of the original ones.
		if rScheme, rHost := serverAddress(r); rHost != "" {
			r.Host = rHost
			r.URL.Scheme = rScheme
		}

		// Call the next handler in the chain with the modified request and response.
		h.ServeHTTP(w, r)
	}

	// Return the new handler function as a http.Handler object.
	return http.HandlerFunc(fn)
}

// Define constants for the X-Forwarded-* header keys.
var (
	xForwardedHost   = http.CanonicalHeaderKey("X-Forwarded-Host")
	xForwardedProto  = http.CanonicalHeaderKey("X-Forwarded-Proto")
	xForwardedScheme = http.CanonicalHeaderKey("X-Forwarded-Scheme")
)

// serverAddress is a helper function that returns the scheme and host of the server
// handling the given request, as determined by the presence of X-Forwarded-* headers
// or the scheme and host of the request URL.
//
// serverAddress 计算对外的 scheme 与 host。
// X-Forwarded-Host 在多级代理下可能是逗号分隔的列表，取第一个（最靠近客户端的那个）。
func serverAddress(r *http.Request) (scheme, host string) {
	// Save the original request host for later comparison.
	origHost := r.Host

	// Determine the protocol of the request based on the presence of a TLS connection.
	protocol := "http"
	if r.TLS != nil {
		protocol = "https"
	}

	// Get the X-Forwarded-Host header and extract the first host name if there are
	// multiple hosts listed. If there is no X-Forwarded-Host header, use the original
	// request host as the default.
	xfh := r.Header.Get(xForwardedHost)
	if xfh != "" {
		i := strings.Index(xfh, ",")
		if i == -1 {
			i = len(xfh)
		}
		xfh = xfh[:i]
	}
	host = cmp.Or(xfh, r.Host)

	// Determine the protocol and scheme of the request based on the presence of
	// X-Forwarded-* headers or the scheme of the request URL.
	scheme = cmp.Or(
		r.Header.Get(xForwardedProto),
		r.Header.Get(xForwardedScheme),
		r.URL.Scheme,
		protocol,
	)

	// If the request host has changed due to the X-Forwarded-Host header, log a trace
	// message with the original and new host values, as well as the scheme and URL.
	if host != origHost {
		log.Trace(r.Context(), "Request host has changed", "origHost", origHost, "host", host, "scheme", scheme, "url", r.URL)
	}

	// Return the scheme and host of the server handling the request.
	return scheme, host
}

// URLParamsMiddleware is a middleware function that decodes the query string of
// the incoming HTTP request, adds the URL parameters from the routing context,
// and re-encodes the modified query string.
//
// URLParamsMiddleware 把 chi 的路径参数以 ":name" 形式并入查询串，
// 让下游处理器可以统一从查询参数读取，无需区分参数来自路径还是查询串。
// 通配符参数跳过。
func URLParamsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Retrieve the routing context from the request context.
		ctx := chi.RouteContext(r.Context())

		// Parse the existing query string into a URL values map.
		params, _ := url.ParseQuery(r.URL.RawQuery)

		// Loop through each URL parameter in the routing context.
		for i, key := range ctx.URLParams.Keys {
			// Skip any wildcard URL parameter keys.
			if strings.Contains(key, "*") {
				continue
			}

			// Add the URL parameter key-value pair to the URL values map.
			params.Add(":"+key, ctx.URLParams.Values[i])
		}

		// Re-encode the URL values map as a query string and replace the
		// existing query string in the request.
		r.URL.RawQuery = params.Encode()

		// Call the next handler in the chain with the modified request and response.
		next.ServeHTTP(w, r)
	})
}

// UpdateLastAccessMiddleware 更新用户的最近访问时间。
//
// 用限流器按用户降频，避免每个请求都写库。
// 写操作限时 1 秒且失败只告警：该字段并非关键数据，不应拖慢或中断请求。
func UpdateLastAccessMiddleware(ds model.DataStore) func(next http.Handler) http.Handler {
	userAccessLimiter := utils.Limiter{Interval: consts.UpdateLastAccessFrequency}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			usr, ok := request.UserFrom(ctx)
			if ok {
				userAccessLimiter.Do(usr.ID, func() {
					start := time.Now()
					ctx, cancel := context.WithTimeout(ctx, time.Second)
					defer cancel()

					err := ds.User(ctx).UpdateLastAccessAt(usr.ID)
					if err != nil {
						log.Warn(ctx, "Could not update user's lastAccessAt", "username", usr.UserName,
							"elapsed", time.Since(start), err)
					} else {
						log.Trace(ctx, "Update user's lastAccessAt", "username", usr.UserName,
							"elapsed", time.Since(start))
					}
				})
			}
			next.ServeHTTP(w, r)
		})
	}
}
