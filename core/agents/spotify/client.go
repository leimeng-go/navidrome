package spotify

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/navidrome/navidrome/log"
)

const apiBaseUrl = "https://api.spotify.com/v1/"

var (
	ErrNotFound = errors.New("spotify: not found")
)

// httpDoer 抽象 HTTP 执行，便于注入缓存客户端与测试替身。
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// newClient 创建 Spotify API 客户端。
func newClient(id, secret string, hc httpDoer) *client {
	return &client{id, secret, hc}
}

// client 是 Spotify Web API 的薄封装。
type client struct {
	id     string
	secret string
	hc     httpDoer
}

// searchArtists 按名称搜索艺人。
func (c *client) searchArtists(ctx context.Context, name string, limit int) ([]Artist, error) {
	token, err := c.authorize(ctx)
	if err != nil {
		return nil, err
	}

	params := url.Values{}
	params.Add("type", "artist")
	params.Add("q", name)
	params.Add("offset", "0")
	params.Add("limit", strconv.Itoa(limit))
	req, _ := http.NewRequestWithContext(ctx, "GET", apiBaseUrl+"search", nil)
	req.URL.RawQuery = params.Encode()
	req.Header.Add("Authorization", "Bearer "+token)

	var results SearchResults
	err = c.makeRequest(req, &results)
	if err != nil {
		return nil, err
	}

	if len(results.Artists.Items) == 0 {
		return nil, ErrNotFound
	}
	return results.Artists.Items, err
}

// authorize 走 client credentials 流程换取访问令牌。
// 该流程不涉及用户身份，仅用于访问公开数据。
// 令牌本身不在此缓存，依赖注入的 HTTP 缓存客户端复用响应。
func (c *client) authorize(ctx context.Context) (string, error) {
	payload := url.Values{}
	payload.Add("grant_type", "client_credentials")

	encodePayload := payload.Encode()
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://accounts.spotify.com/api/token", strings.NewReader(encodePayload))
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Content-Length", strconv.Itoa(len(encodePayload)))
	auth := c.id + ":" + c.secret
	req.Header.Add("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(auth)))

	response := map[string]interface{}{}
	err := c.makeRequest(req, &response)
	if err != nil {
		return "", err
	}

	if v, ok := response["access_token"]; ok {
		return v.(string), nil
	}
	log.Error(ctx, "Invalid spotify response", "resp", response)
	return "", errors.New("invalid response")
}

// makeRequest 发起请求并解析 JSON 响应。
func (c *client) makeRequest(req *http.Request, response interface{}) error {
	log.Trace(req.Context(), fmt.Sprintf("Sending Spotify %s request", req.Method), "url", req.URL)
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode != 200 {
		return c.parseError(data)
	}

	return json.Unmarshal(data, response)
}

// parseError 解析 Spotify 的错误响应体。
func (c *client) parseError(data []byte) error {
	var e Error
	err := json.Unmarshal(data, &e)
	if err != nil {
		return err
	}
	return fmt.Errorf("spotify error(%s): %s", e.Code, e.Message)
}
