package cache

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/navidrome/navidrome/log"
)

const cacheSizeLimit = 100

// HTTPClient 为外部 API 请求加一层短期缓存，
// 避免刷新页面等操作对第三方服务产生重复调用（也受限于对方的调用频率限制）。
type HTTPClient struct {
	cache SimpleCache[string, string]
	hc    httpDoer
	ttl   time.Duration
}

type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// requestData 是请求的可序列化形式，用于生成缓存键。
type requestData struct {
	Method string
	Header http.Header
	URL    string
	Body   *string
}

// NewHTTPClient 包装一个 HTTP 客户端并加上缓存。
func NewHTTPClient(wrapped httpDoer, ttl time.Duration) *HTTPClient {
	c := &HTTPClient{hc: wrapped, ttl: ttl}
	c.cache = NewSimpleCache[string, string](Options{
		SizeLimit:  cacheSizeLimit,
		DefaultTTL: ttl,
	})
	return c
}

// Do 发起请求，命中缓存则直接返回缓存的响应。
func (c *HTTPClient) Do(req *http.Request) (*http.Response, error) {
	key := c.serializeReq(req)
	cached := true
	start := time.Now()
	respStr, err := c.cache.GetWithLoader(key, func(key string) (string, time.Duration, error) {
		cached = false
		req, err := c.deserializeReq(key)
		if err != nil {
			log.Trace(req.Context(), "CachedHTTPClient.Do", "key", key, err)
			return "", 0, err
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			log.Trace(req.Context(), "CachedHTTPClient.Do", "req", req, err)
			return "", 0, err
		}
		defer resp.Body.Close()
		return c.serializeResponse(resp), c.ttl, nil
	})
	log.Trace(req.Context(), "CachedHTTPClient.Do", "key", key, "cached", cached, "elapsed", time.Since(start), err)
	if err != nil {
		return nil, err
	}
	return c.deserializeResponse(req, respStr)
}

// serializeReq 把请求序列化为缓存键：
// 方法、URL、请求头与请求体都参与，任一不同即视为不同请求。
func (c *HTTPClient) serializeReq(req *http.Request) string {
	data := requestData{
		Method: req.Method,
		Header: req.Header,
		URL:    req.URL.String(),
	}
	if req.Body != nil {
		bodyData, _ := io.ReadAll(req.Body)
		bodyStr := base64.StdEncoding.EncodeToString(bodyData)
		data.Body = &bodyStr
	}
	j, _ := json.Marshal(&data)
	return string(j)
}

// deserializeReq 从缓存键还原请求，未命中时用它真正发起调用。
func (c *HTTPClient) deserializeReq(reqStr string) (*http.Request, error) {
	var data requestData
	_ = json.Unmarshal([]byte(reqStr), &data)
	var body io.Reader
	if data.Body != nil {
		bodyStr, _ := base64.StdEncoding.DecodeString(*data.Body)
		body = strings.NewReader(string(bodyStr))
	}
	req, err := http.NewRequest(data.Method, data.URL, body)
	if err != nil {
		return nil, err
	}
	req.Header = data.Header
	return req, nil
}

func (c *HTTPClient) serializeResponse(resp *http.Response) string {
	var b = &bytes.Buffer{}
	_ = resp.Write(b)
	return b.String()
}

func (c *HTTPClient) deserializeResponse(req *http.Request, respStr string) (*http.Response, error) {
	r := bufio.NewReader(strings.NewReader(respStr))
	return http.ReadResponse(r, req)
}
