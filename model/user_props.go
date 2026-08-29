package model

// UserPropsRepository 是用户级键值属性仓储，与 PropertyRepository 的区别在于
// 数据按用户隔离。典型用途是保存外部服务的会话凭据
// （如 Last.fm 的 session key）与用户个性化设置。
type UserPropsRepository interface {
	Put(userId, key string, value string) error
	// Get 键不存在时返回 ErrNotFound
	Get(userId, key string) (string, error)
	Delete(userId, key string) error
	// DefaultGet 键不存在时返回 defaultValue 而非报错
	DefaultGet(userId, key string, defaultValue string) (string, error)
}
