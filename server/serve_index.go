package server

import (
	"encoding/json"
	"html/template"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/conf/mime"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/navidrome/navidrome/utils/str"
)

// Index 返回前端首页处理器。
func Index(ds model.DataStore, fs fs.FS) http.HandlerFunc {
	return serveIndex(ds, fs, nil)
}

// IndexWithShare 返回带分享信息的首页处理器，供公开分享页使用。
func IndexWithShare(ds model.DataStore, fs fs.FS, shareInfo *model.Share) http.HandlerFunc {
	return serveIndex(ds, fs, shareInfo)
}

// Injects the config in the `index.html` template
//
// serveIndex 渲染首页，把服务端配置以 JSON 形式注入模板。
//
// 前端需要这些开关来决定渲染哪些功能，通过内联注入可省去一次额外请求。
// 用户数为 0 时置 firstTime，引导前端进入创建管理员流程。
// 用户可自定义的文本（欢迎语、背景图地址）需经 SanitizeText 处理以防 XSS。
func serveIndex(ds model.DataStore, fs fs.FS, shareInfo *model.Share) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := ds.User(r.Context()).CountAll()
		firstTime := c == 0 && err == nil

		t, err := getIndexTemplate(r, fs)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		appConfig := map[string]interface{}{
			"version":                   consts.Version,
			"firstTime":                 firstTime,
			"variousArtistsId":          consts.VariousArtistsID,
			"baseURL":                   str.SanitizeText(strings.TrimSuffix(conf.Server.BasePath, "/")),
			"loginBackgroundURL":        str.SanitizeText(conf.Server.UILoginBackgroundURL),
			"welcomeMessage":            str.SanitizeText(conf.Server.UIWelcomeMessage),
			"maxSidebarPlaylists":       conf.Server.MaxSidebarPlaylists,
			"enableTranscodingConfig":   conf.Server.EnableTranscodingConfig,
			"enableDownloads":           conf.Server.EnableDownloads,
			"enableFavourites":          conf.Server.EnableFavourites,
			"enableStarRating":          conf.Server.EnableStarRating,
			"defaultTheme":              conf.Server.DefaultTheme,
			"defaultLanguage":           conf.Server.DefaultLanguage,
			"defaultUIVolume":           conf.Server.DefaultUIVolume,
			"enableCoverAnimation":      conf.Server.EnableCoverAnimation,
			"enableNowPlaying":          conf.Server.EnableNowPlaying,
			"gaTrackingId":              conf.Server.GATrackingID,
			"losslessFormats":           strings.ToUpper(strings.Join(mime.LosslessFormats, ",")),
			"devActivityPanel":          conf.Server.DevActivityPanel,
			"enableUserEditing":         conf.Server.EnableUserEditing,
			"enableSharing":             conf.Server.EnableSharing,
			"shareURL":                  conf.Server.ShareURL,
			"defaultDownloadableShare":  conf.Server.DefaultDownloadableShare,
			"devSidebarPlaylists":       conf.Server.DevSidebarPlaylists,
			"lastFMEnabled":             conf.Server.LastFM.Enabled,
			"devShowArtistPage":         conf.Server.DevShowArtistPage,
			"devUIShowConfig":           conf.Server.DevUIShowConfig,
			"devNewEventStream":         conf.Server.DevNewEventStream,
			"listenBrainzEnabled":       conf.Server.ListenBrainz.Enabled,
			"enableExternalServices":    conf.Server.EnableExternalServices,
			"enableReplayGain":          conf.Server.EnableReplayGain,
			"defaultDownsamplingFormat": conf.Server.DefaultDownsamplingFormat,
			"separator":                 string(os.PathSeparator),
			"enableInspect":             conf.Server.Inspect.Enabled,
		}
		if strings.HasPrefix(conf.Server.UILoginBackgroundURL, "/") {
			appConfig["loginBackgroundURL"] = path.Join(conf.Server.BasePath, conf.Server.UILoginBackgroundURL)
		}
		auth := handleLoginFromHeaders(ds, r)
		if auth != nil {
			appConfig["auth"] = auth
		}
		appConfigJson, err := json.Marshal(appConfig)
		if err != nil {
			log.Error(r, "Error converting config to JSON", "config", appConfig, err)
		} else {
			log.Trace(r, "Injecting config in index.html", "config", string(appConfigJson))
		}

		log.Debug("UI configuration", "appConfig", appConfig)
		version := consts.Version
		if version != "dev" {
			version = "v" + version
		}
		data := map[string]interface{}{
			"AppConfig": string(appConfigJson),
			"Version":   version,
		}
		addShareData(r, data, shareInfo)

		w.Header().Set("Content-Type", "text/html")
		err = t.Execute(w, data)
		if err != nil {
			log.Error(r, "Could not execute `index.html` template", err)
		}
	}
}

// getIndexTemplate 从内嵌 FS 读取并解析 index.html 模板。
func getIndexTemplate(r *http.Request, fs fs.FS) (*template.Template, error) {
	t := template.New("initial state")
	indexHtml, err := fs.Open("index.html")
	if err != nil {
		log.Error(r, "Could not find `index.html` template", err)
		return nil, err
	}
	indexStr, err := io.ReadAll(indexHtml)
	if err != nil {
		log.Error(r, "Could not read from `index.html`", err)
		return nil, err
	}
	t, err = t.Parse(string(indexStr))
	if err != nil {
		log.Error(r, "Error parsing `index.html`", err)
		return nil, err
	}
	return t, nil
}

// shareData 是注入分享页的数据。
type shareData struct {
	ID           string       `json:"id"`
	Description  string       `json:"description"`
	Downloadable bool         `json:"downloadable"`
	Tracks       []shareTrack `json:"tracks"`
}

// shareTrack 是分享页中的曲目信息，只暴露展示所需的最小字段。
type shareTrack struct {
	ID        string    `json:"id,omitempty"`
	Title     string    `json:"title,omitempty"`
	Artist    string    `json:"artist,omitempty"`
	Album     string    `json:"album,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Duration  float32   `json:"duration,omitempty"`
}

// addShareData 注入分享数据与 Open Graph 所需的描述、链接与封面，
// 使分享链接在社交平台上能生成预览卡片。
// 未填描述时退回用曲目列表内容作为描述。
func addShareData(r *http.Request, data map[string]interface{}, shareInfo *model.Share) {
	ctx := r.Context()
	if shareInfo == nil || shareInfo.ID == "" {
		return
	}
	sd := shareData{
		ID:           shareInfo.ID,
		Description:  shareInfo.Description,
		Downloadable: shareInfo.Downloadable,
	}
	sd.Tracks = slice.Map(shareInfo.Tracks, func(mf model.MediaFile) shareTrack {
		return shareTrack{
			ID:        mf.ID,
			Title:     mf.Title,
			Artist:    mf.Artist,
			Album:     mf.Album,
			Duration:  mf.Duration,
			UpdatedAt: mf.UpdatedAt,
		}
	})

	shareInfoJson, err := json.Marshal(sd)
	if err != nil {
		log.Error(ctx, "Error converting shareInfo to JSON", "config", shareInfo, err)
	} else {
		log.Trace(ctx, "Injecting shareInfo in index.html", "config", string(shareInfoJson))
	}

	if shareInfo.Description != "" {
		data["ShareDescription"] = shareInfo.Description
	} else {
		data["ShareDescription"] = shareInfo.Contents
	}
	data["ShareURL"] = shareInfo.URL
	data["ShareImageURL"] = shareInfo.ImageURL
	data["ShareInfo"] = string(shareInfoJson)
}
