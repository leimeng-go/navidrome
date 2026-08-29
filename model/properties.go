package model

// PropertyRepository 是全局键值属性仓储，用于持久化系统级状态，
// 例如上次扫描使用的 PID 配置、数据库 schema 版本标记等。
type PropertyRepository interface {
	Put(id string, value string) error
	// Get 键不存在时返回 ErrNotFound
	Get(id string) (string, error)
	Delete(id string) error
	// DefaultGet 键不存在时返回 defaultValue 而非报错
	DefaultGet(id string, defaultValue string) (string, error)
}
