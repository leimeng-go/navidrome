package persistence

import (
	"context"
	"fmt"
	"slices"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// tagRepository 是标签仓储，覆盖所有标签类型（流派、情绪、厂牌等）。
// 通用查询能力由 baseTagRepository 提供，本类型只补充写入与维护逻辑。
type tagRepository struct {
	*baseTagRepository
}

// NewTagRepository 创建不限标签类型的标签仓储。
func NewTagRepository(ctx context.Context, db dbx.Builder) model.TagRepository {
	return &tagRepository{
		baseTagRepository: newBaseTagRepository(ctx, db, nil), // nil = no filter, works with all tags
	}
}

// Add 批量写入标签，并建立与音乐库的关联。
// 标签 ID 由名称与取值哈希而来，故重复写入用 on conflict do nothing 即可幂等。
// 计数先置 0，后续由 UpdateCounts 统一重算。
// 每 200 条一批，规避 SQLite 参数数量上限。
func (r *tagRepository) Add(libraryID int, tags ...model.Tag) error {
	for chunk := range slices.Chunk(tags, 200) {
		sq := Insert(r.tableName).Columns("id", "tag_name", "tag_value").
			Suffix("on conflict (id) do nothing")
		for _, t := range chunk {
			sq = sq.Values(t.ID, t.TagName, t.TagValue)
		}
		_, err := r.executeSQL(sq)
		if err != nil {
			return err
		}

		// Create library_tag entries for library filtering
		libSq := Insert("library_tag").Columns("tag_id", "library_id", "album_count", "media_file_count").
			Suffix("on conflict (tag_id, library_id) do nothing")
		for _, t := range chunk {
			libSq = libSq.Values(t.ID, libraryID, 0, 0)
		}
		_, err = r.executeSQL(libSq)
		if err != nil {
			return fmt.Errorf("adding library_tag entries: %w", err)
		}
	}
	return nil
}

// UpdateCounts updates the library_tag table with per-library statistics.
// Only genres are being updated for now.
//
// UpdateCounts 重算标签在各音乐库中的专辑数与曲目数。
// 目前只统计流派——其他标签类型暂无按数量浏览的需求。
//
// SQL 模板对 album 与 media_file 两张表复用：
// 用 json_tree 展开 tags 列中 $.genre 下的标签 ID
// （key = 'id' 用于只取 ID 节点），按「标签 × 库」分组计数后 upsert。
func (r *tagRepository) UpdateCounts() error {
	template := `
INSERT INTO library_tag (tag_id, library_id, %[1]s_count)
SELECT jt.value as tag_id, %[1]s.library_id, count(distinct %[1]s.id) as %[1]s_count
FROM %[1]s
JOIN json_tree(%[1]s.tags, '$.genre') as jt ON jt.atom IS NOT NULL AND jt.key = 'id'
JOIN tag ON tag.id = jt.value
GROUP BY jt.value, %[1]s.library_id
ON CONFLICT (tag_id, library_id) 
DO UPDATE SET %[1]s_count = excluded.%[1]s_count;
`

	for _, table := range []string{"album", "media_file"} {
		start := time.Now()
		query := Expr(fmt.Sprintf(template, table))
		c, err := r.executeSQL(query)
		log.Debug(r.ctx, "Updated library tag counts", "table", table, "elapsed", time.Since(start), "updated", c)
		if err != nil {
			return fmt.Errorf("updating %s library tag counts: %w", table, err)
		}
	}
	return nil
}

// purgeUnused 删除不再被任何专辑或曲目引用的标签，由 GC 调用。
// 这里展开 tags 的整棵 JSON 树（'$' 而非 '$.genre'），
// 因为要保留的是所有类型的在用标签。
func (r *tagRepository) purgeUnused() error {
	del := Delete(r.tableName).Where(`	
	id not in (select jt.value
	from album left join json_tree(album.tags, '$') as jt
	where atom is not null
	  and key = 'id'
	UNION 
	select jt.value
	from media_file left join json_tree(media_file.tags, '$') as jt
	where atom is not null
	  and key = 'id')
`)
	c, err := r.executeSQL(del)
	if err != nil {
		return fmt.Errorf("error purging %s unused tags: %w", r.tableName, err)
	}
	if c > 0 {
		log.Debug(r.ctx, "Purged unused tags", "totalDeleted", c, "table", r.tableName)
	}
	return err
}

var _ model.ResourceRepository = &tagRepository{}
