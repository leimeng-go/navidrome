package spotify

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/cache"
	"github.com/xrash/smetrics"
)

const spotifyAgentName = "spotify"

// spotifyAgent 是 Spotify 元数据代理，目前仅用于获取艺人图片。
type spotifyAgent struct {
	ds     model.DataStore
	id     string
	secret string
	client *client
}

// spotifyConstructor 构造代理。未配置 ID/Secret 时返回 nil 表示不可用。
func spotifyConstructor(ds model.DataStore) agents.Interface {
	if conf.Server.Spotify.ID == "" || conf.Server.Spotify.Secret == "" {
		return nil
	}
	l := &spotifyAgent{
		ds:     ds,
		id:     conf.Server.Spotify.ID,
		secret: conf.Server.Spotify.Secret,
	}
	hc := &http.Client{
		Timeout: consts.DefaultHttpClientTimeOut,
	}
	chc := cache.NewHTTPClient(hc, consts.DefaultHttpClientTimeOut)
	l.client = newClient(l.id, l.secret, chc)
	return l
}

// AgentName 返回代理名。
func (s *spotifyAgent) AgentName() string {
	return spotifyAgentName
}

// GetArtistImages 返回艺人图片，尺寸取图片宽度。
func (s *spotifyAgent) GetArtistImages(ctx context.Context, id, name, mbid string) ([]agents.ExternalImage, error) {
	a, err := s.searchArtist(ctx, name)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			log.Warn(ctx, "Artist not found in Spotify", "artist", name)
		} else {
			log.Error(ctx, "Error calling Spotify", "artist", name, err)
		}
		return nil, err
	}

	var res []agents.ExternalImage
	for _, img := range a.Images {
		res = append(res, agents.ExternalImage{
			URL:  img.URL,
			Size: img.Width,
		})
	}
	return res, nil
}

// searchArtist 按名称搜索艺人并挑出最佳匹配。
//
// Spotify 的搜索排序未必符合需要，故本地重排：
// 依次按「有无图片」「名称编辑距离」「热度」三级排序，
// 用定长格式化字符串拼接实现多字段比较。
// 最终仍要求首位结果名称完全一致，否则视为未找到，避免误配。
func (s *spotifyAgent) searchArtist(ctx context.Context, name string) (*Artist, error) {
	artists, err := s.client.searchArtists(ctx, name, 40)
	if err != nil || len(artists) == 0 {
		return nil, model.ErrNotFound
	}
	name = strings.ToLower(name)

	// Sort results, prioritizing artists with images, with similar names and with high popularity, in this order
	sort.Slice(artists, func(i, j int) bool {
		ai := fmt.Sprintf("%-5t-%03d-%04d", len(artists[i].Images) == 0, smetrics.WagnerFischer(name, strings.ToLower(artists[i].Name), 1, 1, 2), 1000-artists[i].Popularity)
		aj := fmt.Sprintf("%-5t-%03d-%04d", len(artists[j].Images) == 0, smetrics.WagnerFischer(name, strings.ToLower(artists[j].Name), 1, 1, 2), 1000-artists[j].Popularity)
		return ai < aj
	})

	// If the first one has the same name, that's the one
	if strings.ToLower(artists[0].Name) != name {
		return nil, model.ErrNotFound
	}
	return &artists[0], err
}

// init 在配置加载后注册代理。
func init() {
	conf.AddHook(func() {
		agents.Register(spotifyAgentName, spotifyConstructor)
	})
}
