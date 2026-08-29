package persistence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// Format of a tag in the DB
// dbTag 是标签在数据库 JSON 列中的存储形式。
// 同时存 ID 与值：ID 便于按标签精确检索与关联统计，
// 值则免去每次显示都要回查 tag 表。
type dbTag struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// dbTags 是「标签名 → 标签条目列表」的映射，对应 media_file.tags 列的结构。
type dbTags map[model.TagName][]dbTag

// unmarshalTags 把数据库中的 JSON 解析为领域模型的标签集合（只保留值）。
func unmarshalTags(data string) (model.Tags, error) {
	var dbTags dbTags
	err := json.Unmarshal([]byte(data), &dbTags)
	if err != nil {
		return nil, fmt.Errorf("parsing tags: %w", err)
	}

	res := make(model.Tags, len(dbTags))
	for name, tags := range dbTags {
		res[name] = make([]string, len(tags))
		for i, tag := range tags {
			res[name][i] = tag.Value
		}
	}
	return res, nil
}

// marshalTags 把标签集合序列化为数据库 JSON，写入时顺带算出每个标签的 ID。
func marshalTags(tags model.Tags) string {
	dbTags := dbTags{}
	for name, values := range tags {
		for _, value := range values {
			t := model.NewTag(name, value)
			dbTags[name] = append(dbTags[name], dbTag{ID: t.ID, Value: value})
		}
	}
	res, _ := json.Marshal(dbTags)
	return string(res)
}

// tagIDFilter 构造「按标签 ID 过滤曲目」的条件。
//
// 标签存于 JSON 列，故用 json_tree 展开后做存在性判断。
// 过滤参数名形如 genre_id，需去掉 _id 后缀才是 JSON 中的标签名。
// atom 非空用于排除 json_tree 展开出的容器节点（对象与数组本身）。
func tagIDFilter(name string, idValue any) Sqlizer {
	name = strings.TrimSuffix(name, "_id")
	return Exists(
		fmt.Sprintf(`json_tree(tags, "$.%s")`, name),
		And{
			NotEq{"json_tree.atom": nil},
			Eq{"value": idValue},
		},
	)
}

// tagLibraryIdFilter filters tags based on library access through the library_tag table
// tagLibraryIdFilter 通过 library_tag 关联表按音乐库过滤标签。
func tagLibraryIdFilter(_ string, value interface{}) Sqlizer {
	return Eq{"library_tag.library_id": value}
}

// baseTagRepository provides common functionality for all tag-based repositories.
// It handles CRUD operations with optional filtering by tag name.
// baseTagRepository 是标签类仓储的共同基类。
// 流派（genre）等仓储本质上都是「限定了标签名的标签仓储」，
// 故抽出此基类，由 tagFilter 决定是否限定某一种标签。
type baseTagRepository struct {
	sqlRepository
	tagFilter *model.TagName // nil = no filter (all tags), non-nil = filter by specific tag name
}

// newBaseTagRepository creates a new base tag repository with optional tag filtering.
// If tagFilter is nil, the repository will work with all tags.
// If tagFilter is provided, the repository will only work with tags of that specific name.
func newBaseTagRepository(ctx context.Context, db dbx.Builder, tagFilter *model.TagName) *baseTagRepository {
	r := &baseTagRepository{
		tagFilter: tagFilter,
	}
	r.ctx = ctx
	r.db = db
	r.tableName = "tag"
	r.registerModel(&model.Tag{}, map[string]filterFunc{
		"name":       containsFilter("tag_value"),
		"library_id": tagLibraryIdFilter,
	})
	r.setSortMappings(map[string]string{
		"name": "tag_value",
	})
	return r
}

// applyLibraryFiltering adds the appropriate library joins based on user context
// applyLibraryFiltering 关联音乐库并施加访问权限。
//
// library_tag 用 LEFT JOIN：标签的计数信息按库存放，
// 即便某标签在某库中无统计也应保留该标签。
// user_library 用 INNER JOIN：这是权限过滤，无权访问的库必须被排除。
// 未登录时（如扫描器内部调用）跳过权限过滤。
func (r *baseTagRepository) applyLibraryFiltering(sq SelectBuilder) SelectBuilder {
	// Add library_tag join
	sq = sq.LeftJoin("library_tag on library_tag.tag_id = tag.id")

	// For authenticated users, also join with user_library to filter by accessible libraries
	user := loggedUser(r.ctx)
	if user.ID != invalidUserId {
		sq = sq.Join("user_library on user_library.library_id = library_tag.library_id AND user_library.user_id = ?", user.ID)
	}

	return sq
}

// newSelect overrides the base implementation to apply tag name filtering and library filtering.
// newSelect 覆盖基类实现，追加标签名过滤、库权限过滤与跨库计数聚合。
//
// 计数需要 SUM + GROUP BY：同一标签在多个音乐库中各有一份统计，
// 展示时要合并为该用户可见范围内的总数。
func (r *baseTagRepository) newSelect(options ...model.QueryOptions) SelectBuilder {
	sq := r.sqlRepository.newSelect(options...)

	// Apply tag name filtering if specified
	if r.tagFilter != nil {
		sq = sq.Where(Eq{"tag.tag_name": *r.tagFilter})
	}

	// Apply library filtering and set up aggregation columns
	sq = r.applyLibraryFiltering(sq).Columns(
		"tag.id",
		"tag.tag_name",
		"tag.tag_value",
		"COALESCE(SUM(library_tag.album_count), 0) as album_count",
		"COALESCE(SUM(library_tag.media_file_count), 0) as song_count",
	).GroupBy("tag.id", "tag.tag_name", "tag.tag_value")

	return sq
}

// ResourceRepository interface implementation
// 以下实现 model.ResourceRepository，供通用 REST 层调用。

// Count 统计标签数。用 COUNT(DISTINCT) 消除 JOIN 产生的重复行。
func (r *baseTagRepository) Count(options ...rest.QueryOptions) (int64, error) {
	sq := Select("COUNT(DISTINCT tag.id)").From("tag")

	// Apply tag name filtering if specified
	if r.tagFilter != nil {
		sq = sq.Where(Eq{"tag.tag_name": *r.tagFilter})
	}

	// Apply library filtering
	sq = r.applyLibraryFiltering(sq)

	return r.count(sq, r.parseRestOptions(r.ctx, options...))
}

// Read 按 ID 读取单个标签。
func (r *baseTagRepository) Read(id string) (interface{}, error) {
	query := r.newSelect().Where(Eq{"id": id})
	var res model.Tag
	err := r.queryOne(query, &res)
	return &res, err
}

// ReadAll 按查询条件读取标签列表。
func (r *baseTagRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	query := r.newSelect(r.parseRestOptions(r.ctx, options...))
	var res model.TagList
	err := r.queryAll(query, &res)
	return res, err
}

// EntityName 返回资源名，供 REST 层构造响应。
func (r *baseTagRepository) EntityName() string {
	return "tag"
}

// NewInstance 返回空实例，供 REST 层反序列化请求体。
func (r *baseTagRepository) NewInstance() interface{} {
	return model.Tag{}
}

// Interface compliance check
// 编译期接口实现检查
var _ model.ResourceRepository = (*baseTagRepository)(nil)
