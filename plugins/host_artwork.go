package plugins

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/plugins/host/artwork"
	"github.com/navidrome/navidrome/server/public"
)

// artworkServiceImpl 为插件生成封面图的公开访问链接。
type artworkServiceImpl struct{}

func (a *artworkServiceImpl) GetArtistUrl(_ context.Context, req *artwork.GetArtworkUrlRequest) (*artwork.GetArtworkUrlResponse, error) {
	artID := model.ArtworkID{Kind: model.KindArtistArtwork, ID: req.Id}
	imageURL := public.ImageURL(a.createRequest(), artID, int(req.Size))
	return &artwork.GetArtworkUrlResponse{Url: imageURL}, nil
}

func (a *artworkServiceImpl) GetAlbumUrl(_ context.Context, req *artwork.GetArtworkUrlRequest) (*artwork.GetArtworkUrlResponse, error) {
	artID := model.ArtworkID{Kind: model.KindAlbumArtwork, ID: req.Id}
	imageURL := public.ImageURL(a.createRequest(), artID, int(req.Size))
	return &artwork.GetArtworkUrlResponse{Url: imageURL}, nil
}

func (a *artworkServiceImpl) GetTrackUrl(_ context.Context, req *artwork.GetArtworkUrlRequest) (*artwork.GetArtworkUrlResponse, error) {
	artID := model.ArtworkID{Kind: model.KindMediaFileArtwork, ID: req.Id}
	imageURL := public.ImageURL(a.createRequest(), artID, int(req.Size))
	return &artwork.GetArtworkUrlResponse{Url: imageURL}, nil
}

// createRequest 伪造一个请求对象，只为复用 public.ImageURL 的链接构造逻辑。
// 插件调用不来自真实 HTTP 请求，故用 ShareURL 配置推导主机名，
// 未配置时退回 localhost。
func (a *artworkServiceImpl) createRequest() *http.Request {
	var scheme, host string
	if conf.Server.ShareURL != "" {
		shareURL, _ := url.Parse(conf.Server.ShareURL)
		scheme = shareURL.Scheme
		host = shareURL.Host
	} else {
		scheme = "http"
		host = "localhost"
	}
	r, _ := http.NewRequest("GET", fmt.Sprintf("%s://%s", scheme, host), nil)
	return r
}
