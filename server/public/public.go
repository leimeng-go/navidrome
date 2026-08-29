package public

import (
	"net/http"
	"net/url"
	"path"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/ui"
)

// Router 提供无需登录即可访问的公开端点：分享页、公开图片与流媒体。
type Router struct {
	http.Handler
	artwork       artwork.Artwork
	streamer      core.MediaStreamer
	archiver      core.Archiver
	share         core.Share
	assetsHandler http.Handler
	ds            model.DataStore
}

// New 创建公开路由。
func New(ds model.DataStore, artwork artwork.Artwork, streamer core.MediaStreamer, share core.Share, archiver core.Archiver) *Router {
	p := &Router{ds: ds, artwork: artwork, streamer: streamer, share: share, archiver: archiver}
	shareRoot := path.Join(conf.Server.BasePath, consts.URLPathPublic)
	p.assetsHandler = http.StripPrefix(shareRoot, http.FileServer(http.FS(ui.BuildAssets())))
	p.Handler = p.routes()

	return p
}

// routes 注册公开路由。
//
// 图片端点可选限流：公开访问不受认证保护，需防止被刷爆导致封面生成压垮服务。
// 分享相关端点整体受 EnableSharing 开关控制，下载再叠一层 EnableDownloads。
func (pub *Router) routes() http.Handler {
	r := chi.NewRouter()

	r.Group(func(r chi.Router) {
		r.Use(server.URLParamsMiddleware)
		r.Group(func(r chi.Router) {
			if conf.Server.DevArtworkMaxRequests > 0 {
				log.Debug("Throttling public images endpoint", "maxRequests", conf.Server.DevArtworkMaxRequests,
					"backlogLimit", conf.Server.DevArtworkThrottleBacklogLimit, "backlogTimeout",
					conf.Server.DevArtworkThrottleBacklogTimeout)
				r.Use(middleware.ThrottleBacklog(conf.Server.DevArtworkMaxRequests, conf.Server.DevArtworkThrottleBacklogLimit,
					conf.Server.DevArtworkThrottleBacklogTimeout))
			}
			r.HandleFunc("/img/{id}", pub.handleImages)
		})
		if conf.Server.EnableSharing {
			r.HandleFunc("/s/{id}", pub.handleStream)
			if conf.Server.EnableDownloads {
				r.HandleFunc("/d/{id}", pub.handleDownloads)
			}
			r.HandleFunc("/{id}/m3u", pub.handleM3U)
			r.HandleFunc("/{id}", pub.handleShares)
			r.HandleFunc("/", pub.handleShares)
			r.Handle("/*", pub.assetsHandler)
		}
	})
	return r
}

// ShareURL 生成分享链接。
func ShareURL(r *http.Request, id string) string {
	uri := path.Join(consts.URLPathPublic, id)
	return publicURL(r, uri, nil)
}

// publicURL 生成对外可访问的链接。
// 配置了 ShareURL 时用它的协议与主机名——分享链接常需走独立的公网域名，
// 与内网访问地址不同。
func publicURL(r *http.Request, u string, params url.Values) string {
	if conf.Server.ShareURL != "" {
		shareUrl, _ := url.Parse(conf.Server.ShareURL)
		buildUrl, _ := url.Parse(u)
		buildUrl.Scheme = shareUrl.Scheme
		buildUrl.Host = shareUrl.Host
		if len(params) > 0 {
			buildUrl.RawQuery = params.Encode()
		}
		return buildUrl.String()
	}
	return server.AbsoluteURL(r, u, params)
}
