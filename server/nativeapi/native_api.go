package nativeapi

import (
	"context"
	"encoding/json"
	"html"
	"net/http"
	"strconv"
	"time"

	"github.com/deluan/rest"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server"
)

// Router 提供 Navidrome 自有的 Native API，供 Web UI 使用。
type Router struct {
	http.Handler
	ds          model.DataStore
	share       core.Share
	playlists   core.Playlists
	insights    metrics.Insights
	libs        core.Library
	maintenance core.Maintenance
}

// New 创建 Native API 路由。
func New(ds model.DataStore, share core.Share, playlists core.Playlists, insights metrics.Insights, libraryService core.Library, maintenance core.Maintenance) *Router {
	r := &Router{ds: ds, share: share, playlists: playlists, insights: insights, libs: libraryService, maintenance: maintenance}
	r.Handler = r.routes()
	return r
}

// routes 注册所有 Native API 路由。
//
// 翻译资源是公开的：登录页也需要它，故不能要求认证。
// 其余端点均需登录；配置、诊断、库管理等再叠加管理员校验。
// 部分资源只读（persistable=false），如歌曲、专辑——它们由扫描器维护，不允许直接改写。
func (api *Router) routes() http.Handler {
	r := chi.NewRouter()

	// Public
	api.RX(r, "/translation", newTranslationRepository, false)

	// Protected
	r.Group(func(r chi.Router) {
		r.Use(server.Authenticator(api.ds))
		r.Use(server.JWTRefresher)
		r.Use(server.UpdateLastAccessMiddleware(api.ds))
		api.R(r, "/user", model.User{}, true)
		api.R(r, "/song", model.MediaFile{}, false)
		api.R(r, "/album", model.Album{}, false)
		api.R(r, "/artist", model.Artist{}, false)
		api.R(r, "/genre", model.Genre{}, false)
		api.R(r, "/player", model.Player{}, true)
		api.R(r, "/transcoding", model.Transcoding{}, conf.Server.EnableTranscodingConfig)
		api.R(r, "/radio", model.Radio{}, true)
		api.R(r, "/tag", model.Tag{}, true)
		if conf.Server.EnableSharing {
			api.RX(r, "/share", api.share.NewRepository, true)
		}

		api.addPlaylistRoute(r)
		api.addPlaylistTrackRoute(r)
		api.addSongPlaylistsRoute(r)
		api.addQueueRoute(r)
		api.addMissingFilesRoute(r)
		api.addKeepAliveRoute(r)
		api.addInsightsRoute(r)

		r.With(adminOnlyMiddleware).Group(func(r chi.Router) {
			api.addInspectRoute(r)
			api.addConfigRoute(r)
			api.addUserLibraryRoute(r)
			api.RX(r, "/library", api.libs.NewRepository, true)
		})
	})

	return r
}

// R 为一个模型注册标准的 REST 路由。
func (api *Router) R(r chi.Router, pathPrefix string, model interface{}, persistable bool) {
	constructor := func(ctx context.Context) rest.Repository {
		return api.ds.Resource(ctx, model)
	}
	api.RX(r, pathPrefix, constructor, persistable)
}

// RX 用自定义仓库构造器注册 REST 路由。
// persistable 为 false 时只暴露读接口。
func (api *Router) RX(r chi.Router, pathPrefix string, constructor rest.RepositoryConstructor, persistable bool) {
	r.Route(pathPrefix, func(r chi.Router) {
		r.Get("/", rest.GetAll(constructor))
		if persistable {
			r.Post("/", rest.Post(constructor))
		}
		r.Route("/{id}", func(r chi.Router) {
			r.Use(server.URLParamsMiddleware)
			r.Get("/", rest.Get(constructor))
			if persistable {
				r.Put("/", rest.Put(constructor))
				r.Delete("/", rest.Delete(constructor))
			}
		})
	})
}

// addPlaylistRoute 注册歌单路由。
// POST 依 Content-Type 分流：JSON 走常规创建，其余按 M3U 文件导入处理。
func (api *Router) addPlaylistRoute(r chi.Router) {
	constructor := func(ctx context.Context) rest.Repository {
		return api.ds.Resource(ctx, model.Playlist{})
	}

	r.Route("/playlist", func(r chi.Router) {
		r.Get("/", rest.GetAll(constructor))
		r.Post("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Content-type") == "application/json" {
				rest.Post(constructor)(w, r)
				return
			}
			createPlaylistFromM3U(api.playlists)(w, r)
		})

		r.Route("/{id}", func(r chi.Router) {
			r.Use(server.URLParamsMiddleware)
			r.Get("/", rest.Get(constructor))
			r.Put("/", rest.Put(constructor))
			r.Delete("/", rest.Delete(constructor))
		})
	})
}

// addPlaylistTrackRoute 注册歌单曲目的增删改查与排序路由。
func (api *Router) addPlaylistTrackRoute(r chi.Router) {
	r.Route("/playlist/{playlistId}/tracks", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			getPlaylist(api.ds)(w, r)
		})
		r.With(server.URLParamsMiddleware).Route("/", func(r chi.Router) {
			r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
				deleteFromPlaylist(api.ds)(w, r)
			})
			r.Post("/", func(w http.ResponseWriter, r *http.Request) {
				addToPlaylist(api.ds)(w, r)
			})
		})
		r.Route("/{id}", func(r chi.Router) {
			r.Use(server.URLParamsMiddleware)
			r.Get("/", func(w http.ResponseWriter, r *http.Request) {
				getPlaylistTrack(api.ds)(w, r)
			})
			r.Put("/", func(w http.ResponseWriter, r *http.Request) {
				reorderItem(api.ds)(w, r)
			})
			r.Delete("/", func(w http.ResponseWriter, r *http.Request) {
				deleteFromPlaylist(api.ds)(w, r)
			})
		})
	})
}

// addSongPlaylistsRoute 注册「查询某首歌属于哪些歌单」的路由。
func (api *Router) addSongPlaylistsRoute(r chi.Router) {
	r.With(server.URLParamsMiddleware).Get("/song/{id}/playlists", func(w http.ResponseWriter, r *http.Request) {
		getSongPlaylists(api.ds)(w, r)
	})
}

// addQueueRoute 注册播放队列路由，用于在多设备间同步播放进度。
func (api *Router) addQueueRoute(r chi.Router) {
	r.Route("/queue", func(r chi.Router) {
		r.Get("/", getQueue(api.ds))
		r.Post("/", saveQueue(api.ds))
		r.Put("/", updateQueue(api.ds))
		r.Delete("/", clearQueue(api.ds))
	})
}

// addMissingFilesRoute 注册缺失文件的查询与清理路由。
func (api *Router) addMissingFilesRoute(r chi.Router) {
	r.Route("/missing", func(r chi.Router) {
		api.RX(r, "/", newMissingRepository(api.ds), false)
		r.Delete("/", deleteMissingFiles(api.maintenance))
	})
}

// writeDeleteManyResponse 输出批量删除的结果。
// 单条时返回 {"id":...}，多条时返回 {"ids":[...]}，以适配前端框架对两种场景的不同预期。
func writeDeleteManyResponse(w http.ResponseWriter, r *http.Request, ids []string) {
	var resp []byte
	var err error
	if len(ids) == 1 {
		resp = []byte(`{"id":"` + html.EscapeString(ids[0]) + `"}`)
	} else {
		resp, err = json.Marshal(&struct {
			Ids []string `json:"ids"`
		}{Ids: ids})
		if err != nil {
			log.Error(r.Context(), "Error marshaling response", "ids", ids, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
	_, err = w.Write(resp)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// addInspectRoute 注册文件诊断路由。
// 该操作需读取并解析原始文件，开销较大，故支持限流配置。
func (api *Router) addInspectRoute(r chi.Router) {
	if conf.Server.Inspect.Enabled {
		r.Group(func(r chi.Router) {
			if conf.Server.Inspect.MaxRequests > 0 {
				log.Debug("Throttling inspect", "maxRequests", conf.Server.Inspect.MaxRequests,
					"backlogLimit", conf.Server.Inspect.BacklogLimit, "backlogTimeout",
					conf.Server.Inspect.BacklogTimeout)
				r.Use(middleware.ThrottleBacklog(conf.Server.Inspect.MaxRequests, conf.Server.Inspect.BacklogLimit, time.Duration(conf.Server.Inspect.BacklogTimeout)))
			}
			r.Get("/inspect", inspect(api.ds))
		})
	}
}

// addConfigRoute 注册配置查看路由，仅在开发模式下开放。
func (api *Router) addConfigRoute(r chi.Router) {
	if conf.Server.DevUIShowConfig {
		r.Get("/config/*", getConfig)
	}
}

// addKeepAliveRoute 提供保活端点，供前端维持会话不过期。
func (api *Router) addKeepAliveRoute(r chi.Router) {
	r.Get("/keepalive/*", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"response":"ok", "id":"keepalive"}`))
	})
}

// addInsightsRoute 返回统计上报的最近执行状态，供设置页展示。
func (api *Router) addInsightsRoute(r chi.Router) {
	r.Get("/insights/*", func(w http.ResponseWriter, r *http.Request) {
		last, success := api.insights.LastRun(r.Context())
		if conf.Server.EnableInsightsCollector {
			_, _ = w.Write([]byte(`{"id":"insights_status", "lastRun":"` + last.Format("2006-01-02 15:04:05") + `", "success":` + strconv.FormatBool(success) + `}`))
		} else {
			_, _ = w.Write([]byte(`{"id":"insights_status", "lastRun":"disabled", "success":false}`))
		}
	})
}

// Middleware to ensure only admin users can access endpoints
// adminOnlyMiddleware 限制仅管理员可访问。
func adminOnlyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := request.UserFrom(r.Context())
		if !ok || !user.IsAdmin {
			http.Error(w, "Access denied: admin privileges required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
