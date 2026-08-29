package listenbrainz

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path"

	"github.com/navidrome/navidrome/log"
)

// listenBrainzError 是服务端返回的业务错误，Code 用于判定是否可重试。
type listenBrainzError struct {
	Code    int
	Message string
}

func (e *listenBrainzError) Error() string {
	return fmt.Sprintf("ListenBrainz error(%d): %s", e.Code, e.Message)
}

// httpDoer 抽象 HTTP 执行，便于注入缓存客户端与测试替身。
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// newClient 创建 ListenBrainz API 客户端。
func newClient(baseURL string, hc httpDoer) *client {
	return &client{baseURL, hc}
}

// client 是 ListenBrainz API 的薄封装。
type client struct {
	baseURL string
	hc      httpDoer
}

// listenBrainzResponse 是各接口的统一响应结构。
type listenBrainzResponse struct {
	Code     int    `json:"code"`
	Message  string `json:"message"`
	Error    string `json:"error"`
	Status   string `json:"status"`
	Valid    bool   `json:"valid"`
	UserName string `json:"user_name"`
}

// listenBrainzRequest 是请求封装，ApiKey 以 Authorization 头发送而非请求体。
type listenBrainzRequest struct {
	ApiKey string
	Body   listenBrainzRequestBody
}

// listenBrainzRequestBody 是请求体。
type listenBrainzRequestBody struct {
	ListenType listenType   `json:"listen_type,omitempty"`
	Payload    []listenInfo `json:"payload,omitempty"`
}

// listenType 区分上报类型：单次收听或正在播放。
type listenType string

const (
	Single     listenType = "single"
	PlayingNow listenType = "playing_now"
)

// listenInfo 是一条收听记录。playing_now 类型不带 ListenedAt。
type listenInfo struct {
	ListenedAt    int           `json:"listened_at,omitempty"`
	TrackMetadata trackMetadata `json:"track_metadata,omitempty"`
}

// trackMetadata 是曲目元数据。
type trackMetadata struct {
	ArtistName     string         `json:"artist_name,omitempty"`
	TrackName      string         `json:"track_name,omitempty"`
	ReleaseName    string         `json:"release_name,omitempty"`
	AdditionalInfo additionalInfo `json:"additional_info,omitempty"`
}

// additionalInfo 是附加元数据，MBID 让服务端可精确匹配而非靠字符串。
type additionalInfo struct {
	SubmissionClient        string   `json:"submission_client,omitempty"`
	SubmissionClientVersion string   `json:"submission_client_version,omitempty"`
	TrackNumber             int      `json:"tracknumber,omitempty"`
	ArtistNames             []string `json:"artist_names,omitempty"`
	ArtistMBIDs             []string `json:"artist_mbids,omitempty"`
	RecordingMBID           string   `json:"recording_mbid,omitempty"`
	ReleaseMBID             string   `json:"release_mbid,omitempty"`
	ReleaseGroupMBID        string   `json:"release_group_mbid,omitempty"`
	DurationMs              int      `json:"duration_ms,omitempty"`
}

// validateToken 校验 token 是否有效，并返回对应用户名。
func (c *client) validateToken(ctx context.Context, apiKey string) (*listenBrainzResponse, error) {
	r := &listenBrainzRequest{
		ApiKey: apiKey,
	}
	response, err := c.makeRequest(ctx, http.MethodGet, "validate-token", r)
	if err != nil {
		return nil, err
	}
	return response, nil
}

// updateNowPlaying 上报当前播放。
func (c *client) updateNowPlaying(ctx context.Context, apiKey string, li listenInfo) error {
	r := &listenBrainzRequest{
		ApiKey: apiKey,
		Body: listenBrainzRequestBody{
			ListenType: PlayingNow,
			Payload:    []listenInfo{li},
		},
	}

	resp, err := c.makeRequest(ctx, http.MethodPost, "submit-listens", r)
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		log.Warn(ctx, "ListenBrainz: NowPlaying was not accepted", "status", resp.Status)
	}
	return nil
}

// scrobble 上报一条收听记录。
func (c *client) scrobble(ctx context.Context, apiKey string, li listenInfo) error {
	r := &listenBrainzRequest{
		ApiKey: apiKey,
		Body: listenBrainzRequestBody{
			ListenType: Single,
			Payload:    []listenInfo{li},
		},
	}
	resp, err := c.makeRequest(ctx, http.MethodPost, "submit-listens", r)
	if err != nil {
		return err
	}
	if resp.Status != "ok" {
		log.Warn(ctx, "ListenBrainz: Scrobble was not accepted", "status", resp.Status)
	}
	return nil
}

// path 拼接完整请求地址，保留 baseURL 中可能存在的路径前缀。
func (c *client) path(endpoint string) (string, error) {
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(u.Path, endpoint)
	return u.String(), nil
}

// makeRequest 发起请求并解析响应。
// 服务端在出错时仍返回带 code 的 JSON，故优先解析 body，
// 解析不出时才退回报 HTTP 状态错误。
func (c *client) makeRequest(ctx context.Context, method string, endpoint string, r *listenBrainzRequest) (*listenBrainzResponse, error) {
	b, _ := json.Marshal(r.Body)
	uri, err := c.path(endpoint)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequestWithContext(ctx, method, uri, bytes.NewBuffer(b))
	req.Header.Add("Content-Type", "application/json; charset=UTF-8")

	if r.ApiKey != "" {
		req.Header.Add("Authorization", fmt.Sprintf("Token %s", r.ApiKey))
	}

	log.Trace(ctx, fmt.Sprintf("Sending ListenBrainz %s request", req.Method), "url", req.URL)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()
	decoder := json.NewDecoder(resp.Body)

	var response listenBrainzResponse
	jsonErr := decoder.Decode(&response)
	if resp.StatusCode != 200 && jsonErr != nil {
		return nil, fmt.Errorf("ListenBrainz: HTTP Error, Status: (%d)", resp.StatusCode)
	}
	if jsonErr != nil {
		return nil, jsonErr
	}
	if response.Code != 0 && response.Code != 200 {
		return &response, &listenBrainzError{Code: response.Code, Message: response.Error}
	}

	return &response, nil
}
