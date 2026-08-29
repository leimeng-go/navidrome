package subsonic

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/core/external"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/core/playback"
	"github.com/navidrome/navidrome/core/scrobbler"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
)

// Version 是本实现声称兼容的 Subsonic API 版本。
const Version = "1.16.1"

// handler 是常规端点处理器，返回结构化响应由框架统一序列化。
type handler = func(*http.Request) (*responses.Subsonic, error)

// handlerRaw 用于需要直接写响应体的端点（如流媒体、封面图）。
type handlerRaw = func(http.ResponseWriter, *http.Request) (*responses.Subsonic, error)

// Router 实现 Subsonic 兼容 API，供第三方客户端接入。
type Router struct {
	http.Handler
	ds        model.DataStore
	artwork   artwork.Artwork
	streamer  core.MediaStreamer
	archiver  core.Archiver
	players   core.Players
	provider  external.Provider
	playlists core.Playlists
	scanner   model.Scanner
	broker    events.Broker
	scrobbler scrobbler.PlayTracker
	share     core.Share
	playback  playback.PlaybackServer
	metrics   metrics.Metrics
}

// New 创建 Subsonic API 路由。
func New(ds model.DataStore, artwork artwork.Artwork, streamer core.MediaStreamer, archiver core.Archiver,
	players core.Players, provider external.Provider, scanner model.Scanner, broker events.Broker,
	playlists core.Playlists, scrobbler scrobbler.PlayTracker, share core.Share, playback playback.PlaybackServer,
	metrics metrics.Metrics,
) *Router {
	r := &Router{
		ds:        ds,
		artwork:   artwork,
		streamer:  streamer,
		archiver:  archiver,
		players:   players,
		provider:  provider,
		playlists: playlists,
		scanner:   scanner,
		broker:    broker,
		scrobbler: scrobbler,
		share:     share,
		playback:  playback,
		metrics:   metrics,
	}
	r.Handler = r.routes()
	return r
}

// routes 注册全部 Subsonic 端点。
//
// postFormToQueryParams 需最先执行：Subsonic 允许参数以表单方式提交，
// 统一转成查询参数后，后续处理无需区分来源。
// getOpenSubsonicExtensions 是公开端点——客户端需在认证前探测服务端能力。
// 各端点按功能分组并挂上 getPlayer，以便按客户端记录播放器与转码配置。
// 未实现的端点返回 501，明确不打算实现的返回 410，避免客户端反复重试。
func (api *Router) routes() http.Handler {
	r := chi.NewRouter()

	if conf.Server.Prometheus.Enabled {
		r.Use(recordStats(api.metrics))
	}

	r.Use(postFormToQueryParams)

	// Public
	h(r, "getOpenSubsonicExtensions", api.GetOpenSubsonicExtensions)

	// Protected
	r.Group(func(r chi.Router) {
		r.Use(checkRequiredParameters)
		r.Use(authenticate(api.ds))
		r.Use(server.UpdateLastAccessMiddleware(api.ds))

		// Subsonic endpoints, grouped by controller
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "ping", api.Ping)
			h(r, "getLicense", api.GetLicense)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "getMusicFolders", api.GetMusicFolders)
			h(r, "getIndexes", api.GetIndexes)
			h(r, "getArtists", api.GetArtists)
			h(r, "getGenres", api.GetGenres)
			h(r, "getMusicDirectory", api.GetMusicDirectory)
			h(r, "getArtist", api.GetArtist)
			h(r, "getAlbum", api.GetAlbum)
			h(r, "getSong", api.GetSong)
			h(r, "getAlbumInfo", api.GetAlbumInfo)
			h(r, "getAlbumInfo2", api.GetAlbumInfo)
			h(r, "getArtistInfo", api.GetArtistInfo)
			h(r, "getArtistInfo2", api.GetArtistInfo2)
			h(r, "getTopSongs", api.GetTopSongs)
			h(r, "getSimilarSongs", api.GetSimilarSongs)
			h(r, "getSimilarSongs2", api.GetSimilarSongs2)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			hr(r, "getAlbumList", api.GetAlbumList)
			hr(r, "getAlbumList2", api.GetAlbumList2)
			h(r, "getStarred", api.GetStarred)
			h(r, "getStarred2", api.GetStarred2)
			h(r, "getNowPlaying", api.GetNowPlaying)
			h(r, "getRandomSongs", api.GetRandomSongs)
			h(r, "getSongsByGenre", api.GetSongsByGenre)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "setRating", api.SetRating)
			h(r, "star", api.Star)
			h(r, "unstar", api.Unstar)
			h(r, "scrobble", api.Scrobble)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "getPlaylists", api.GetPlaylists)
			h(r, "getPlaylist", api.GetPlaylist)
			h(r, "createPlaylist", api.CreatePlaylist)
			h(r, "deletePlaylist", api.DeletePlaylist)
			h(r, "updatePlaylist", api.UpdatePlaylist)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "getBookmarks", api.GetBookmarks)
			h(r, "createBookmark", api.CreateBookmark)
			h(r, "deleteBookmark", api.DeleteBookmark)
			h(r, "getPlayQueue", api.GetPlayQueue)
			h(r, "getPlayQueueByIndex", api.GetPlayQueueByIndex)
			h(r, "savePlayQueue", api.SavePlayQueue)
			h(r, "savePlayQueueByIndex", api.SavePlayQueueByIndex)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "search2", api.Search2)
			h(r, "search3", api.Search3)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "getUser", api.GetUser)
			h(r, "getUsers", api.GetUsers)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "getScanStatus", api.GetScanStatus)
			h(r, "startScan", api.StartScan)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			hr(r, "getAvatar", api.GetAvatar)
			h(r, "getLyrics", api.GetLyrics)
			h(r, "getLyricsBySongId", api.GetLyricsBySongId)
			hr(r, "stream", api.Stream)
			hr(r, "download", api.Download)
		})
		r.Group(func(r chi.Router) {
			// configure request throttling
			if conf.Server.DevArtworkMaxRequests > 0 {
				log.Debug("Throttling Subsonic getCoverArt endpoint", "maxRequests", conf.Server.DevArtworkMaxRequests,
					"backlogLimit", conf.Server.DevArtworkThrottleBacklogLimit, "backlogTimeout",
					conf.Server.DevArtworkThrottleBacklogTimeout)
				r.Use(middleware.ThrottleBacklog(conf.Server.DevArtworkMaxRequests, conf.Server.DevArtworkThrottleBacklogLimit,
					conf.Server.DevArtworkThrottleBacklogTimeout))
			}
			hr(r, "getCoverArt", api.GetCoverArt)
		})
		r.Group(func(r chi.Router) {
			r.Use(getPlayer(api.players))
			h(r, "createInternetRadioStation", api.CreateInternetRadio)
			h(r, "deleteInternetRadioStation", api.DeleteInternetRadio)
			h(r, "getInternetRadioStations", api.GetInternetRadios)
			h(r, "updateInternetRadioStation", api.UpdateInternetRadio)
		})
		if conf.Server.EnableSharing {
			r.Group(func(r chi.Router) {
				r.Use(getPlayer(api.players))
				h(r, "getShares", api.GetShares)
				h(r, "createShare", api.CreateShare)
				h(r, "updateShare", api.UpdateShare)
				h(r, "deleteShare", api.DeleteShare)
			})
		} else {
			h501(r, "getShares", "createShare", "updateShare", "deleteShare")
		}

		if conf.Server.Jukebox.Enabled {
			r.Group(func(r chi.Router) {
				r.Use(getPlayer(api.players))
				h(r, "jukeboxControl", api.JukeboxControl)
			})
		} else {
			h501(r, "jukeboxControl")
		}

		// Not Implemented (yet?)
		h501(r, "getPodcasts", "getNewestPodcasts", "refreshPodcasts", "createPodcastChannel", "deletePodcastChannel",
			"deletePodcastEpisode", "downloadPodcastEpisode")
		h501(r, "createUser", "updateUser", "deleteUser", "changePassword")

		// Deprecated/Won't implement/Out of scope endpoints
		h410(r, "search")
		h410(r, "getChatMessages", "addChatMessage")
		h410(r, "getVideos", "getVideoInfo", "getCaptions", "hls")
	})
	return r
}

// Add a Subsonic handler
// h 注册一个常规端点。
func h(r chi.Router, path string, f handler) {
	hr(r, path, func(_ http.ResponseWriter, r *http.Request) (*responses.Subsonic, error) {
		return f(r)
	})
}

// Add a Subsonic handler that requires an http.ResponseWriter (ex: stream, getCoverArt...)
// hr 注册需直接操作响应体的端点。
// 请求中途被取消时不再写响应，避免向已断开的连接写数据。
func hr(r chi.Router, path string, f handlerRaw) {
	handle := func(w http.ResponseWriter, r *http.Request) {
		res, err := f(w, r)
		if err != nil {
			sendError(w, r, err)
			return
		}
		if r.Context().Err() != nil {
			if log.IsGreaterOrEqualTo(log.LevelDebug) {
				log.Warn(r.Context(), "Request was interrupted", "endpoint", r.URL.Path, r.Context().Err())
			}
			return
		}
		if res != nil {
			sendResponse(w, r, res)
		}
	}
	addHandler(r, path, handle)
}

// Add a handler that returns 501 - Not implemented. Used to signal that an endpoint is not implemented yet
// h501 注册返回「尚未实现」的端点。
func h501(r chi.Router, paths ...string) {
	for _, path := range paths {
		handle := func(w http.ResponseWriter, r *http.Request) {
			w.Header().Add("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte("This endpoint is not implemented, but may be in future releases"))
		}
		addHandler(r, path, handle)
	}
}

// Add a handler that returns 410 - Gone. Used to signal that an endpoint will not be implemented
// h410 注册返回「不会实现」的端点。
func h410(r chi.Router, paths ...string) {
	for _, path := range paths {
		handle := func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusGone)
			_, _ = w.Write([]byte("This endpoint will not be implemented"))
		}
		addHandler(r, path, handle)
	}
}

// addHandler 同时注册带与不带 .view 后缀的路径，
// 老客户端习惯请求 xxx.view，需一并兼容。
func addHandler(r chi.Router, path string, handle func(w http.ResponseWriter, r *http.Request)) {
	r.HandleFunc("/"+path, handle)
	r.HandleFunc("/"+path+".view", handle)
}

// mapToSubsonicError 把内部错误映射为 Subsonic 错误码。
// 未识别的错误统一归为通用错误，避免把内部实现细节透给客户端。
func mapToSubsonicError(err error) subError {
	switch {
	case errors.Is(err, errSubsonic): // do nothing
	case errors.Is(err, req.ErrMissingParam):
		err = newError(responses.ErrorMissingParameter, err.Error())
	case errors.Is(err, req.ErrInvalidParam):
		err = newError(responses.ErrorGeneric, err.Error())
	case errors.Is(err, model.ErrNotFound):
		err = newError(responses.ErrorDataNotFound, "data not found")
	default:
		err = newError(responses.ErrorGeneric, fmt.Sprintf("Internal Server Error: %s", err))
	}
	var subErr subError
	errors.As(err, &subErr)
	return subErr
}

// sendError 以 Subsonic 约定的格式返回错误。
// 注意仍返回 HTTP 200，错误信息在响应体内——这是协议要求。
func sendError(w http.ResponseWriter, r *http.Request, err error) {
	subErr := mapToSubsonicError(err)
	response := newResponse()
	response.Status = responses.StatusFailed
	response.Error = &responses.Error{Code: subErr.code, Message: subErr.Error()}

	sendResponse(w, r, response)
}

// sendResponse 按客户端请求的格式（xml/json/jsonp）序列化并输出响应。
//
// 结果状态码会回写到上下文中的指针，供指标中间件统计——
// 因为 HTTP 状态恒为 200，只能从响应体里取真实结果。
func sendResponse(w http.ResponseWriter, r *http.Request, payload *responses.Subsonic) {
	p := req.Params(r)
	f, _ := p.String("f")
	var response []byte
	var err error
	switch f {
	case "json":
		w.Header().Set("Content-Type", "application/json")
		wrapper := &responses.JsonWrapper{Subsonic: *payload}
		response, err = json.Marshal(wrapper)
	case "jsonp":
		w.Header().Set("Content-Type", "application/javascript")
		callback, _ := p.String("callback")
		wrapper := &responses.JsonWrapper{Subsonic: *payload}
		response, err = json.Marshal(wrapper)
		response = []byte(fmt.Sprintf("%s(%s)", callback, response))
	default:
		w.Header().Set("Content-Type", "application/xml")
		response, err = xml.Marshal(payload)
	}
	// This should never happen, but if it does, we need to know
	if err != nil {
		log.Error(r.Context(), "Error marshalling response", "format", f, err)
		sendError(w, r, err)
		return
	}

	if payload.Status == responses.StatusOK {
		if log.IsGreaterOrEqualTo(log.LevelTrace) {
			log.Debug(r.Context(), "API: Successful response", "endpoint", r.URL.Path, "status", "OK", "body", string(response))
		} else {
			log.Debug(r.Context(), "API: Successful response", "endpoint", r.URL.Path, "status", "OK")
		}
	} else {
		log.Warn(r.Context(), "API: Failed response", "endpoint", r.URL.Path, "error", payload.Error.Code, "message", payload.Error.Message)
	}

	statusPointer, ok := r.Context().Value(subsonicErrorPointer).(*int32)

	if ok && statusPointer != nil {
		if payload.Status == responses.StatusOK {
			*statusPointer = 0
		} else {
			*statusPointer = payload.Error.Code
		}
	}

	if _, err := w.Write(response); err != nil {
		log.Error(r, "Error sending response to client", "endpoint", r.URL.Path, "payload", string(response), err)
	}
}
