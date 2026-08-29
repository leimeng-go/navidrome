package model

import "errors"

// 领域层的哨兵错误。各层通过 errors.Is 判断类型，
// API 层再据此映射为对应的 HTTP 状态码或 Subsonic 错误码。
var (
	ErrNotFound      = errors.New("data not found")              // 数据不存在
	ErrInvalidAuth   = errors.New("invalid authentication")      // 凭据无效（用户名/密码/token 错误）
	ErrNotAuthorized = errors.New("not authorized")              // 已认证但无权限
	ErrExpired       = errors.New("access expired")              // 访问已过期（如分享链接失效）
	ErrNotAvailable  = errors.New("functionality not available") // 功能不可用（未启用或依赖缺失）
	ErrValidation    = errors.New("validation error")            // 入参校验失败
)
