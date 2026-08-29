// Package request 定义请求级上下文的键与读写辅助函数。
//
// 中间件在鉴权、识别客户端后把用户、播放器、转码配置等信息存入 context，
// 下游各层通过这里的 XxxFrom 函数取用，从而无需在每个函数签名中层层传递。
//
// contextKey 是包内私有类型，可避免与其他包的 context 键冲突。
package request

import (
	"context"

	"github.com/navidrome/navidrome/model"
)

// contextKey 是 context 键的专用类型，防止与其他包的键碰撞。
type contextKey string

const (
	User           = contextKey("user")
	Username       = contextKey("username")
	Client         = contextKey("client")
	Version        = contextKey("version")
	Player         = contextKey("player")
	Transcoding    = contextKey("transcoding")
	ClientUniqueId = contextKey("clientUniqueId")
	ReverseProxyIp = contextKey("reverseProxyIp")
	InternalAuth   = contextKey("internalAuth") // Used for internal API calls, e.g., from the plugins
	// InternalAuth 标记内部调用（如插件发起的 API 请求），
	// 使这类请求可绕过常规的 HTTP 鉴权流程
)

// allKeys 汇总所有键，供 AddValues 批量复制上下文值。
// 新增键时必须同步加入此列表，否则该值无法跨 context 传递。
var allKeys = []contextKey{
	User,
	Username,
	Client,
	Version,
	Player,
	Transcoding,
	ClientUniqueId,
	ReverseProxyIp,
	InternalAuth,
}

// 以下 WithXxx 函数把值写入 context，XxxFrom 函数读取并做类型断言。
// 读取失败统一返回零值与 false，由调用方决定如何处理缺失。

func WithUser(ctx context.Context, u model.User) context.Context {
	return context.WithValue(ctx, User, u)
}

func WithUsername(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, Username, username)
}

func WithClient(ctx context.Context, client string) context.Context {
	return context.WithValue(ctx, Client, client)
}

func WithVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, Version, version)
}

func WithPlayer(ctx context.Context, player model.Player) context.Context {
	return context.WithValue(ctx, Player, player)
}

func WithTranscoding(ctx context.Context, t model.Transcoding) context.Context {
	return context.WithValue(ctx, Transcoding, t)
}

func WithClientUniqueId(ctx context.Context, clientUniqueId string) context.Context {
	return context.WithValue(ctx, ClientUniqueId, clientUniqueId)
}

func WithReverseProxyIp(ctx context.Context, reverseProxyIp string) context.Context {
	return context.WithValue(ctx, ReverseProxyIp, reverseProxyIp)
}

func WithInternalAuth(ctx context.Context, username string) context.Context {
	return context.WithValue(ctx, InternalAuth, username)
}

func UserFrom(ctx context.Context) (model.User, bool) {
	v, ok := ctx.Value(User).(model.User)
	return v, ok
}

func UsernameFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(Username).(string)
	return v, ok
}

func ClientFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(Client).(string)
	return v, ok
}

func VersionFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(Version).(string)
	return v, ok
}

func PlayerFrom(ctx context.Context) (model.Player, bool) {
	v, ok := ctx.Value(Player).(model.Player)
	return v, ok
}

func TranscodingFrom(ctx context.Context) (model.Transcoding, bool) {
	v, ok := ctx.Value(Transcoding).(model.Transcoding)
	return v, ok
}

func ClientUniqueIdFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ClientUniqueId).(string)
	return v, ok
}

func ReverseProxyIpFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ReverseProxyIp).(string)
	return v, ok
}

// InternalAuthFrom 读取内部调用的用户名。
// 这里显式判空后再断言，语义上与其他 XxxFrom 一致，
// 只是把「键不存在」与「类型不符」两种情况分开处理。
func InternalAuthFrom(ctx context.Context) (string, bool) {
	if v := ctx.Value(InternalAuth); v != nil {
		if username, ok := v.(string); ok {
			return username, true
		}
	}
	return "", false
}

// AddValues 把 requestCtx 中的请求级值复制到 ctx。
//
// 用于后台任务：请求处理结束后 HTTP 的 context 会被取消，
// 若直接沿用会导致异步任务被中断。故新建不受取消影响的 context，
// 再把需要的值搬过来，从而保留用户身份等信息而不继承生命周期。
func AddValues(ctx, requestCtx context.Context) context.Context {
	for _, key := range allKeys {
		if v := requestCtx.Value(key); v != nil {
			ctx = context.WithValue(ctx, key, v)
		}
	}
	return ctx
}
