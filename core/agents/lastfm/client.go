package lastfm

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/navidrome/navidrome/log"
)

const (
	apiBaseUrl = "https://ws.audioscrobbler.com/2.0/"
)

// lastFMError 是 Last.fm 返回的业务错误。
// Code 用于区分可重试与不可恢复，见 Scrobble 中的分类逻辑。
type lastFMError struct {
	Code    int
	Message string
}

func (e *lastFMError) Error() string {
	return fmt.Sprintf("last.fm error(%d): %s", e.Code, e.Message)
}

// httpDoer 抽象出 HTTP 执行，便于注入带缓存的客户端与测试替身。
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// newClient 创建 Last.fm API 客户端。
func newClient(apiKey string, secret string, lang string, hc httpDoer) *client {
	return &client{apiKey, secret, lang, hc}
}

// client 是 Last.fm REST API 的薄封装。
type client struct {
	apiKey string
	secret string
	lang   string
	hc     httpDoer
}

// albumGetInfo 查询专辑信息。
func (c *client) albumGetInfo(ctx context.Context, name string, artist string, mbid string) (*Album, error) {
	params := url.Values{}
	params.Add("method", "album.getInfo")
	params.Add("album", name)
	params.Add("artist", artist)
	params.Add("mbid", mbid)
	params.Add("lang", c.lang)
	response, err := c.makeRequest(ctx, http.MethodGet, params, false)
	if err != nil {
		return nil, err
	}
	return &response.Album, nil
}

// artistGetInfo 查询艺人信息。
func (c *client) artistGetInfo(ctx context.Context, name string) (*Artist, error) {
	params := url.Values{}
	params.Add("method", "artist.getInfo")
	params.Add("artist", name)
	params.Add("lang", c.lang)
	response, err := c.makeRequest(ctx, http.MethodGet, params, false)
	if err != nil {
		return nil, err
	}
	return &response.Artist, nil
}

// artistGetSimilar 查询相似艺人。
func (c *client) artistGetSimilar(ctx context.Context, name string, limit int) (*SimilarArtists, error) {
	params := url.Values{}
	params.Add("method", "artist.getSimilar")
	params.Add("artist", name)
	params.Add("limit", strconv.Itoa(limit))
	response, err := c.makeRequest(ctx, http.MethodGet, params, false)
	if err != nil {
		return nil, err
	}
	return &response.SimilarArtists, nil
}

// artistGetTopTracks 查询艺人热门曲目。
func (c *client) artistGetTopTracks(ctx context.Context, name string, limit int) (*TopTracks, error) {
	params := url.Values{}
	params.Add("method", "artist.getTopTracks")
	params.Add("artist", name)
	params.Add("limit", strconv.Itoa(limit))
	response, err := c.makeRequest(ctx, http.MethodGet, params, false)
	if err != nil {
		return nil, err
	}
	return &response.TopTracks, nil
}

// GetToken 申请授权令牌，用户需在 Last.fm 网站上确认该令牌。
func (c *client) GetToken(ctx context.Context) (string, error) {
	params := url.Values{}
	params.Add("method", "auth.getToken")
	c.sign(params)
	response, err := c.makeRequest(ctx, http.MethodGet, params, true)
	if err != nil {
		return "", err
	}
	return response.Token, nil
}

// getSession 用已确认的令牌换取长期 session key。
func (c *client) getSession(ctx context.Context, token string) (string, error) {
	params := url.Values{}
	params.Add("method", "auth.getSession")
	params.Add("token", token)
	response, err := c.makeRequest(ctx, http.MethodGet, params, true)
	if err != nil {
		return "", err
	}
	return response.Session.Key, nil
}

// ScrobbleInfo 是一次上报所需的曲目信息。
type ScrobbleInfo struct {
	artist      string
	track       string
	album       string
	trackNumber int
	mbid        string
	duration    int
	albumArtist string
	timestamp   time.Time
}

// updateNowPlaying 上报当前播放。
// 被忽略时（code != 0）只告警不报错，通常是曲目在 Last.fm 无法匹配。
func (c *client) updateNowPlaying(ctx context.Context, sessionKey string, info ScrobbleInfo) error {
	params := url.Values{}
	params.Add("method", "track.updateNowPlaying")
	params.Add("artist", info.artist)
	params.Add("track", info.track)
	params.Add("album", info.album)
	params.Add("trackNumber", strconv.Itoa(info.trackNumber))
	params.Add("mbid", info.mbid)
	params.Add("duration", strconv.Itoa(info.duration))
	params.Add("albumArtist", info.albumArtist)
	params.Add("sk", sessionKey)
	resp, err := c.makeRequest(ctx, http.MethodPost, params, true)
	if err != nil {
		return err
	}
	if resp.NowPlaying.IgnoredMessage.Code != "0" {
		log.Warn(ctx, "LastFM: NowPlaying was ignored", "code", resp.NowPlaying.IgnoredMessage.Code,
			"text", resp.NowPlaying.IgnoredMessage.Text)
	}
	return nil
}

// scrobble 上报播放记录，被忽略或未被接受时记录告警。
func (c *client) scrobble(ctx context.Context, sessionKey string, info ScrobbleInfo) error {
	params := url.Values{}
	params.Add("method", "track.scrobble")
	params.Add("timestamp", strconv.FormatInt(info.timestamp.Unix(), 10))
	params.Add("artist", info.artist)
	params.Add("track", info.track)
	params.Add("album", info.album)
	params.Add("trackNumber", strconv.Itoa(info.trackNumber))
	params.Add("mbid", info.mbid)
	params.Add("duration", strconv.Itoa(info.duration))
	params.Add("albumArtist", info.albumArtist)
	params.Add("sk", sessionKey)
	resp, err := c.makeRequest(ctx, http.MethodPost, params, true)
	if err != nil {
		return err
	}
	if resp.Scrobbles.Scrobble.IgnoredMessage.Code != "0" {
		log.Warn(ctx, "LastFM: scrobble was ignored", "code", resp.Scrobbles.Scrobble.IgnoredMessage.Code,
			"text", resp.Scrobbles.Scrobble.IgnoredMessage.Text, "info", info)
	}
	if resp.Scrobbles.Attr.Accepted != 1 {
		log.Warn(ctx, "LastFM: scrobble was not accepted", "code", resp.Scrobbles.Scrobble.IgnoredMessage.Code,
			"text", resp.Scrobbles.Scrobble.IgnoredMessage.Text, "info", info)
	}
	return nil
}

// makeRequest 发起请求并解析响应。
// 即便 HTTP 状态非 200，Last.fm 仍会返回带错误码的 JSON，故优先解析 body；
// 只有 JSON 也解析不出时才退回报 HTTP 状态错误。
func (c *client) makeRequest(ctx context.Context, method string, params url.Values, signed bool) (*Response, error) {
	params.Add("format", "json")
	params.Add("api_key", c.apiKey)

	if signed {
		c.sign(params)
	}

	req, _ := http.NewRequestWithContext(ctx, method, apiBaseUrl, nil)
	req.URL.RawQuery = params.Encode()

	log.Trace(ctx, fmt.Sprintf("Sending Last.fm %s request", req.Method), "url", req.URL)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)

	var response Response
	jsonErr := decoder.Decode(&response)
	if resp.StatusCode != 200 && jsonErr != nil {
		return nil, fmt.Errorf("last.fm http status: (%d)", resp.StatusCode)
	}
	if jsonErr != nil {
		return nil, jsonErr
	}
	if response.Error != 0 {
		return &response, &lastFMError{Code: response.Error, Message: response.Message}
	}

	return &response, nil
}

// sign 计算 api_sig 签名。
// Last.fm 要求：按参数名字典序拼接 key+value，末尾追加 secret 后取 MD5。
// format 与 callback 不参与签名。
func (c *client) sign(params url.Values) {
	// the parameters must be in order before hashing
	keys := make([]string, 0, len(params))
	for k := range params {
		if slices.Contains([]string{"format", "callback"}, k) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	msg := strings.Builder{}
	for _, k := range keys {
		msg.WriteString(k)
		msg.WriteString(params[k][0])
	}
	msg.WriteString(c.secret)
	hash := md5.Sum([]byte(msg.String()))
	params.Add("api_sig", hex.EncodeToString(hash[:]))
}
