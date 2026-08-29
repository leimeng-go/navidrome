package deezer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/navidrome/navidrome/log"
)

// jwtToken 是带过期时间的 JWT 缓存，可被多个请求并发读写。
type jwtToken struct {
	token     string
	expiresAt time.Time
	mu        sync.RWMutex
}

// get 返回缓存的 token，已过期则返回 false。
func (j *jwtToken) get() (string, bool) {
	j.mu.RLock()
	defer j.mu.RUnlock()
	if time.Now().Before(j.expiresAt) {
		return j.token, true
	}
	return "", false
}

// set 写入 token 及其有效期。
func (j *jwtToken) set(token string, expiresIn time.Duration) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.token = token
	j.expiresAt = time.Now().Add(expiresIn)
}

// getJWT 获取访问 GraphQL 接口所需的匿名 JWT。
//
// 有效期从 token 自身的 exp 声明解析而来，而非依赖固定常量。
// 预留 1 分钟缓冲，以容忍时钟偏差与网络延迟，避免用到临界过期的 token。
// 此处只解析不校验签名：签名由服务端验证，客户端仅需读取过期时间。
func (c *client) getJWT(ctx context.Context) (string, error) {
	// Check if we have a valid cached token
	if token, valid := c.jwt.get(); valid {
		return token, nil
	}

	// Fetch a new anonymous token
	req, err := http.NewRequestWithContext(ctx, "GET", authBaseURL+"/login/anonymous?jo=p&rto=c", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpDoer.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("deezer: failed to get JWT token: %s", resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	type authResponse struct {
		JWT string `json:"jwt"`
	}

	var result authResponse
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("deezer: failed to parse auth response: %w", err)
	}

	if result.JWT == "" {
		return "", errors.New("deezer: no JWT token in response")
	}

	// Parse JWT to get actual expiration time
	token, err := jwt.ParseString(result.JWT, jwt.WithVerify(false), jwt.WithValidate(false))
	if err != nil {
		return "", fmt.Errorf("deezer: failed to parse JWT token: %w", err)
	}

	// Calculate TTL with a 1-minute buffer for clock skew and network delays
	expiresAt := token.Expiration()
	if expiresAt.IsZero() {
		return "", errors.New("deezer: JWT token has no expiration time")
	}

	ttl := time.Until(expiresAt) - 1*time.Minute
	if ttl <= 0 {
		return "", errors.New("deezer: JWT token already expired or expires too soon")
	}

	c.jwt.set(result.JWT, ttl)
	log.Trace(ctx, "Fetched new Deezer JWT token", "expiresAt", expiresAt, "ttl", ttl)

	return result.JWT, nil
}
