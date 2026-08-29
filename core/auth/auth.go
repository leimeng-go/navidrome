package auth

import (
	"cmp"
	"context"
	"crypto/sha256"
	"sync"
	"time"

	"github.com/go-chi/jwtauth/v5"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils"
)

var (
	once sync.Once
	// TokenAuth 是全局 JWT 签发与校验器，须先调用 Init 初始化。
	TokenAuth *jwtauth.JWTAuth
)

// Init creates a JWTAuth object from the secret stored in the DB.
// If the secret is not found, it will create a new one and store it in the DB.
//
// Init 初始化 JWT 认证器。
// 密钥持久化在数据库中（加密存储），使服务重启后已签发的会话依然有效。
// 密钥解密失败时重新生成——此时旧会话全部失效，但总好过完全无法启动。
func Init(ds model.DataStore) {
	once.Do(func() {
		ctx := context.TODO()
		log.Info("Setting Session Timeout", "value", conf.Server.SessionTimeout)

		secret, err := ds.Property(ctx).Get(consts.JWTSecretKey)
		if err != nil || secret == "" {
			log.Info(ctx, "Creating new JWT secret, used for encrypting UI sessions")
			secret = createNewSecret(ctx, ds)
		} else {
			if secret, err = utils.Decrypt(ctx, getEncKey(), secret); err != nil {
				log.Error(ctx, "Could not decrypt JWT secret, creating a new one", err)
				secret = createNewSecret(ctx, ds)
			}
		}

		TokenAuth = jwtauth.New("HS256", []byte(secret), nil)
	})
}

// createBaseClaims 构造所有 token 共有的基础 claims。
func createBaseClaims() map[string]any {
	tokenClaims := map[string]any{}
	tokenClaims[jwt.IssuerKey] = consts.JWTIssuer
	return tokenClaims
}

// CreatePublicToken 签发不过期的公开 token，用于分享链接等场景。
func CreatePublicToken(claims map[string]any) (string, error) {
	tokenClaims := createBaseClaims()
	for k, v := range claims {
		tokenClaims[k] = v
	}
	_, token, err := TokenAuth.Encode(tokenClaims)

	return token, err
}

// CreateExpiringPublicToken 签发带过期时间的公开 token，exp 为零值时不设过期。
func CreateExpiringPublicToken(exp time.Time, claims map[string]any) (string, error) {
	tokenClaims := createBaseClaims()
	if !exp.IsZero() {
		tokenClaims[jwt.ExpirationKey] = exp.UTC().Unix()
	}
	for k, v := range claims {
		tokenClaims[k] = v
	}
	_, token, err := TokenAuth.Encode(tokenClaims)

	return token, err
}

// CreateToken 为登录用户签发会话 token，
// 携带用户 ID 与管理员标记，避免每次请求都回表查询。
func CreateToken(u *model.User) (string, error) {
	claims := createBaseClaims()
	claims[jwt.SubjectKey] = u.UserName
	claims[jwt.IssuedAtKey] = time.Now().UTC().Unix()
	claims["uid"] = u.ID
	claims["adm"] = u.IsAdmin
	token, _, err := TokenAuth.Encode(claims)
	if err != nil {
		return "", err
	}

	return TouchToken(token)
}

// TouchToken 以当前时间重新计算过期时间并重签 token，
// 实现「活跃即续期」的滑动会话。
func TouchToken(token jwt.Token) (string, error) {
	claims, err := token.AsMap(context.Background())
	if err != nil {
		return "", err
	}

	claims[jwt.ExpirationKey] = time.Now().UTC().Add(conf.Server.SessionTimeout).Unix()
	_, newToken, err := TokenAuth.Encode(claims)

	return newToken, err
}

// Validate 校验 token 签名与有效期，返回其中的 claims。
func Validate(tokenStr string) (map[string]interface{}, error) {
	token, err := jwtauth.VerifyToken(TokenAuth, tokenStr)
	if err != nil {
		return nil, err
	}
	return token.AsMap(context.Background())
}

// WithAdminUser 把管理员身份注入上下文，供后台任务（扫描、定时任务）使用。
// 尚无任何用户时（首次启动）只记 Debug，属正常情况；
// 其他情况说明数据异常，记 Error 并退化为空用户。
func WithAdminUser(ctx context.Context, ds model.DataStore) context.Context {
	u, err := ds.User(ctx).FindFirstAdmin()
	if err != nil {
		c, err := ds.User(ctx).CountAll()
		if c == 0 && err == nil {
			log.Debug(ctx, "No admin user yet!", err)
		} else {
			log.Error(ctx, "No admin user found!", err)
		}
		u = &model.User{}
	}

	ctx = request.WithUsername(ctx, u.UserName)
	return request.WithUser(ctx, *u)
}

// createNewSecret 生成新的 JWT 密钥并加密存入数据库。
// 加密或落库失败时仍返回明文密钥，保证本次进程可用（但重启后会话失效）。
func createNewSecret(ctx context.Context, ds model.DataStore) string {
	secret := id.NewRandom()
	encSecret, err := utils.Encrypt(ctx, getEncKey(), secret)
	if err != nil {
		log.Error(ctx, "Could not encrypt JWT secret", err)
		return secret
	}
	if err := ds.Property(ctx).Put(consts.JWTSecretKey, encSecret); err != nil {
		log.Error(ctx, "Could not save JWT secret in DB", err)
	}
	return secret
}

// getEncKey 派生用于加密 JWT 密钥的对称密钥。
// 取配置的密码加密密钥（未配置则用内置默认值），经 SHA-256 摘要成定长 32 字节。
func getEncKey() []byte {
	key := cmp.Or(
		conf.Server.PasswordEncryptionKey,
		consts.DefaultEncryptionKey,
	)
	sum := sha256.Sum256([]byte(key))
	return sum[:]
}
