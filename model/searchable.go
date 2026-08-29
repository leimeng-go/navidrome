package model

// SearchableRepository 是支持全文搜索的仓储能力，
// 由 MediaFile/Album/Artist 等仓储组合使用。泛型参数 T 为返回的集合类型。
type SearchableRepository[T any] interface {
	// Search 按关键词搜索，offset/size 用于分页
	Search(q string, offset, size int, options ...QueryOptions) (T, error)
}
