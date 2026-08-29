package model

import (
	"context"

	"github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
)

// QueryOptions 是所有仓储查询的通用选项，最终会被翻译成 SQL 的
// ORDER BY / LIMIT / OFFSET / WHERE 子句。
type QueryOptions struct {
	Sort    string           // 排序字段名（仓储内部映射为实际列名）
	Order   string           // 排序方向：asc 或 desc
	Max     int              // 最大返回条数，对应 SQL LIMIT
	Offset  int              // 跳过条数，对应 SQL OFFSET
	Filters squirrel.Sqlizer // 附加过滤条件，由 squirrel 拼装为 WHERE 子句
	Seed    string           // for random sorting
	// Seed：随机排序时的种子，保证同一分页请求多次翻页顺序稳定
}

// ResourceRepository 是通用资源仓储，直接复用 deluan/rest 的接口约定，
// 供 Native API 以统一的 REST 语义暴露任意实体的 CRUD。
type ResourceRepository interface {
	rest.Repository
}

// DataStore 是数据访问层的总入口，聚合了所有实体的仓储。
// 每个方法都接收 context，以便把当前登录用户、请求超时等信息透传到 SQL 层
// （例如按用户过滤标注数据、按可见库过滤内容）。
type DataStore interface {
	Library(ctx context.Context) LibraryRepository
	Folder(ctx context.Context) FolderRepository
	Album(ctx context.Context) AlbumRepository
	Artist(ctx context.Context) ArtistRepository
	MediaFile(ctx context.Context) MediaFileRepository
	Genre(ctx context.Context) GenreRepository
	Tag(ctx context.Context) TagRepository
	Playlist(ctx context.Context) PlaylistRepository
	PlayQueue(ctx context.Context) PlayQueueRepository
	Transcoding(ctx context.Context) TranscodingRepository
	Player(ctx context.Context) PlayerRepository
	Radio(ctx context.Context) RadioRepository
	Share(ctx context.Context) ShareRepository
	Property(ctx context.Context) PropertyRepository
	User(ctx context.Context) UserRepository
	UserProps(ctx context.Context) UserPropsRepository
	ScrobbleBuffer(ctx context.Context) ScrobbleBufferRepository
	Scrobble(ctx context.Context) ScrobbleRepository

	// Resource 返回针对任意 model 结构体的通用 REST 仓储
	Resource(ctx context.Context, model interface{}) ResourceRepository

	// WithTx 在事务中执行 block，block 内应使用传入的 tx 而非外层 DataStore。
	// scope 为可选的事务标识，仅用于日志与调试。
	WithTx(block func(tx DataStore) error, scope ...string) error
	// WithTxImmediate 与 WithTx 类似，但使用 SQLite 的 BEGIN IMMEDIATE，
	// 立即获取写锁，适用于确定会写入的场景，避免中途升级锁导致 busy 错误。
	WithTxImmediate(block func(tx DataStore) error, scope ...string) error
	// GC 执行垃圾回收：清理孤立的音轨、空专辑、无引用的艺人与标注等。
	// 不传 libraryIDs 时对全部库执行。
	GC(ctx context.Context, libraryIDs ...int) error
}
