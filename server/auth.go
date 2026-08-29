package server

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/deluan/rest"
	"github.com/go-chi/jwtauth/v5"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/gravatar"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

var (
	ErrNoUsers         = errors.New("no users created")
	ErrUnauthenticated = errors.New("request not authenticated")
)

// login 返回登录接口处理器。
func login(ds model.DataStore) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, err := getCredentialsFromBody(r)
		if err != nil {
			log.Error(r, "Parsing request body", err)
			_ = rest.RespondWithError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}

		doLogin(ds, username, password, w, r)
	}
}

// doLogin 执行登录：校验凭据、签发 JWT，并返回用户信息负载。
// 校验出错与凭据不符区别对待：前者是服务端异常，后者统一回 401 且不透露具体原因。
func doLogin(ds model.DataStore, username string, password string, w http.ResponseWriter, r *http.Request) {
	user, err := validateLogin(ds.User(r.Context()), username, password)
	if err != nil {
		_ = rest.RespondWithError(w, http.StatusInternalServerError, "Unknown error authentication user. Please try again")
		return
	}
	if user == nil {
		log.Warn(r, "Unsuccessful login", "username", username, "request", r.Header)
		_ = rest.RespondWithError(w, http.StatusUnauthorized, "Invalid username or password")
		return
	}

	tokenString, err := auth.CreateToken(user)
	if err != nil {
		_ = rest.RespondWithError(w, http.StatusInternalServerError, "Unknown error authenticating user. Please try again")
		return
	}
	payload := buildAuthPayload(user)
	payload["token"] = tokenString
	_ = rest.RespondWithJSON(w, http.StatusOK, payload)
}

// buildAuthPayload 构造返回给前端的用户负载。
//
// 额外附带 subsonicSalt/subsonicToken：前端需要它们去调用 Subsonic API，
// 这样密码本身无需在前端保存。salt 随机生成，token 为 md5(密码+salt)。
// 随机数生成失败时降级返回不含 Subsonic 凭据的负载，不阻断登录。
func buildAuthPayload(user *model.User) map[string]interface{} {
	payload := map[string]interface{}{
		"id":       user.ID,
		"name":     user.Name,
		"username": user.UserName,
		"isAdmin":  user.IsAdmin,
	}
	if conf.Server.EnableGravatar && user.Email != "" {
		payload["avatar"] = gravatar.Url(user.Email, 50)
	}

	bytes := make([]byte, 3)
	_, err := rand.Read(bytes)
	if err != nil {
		log.Error("Could not create subsonic salt", "user", user.UserName, err)
		return payload
	}
	subsonicSalt := hex.EncodeToString(bytes)
	payload["subsonicSalt"] = subsonicSalt

	subsonicToken := md5.Sum([]byte(user.Password + subsonicSalt))
	payload["subsonicToken"] = hex.EncodeToString(subsonicToken[:])

	return payload
}

// getCredentialsFromBody 从请求体 JSON 中解析用户名与密码。
func getCredentialsFromBody(r *http.Request) (username string, password string, err error) {
	data := make(map[string]string)
	decoder := json.NewDecoder(r.Body)
	if err = decoder.Decode(&data); err != nil {
		log.Error(r, "parsing request body", err)
		err = errors.New("invalid request payload")
		return
	}
	username = data["username"]
	password = data["password"]
	return username, password, nil
}

// createAdmin 返回创建首个管理员的处理器。
// 已存在任何用户时拒绝，防止该接口被用来越权创建管理员。
func createAdmin(ds model.DataStore) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, err := getCredentialsFromBody(r)
		if err != nil {
			log.Error(r, "parsing request body", err)
			_ = rest.RespondWithError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		c, err := ds.User(r.Context()).CountAll()
		if err != nil {
			_ = rest.RespondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if c > 0 {
			_ = rest.RespondWithError(w, http.StatusForbidden, "Cannot create another first admin")
			return
		}
		err = createAdminUser(r.Context(), ds, username, password)
		if err != nil {
			_ = rest.RespondWithError(w, http.StatusInternalServerError, err.Error())
			return
		}
		doLogin(ds, username, password, w, r)
	}
}

// createAdminUser 创建首个管理员用户。
func createAdminUser(ctx context.Context, ds model.DataStore, username, password string) error {
	log.Warn(ctx, "Creating initial user", "user", username)
	now := time.Now()
	caser := cases.Title(language.Und)
	initialUser := model.User{
		ID:          id.NewRandom(),
		UserName:    username,
		Name:        caser.String(username),
		Email:       "",
		NewPassword: password,
		IsAdmin:     true,
		LastLoginAt: &now,
	}
	err := ds.User(ctx).Put(&initialUser)
	if err != nil {
		log.Error(ctx, "Could not create initial user", "user", initialUser, err)
	}
	return nil
}

// validateLogin 校验用户名密码。
// 用户不存在与密码错误都返回 (nil, nil)，由调用方统一回相同的错误，避免用户名枚举。
func validateLogin(userRepo model.UserRepository, userName, password string) (*model.User, error) {
	u, err := userRepo.FindByUsernameWithPassword(userName)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if u.Password != password {
		return nil, nil
	}
	err = userRepo.UpdateLastLoginAt(u.ID)
	if err != nil {
		log.Error("Could not update LastLoginAt", "user", userName)
	}
	return u, nil
}

// JWTVerifier 从请求头、Cookie 或查询参数中提取并校验 JWT。
func JWTVerifier(next http.Handler) http.Handler {
	return jwtauth.Verify(auth.TokenAuth, tokenFromHeader, jwtauth.TokenFromCookie, jwtauth.TokenFromQuery)(next)
}

// tokenFromHeader 从自定义认证头中提取 Bearer 令牌。
func tokenFromHeader(r *http.Request) string {
	// Get token from authorization header.
	bearer := r.Header.Get(consts.UIAuthorizationHeader)
	if len(bearer) > 7 && strings.ToUpper(bearer[0:6]) == "BEARER" {
		return bearer[7:]
	}
	return ""
}

// UsernameFromToken 从已校验的 JWT 中取出用户名。
func UsernameFromToken(r *http.Request) string {
	token, claims, err := jwtauth.FromContext(r.Context())
	if err != nil || claims["sub"] == nil || token == nil {
		return ""
	}
	log.Trace(r, "Found username in JWT token", "username", token.Subject())
	return token.Subject()
}

// UsernameFromExtAuthHeader 从反向代理注入的请求头中取用户名。
//
// 该头部由代理设置，客户端可伪造，故必须先确认请求确实来自可信代理 IP，
// 否则任何人都能凭一个请求头冒充任意用户。
func UsernameFromExtAuthHeader(r *http.Request) string {
	if conf.Server.ExtAuth.TrustedSources == "" {
		return ""
	}
	reverseProxyIp, ok := request.ReverseProxyIpFrom(r.Context())
	if !ok {
		log.Error("ExtAuth enabled but no proxy IP found in request context. Please report this error.")
		return ""
	}
	if !validateIPAgainstList(reverseProxyIp, conf.Server.ExtAuth.TrustedSources) {
		log.Warn(r.Context(), "IP is not whitelisted for external authentication", "proxy-ip", reverseProxyIp, "client-ip", r.RemoteAddr)
		return ""
	}
	username := r.Header.Get(conf.Server.ExtAuth.UserHeader)
	if username == "" {
		return ""
	}
	log.Trace(r, "Found username in ExtAuth.UserHeader", "username", username)
	return username
}

// InternalAuth 取内部调用（如插件）在上下文中标记的用户名。
func InternalAuth(r *http.Request) string {
	username, ok := request.InternalAuthFrom(r.Context())
	if !ok {
		return ""
	}
	log.Trace(r, "Found username in InternalAuth", "username", username)
	return username
}

// UsernameFromConfig 返回开发用的自动登录用户名。
func UsernameFromConfig(*http.Request) string {
	return conf.Server.DevAutoLoginUsername
}

// contextWithUser 把用户信息注入上下文，供后续处理与日志使用。
func contextWithUser(ctx context.Context, ds model.DataStore, username string) (context.Context, error) {
	user, err := ds.User(ctx).FindByUsername(username)
	if err == nil {
		ctx = log.NewContext(ctx, "username", username)
		ctx = request.WithUsername(ctx, user.UserName)
		return request.WithUser(ctx, *user), nil
	}
	log.Error(ctx, "Authenticated username not found in DB", "username", username)
	return ctx, err
}

// authenticateRequest 依次尝试各来源解析用户名，取第一个非空结果。
func authenticateRequest(ds model.DataStore, r *http.Request, findUsernameFns ...func(r *http.Request) string) (context.Context, error) {
	var username string
	for _, fn := range findUsernameFns {
		username = fn(r)
		if username != "" {
			break
		}
	}
	if username == "" {
		return nil, ErrUnauthenticated
	}

	return contextWithUser(r.Context(), ds, username)
}

// Authenticator 是认证中间件。
// 来源优先级：开发自动登录 > JWT > 反向代理头。
func Authenticator(ds model.DataStore) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, err := authenticateRequest(ds, r, UsernameFromConfig, UsernameFromToken, UsernameFromExtAuthHeader)
			if err != nil {
				_ = rest.RespondWithError(w, http.StatusUnauthorized, "Not authenticated")
				return
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// JWTRefresher updates the expiry date of the received JWT token, and add the new one to the Authorization Header
func JWTRefresher(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		token, _, err := jwtauth.FromContext(ctx)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		newTokenString, err := auth.TouchToken(token)
		if err != nil {
			log.Error(r, "Could not sign new token", err)
			_ = rest.RespondWithError(w, http.StatusUnauthorized, "Not authenticated")
			return
		}

		w.Header().Set(consts.UIAuthorizationHeader, newTokenString)
		next.ServeHTTP(w, r)
	})
}

// handleLoginFromHeaders 处理由请求头驱动的免密登录（反向代理或开发自动登录）。
//
// 用户不存在时自动创建，密码取随机值并加前缀标记为自动生成——
// 身份由外部系统保证，本地密码不参与认证。
// 首个被创建的用户自动成为管理员。
func handleLoginFromHeaders(ds model.DataStore, r *http.Request) map[string]interface{} {
	username := UsernameFromConfig(r)
	if username == "" {
		username = UsernameFromExtAuthHeader(r)
		if username == "" {
			return nil
		}
	}

	userRepo := ds.User(r.Context())
	user, err := userRepo.FindByUsernameWithPassword(username)
	if user == nil || err != nil {
		log.Info(r, "User passed in header not found", "user", username)
		// Check if this is the first user being created
		count, _ := userRepo.CountAll()
		isFirstUser := count == 0

		newUser := model.User{
			ID:          id.NewRandom(),
			UserName:    username,
			Name:        username,
			Email:       "",
			NewPassword: consts.PasswordAutogenPrefix + id.NewRandom(),
			IsAdmin:     isFirstUser, // Make the first user an admin
		}
		err := userRepo.Put(&newUser)
		if err != nil {
			log.Error(r, "Could not create new user", "user", username, err)
			return nil
		}
		user, err = userRepo.FindByUsernameWithPassword(username)
		if user == nil || err != nil {
			log.Error(r, "Created user but failed to fetch it", "user", username)
			return nil
		}
	}

	err = userRepo.UpdateLastLoginAt(user.ID)
	if err != nil {
		log.Error(r, "Could not update LastLoginAt", "user", username, err)
		return nil
	}

	return buildAuthPayload(user)
}

// validateIPAgainstList 校验 IP 是否落在逗号分隔的 CIDR 白名单内。
//
// 两个特殊处理：Unix socket 场景下远端地址为 '@'（见 golang/go#49825），
// 需白名单显式包含 '@'；带端口的地址先剥离端口再比较。
func validateIPAgainstList(ip string, comaSeparatedList string) bool {
	if comaSeparatedList == "" || ip == "" {
		return false
	}

	cidrs := strings.Split(comaSeparatedList, ",")

	// Per https://github.com/golang/go/issues/49825, the remote address
	// on a unix socket is '@'
	if ip == "@" && strings.HasPrefix(conf.Server.Address, "unix:") {
		return slices.Contains(cidrs, "@")
	}

	if net.ParseIP(ip) == nil {
		ip, _, _ = net.SplitHostPort(ip)
	}

	if ip == "" {
		return false
	}

	testedIP, _, err := net.ParseCIDR(fmt.Sprintf("%s/32", ip))
	if err != nil {
		return false
	}

	for _, cidr := range cidrs {
		_, ipnet, err := net.ParseCIDR(cidr)
		if err == nil && ipnet.Contains(testedIP) {
			return true
		}
	}

	return false
}
