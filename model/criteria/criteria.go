// Package criteria implements a Criteria API based on Masterminds/squirrel
// Package criteria 实现智能播放列表的查询 DSL。
//
// 它基于 Masterminds/squirrel 构建：规则以 JSON 形式存储（.nsp 文件或数据库），
// 反序列化为表达式树后，通过 squirrel 的 Sqlizer 接口翻译为 SQL 的 WHERE 子句。
// 整个包的核心抽象就是 Expression（即 squirrel.Sqlizer），
// 所有操作符（Is/Contains/Gt/InTheLast 等）都实现该接口，从而可自由嵌套组合。
package criteria

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
)

// Expression 是可翻译为 SQL 片段的表达式，直接复用 squirrel 的接口。
type Expression = squirrel.Sqlizer

// Criteria 是一条完整的智能播放列表规则：过滤条件 + 排序 + 分页。
// 内嵌的 Expression 通常是 All 或 Any 这类连接词，其下可继续嵌套子表达式。
type Criteria struct {
	Expression
	Sort   string // 排序字段，支持逗号分隔多字段，字段前可加 +/- 指定方向
	Order  string // 全局排序方向：asc 或 desc
	Limit  int
	Offset int
}

// OrderBy 把 Sort/Order 翻译为 SQL 的 ORDER BY 子句内容。
//
// 支持三类字段，映射方式各不相同：
//   - 普通字段：直接用列名，或用 fieldMap 中预置的 order 表达式
//   - 标签字段（isTag）：用 json_extract 从 media_file.tags JSON 列中取值
//   - 角色字段（isRole）：用 json_extract 从 participants JSON 列中取艺人名
//
// 方向解析有两层：字段级前缀（+/-）给出默认方向，
// 全局 Order 为 desc 时再对每个字段的方向取反，
// 从而让全局设置在多字段排序中也能一致生效。
// 非法字段与非法 order 值只记录日志并跳过，不使整条规则失效。
func (c Criteria) OrderBy() string {
	if c.Sort == "" {
		c.Sort = "title"
	}

	order := strings.ToLower(strings.TrimSpace(c.Order))
	if order != "" && order != "asc" && order != "desc" {
		log.Error("Invalid value in 'order' field. Valid values: 'asc', 'desc'", "order", c.Order)
		order = ""
	}

	parts := strings.Split(c.Sort, ",")
	fields := make([]string, 0, len(parts))

	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		// 字段级方向前缀：+ 为升序（默认），- 为降序
		dir := "asc"
		if strings.HasPrefix(p, "+") || strings.HasPrefix(p, "-") {
			if strings.HasPrefix(p, "-") {
				dir = "desc"
			}
			p = strings.TrimSpace(p[1:])
		}

		sortField := strings.ToLower(p)
		f := fieldMap[sortField]
		if f == nil {
			log.Error("Invalid field in 'sort' field", "sort", sortField)
			continue
		}

		var mapped string

		if f.order != "" {
			mapped = f.order
		} else if f.isTag {
			// Use the actual field name (handles aliases like albumtype -> releasetype)
			// 用实际字段名而非用户输入，以正确处理别名（如 albumtype → releasetype）。
			// COALESCE 兜底空串，避免 NULL 参与排序导致次序不确定
			tagName := sortField
			if f.field != "" {
				tagName = f.field
			}
			mapped = "COALESCE(json_extract(media_file.tags, '$." + tagName + "[0].value'), '')"
		} else if f.isRole {
			mapped = "COALESCE(json_extract(media_file.participants, '$." + sortField + "[0].name'), '')"
		} else {
			mapped = f.field
		}
		if f.numeric {
			// JSON 提取出的值是文本，数值字段需显式转换，否则会按字典序排序
			mapped = fmt.Sprintf("CAST(%s AS REAL)", mapped)
		}
		// If the global 'order' field is set to 'desc', reverse the default or field-specific sort direction.
		// This ensures that the global order applies consistently across all fields.
		// 全局 order 为 desc 时反转每个字段的方向，
		// 使全局设置对所有字段（含带 +/- 前缀的）都一致生效
		if order == "desc" {
			if dir == "asc" {
				dir = "desc"
			} else {
				dir = "asc"
			}
		}

		fields = append(fields, mapped+" "+dir)
	}

	return strings.Join(fields, ", ")
}

// ToSql 实现 squirrel.Sqlizer，把过滤条件翻译为 SQL 片段与参数。
func (c Criteria) ToSql() (sql string, args []any, err error) {
	return c.Expression.ToSql()
}

// ChildPlaylistIds 返回规则中引用到的其他播放列表 ID。
// 智能列表可以「包含某个播放列表」，因此需要收集这些依赖，
// 用于检测循环引用以及在被引用列表变化时触发重新求值。
func (c Criteria) ChildPlaylistIds() []string {
	if c.Expression == nil {
		return nil
	}

	if parent := c.Expression.(interface{ ChildPlaylistIds() (ids []string) }); parent != nil {
		return parent.ChildPlaylistIds()
	}

	return nil
}

// MarshalJSON 序列化规则。表达式树的根必须是 All 或 Any，
// 若根节点是单个操作符，则包装成 All 以保证输出格式统一。
func (c Criteria) MarshalJSON() ([]byte, error) {
	aux := struct {
		All    []Expression `json:"all,omitempty"`
		Any    []Expression `json:"any,omitempty"`
		Sort   string       `json:"sort,omitempty"`
		Order  string       `json:"order,omitempty"`
		Limit  int          `json:"limit,omitempty"`
		Offset int          `json:"offset,omitempty"`
	}{
		Sort:   c.Sort,
		Order:  c.Order,
		Limit:  c.Limit,
		Offset: c.Offset,
	}
	switch rules := c.Expression.(type) {
	case Any:
		aux.Any = rules
	case All:
		aux.All = rules
	default:
		aux.All = All{rules}
	}
	return json.Marshal(aux)
}

// UnmarshalJSON 反序列化规则。根节点必须提供 "all" 或 "any" 之一，
// 二者都缺失说明规则无效；同时给出时以 "any" 优先。
func (c *Criteria) UnmarshalJSON(data []byte) error {
	var aux struct {
		All    unmarshalConjunctionType `json:"all"`
		Any    unmarshalConjunctionType `json:"any"`
		Sort   string                   `json:"sort"`
		Order  string                   `json:"order"`
		Limit  int                      `json:"limit"`
		Offset int                      `json:"offset"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if len(aux.Any) > 0 {
		c.Expression = Any(aux.Any)
	} else if len(aux.All) > 0 {
		c.Expression = All(aux.All)
	} else {
		return errors.New("invalid criteria json. missing rules (key 'all' or 'any')")
	}
	c.Sort = aux.Sort
	c.Order = aux.Order
	c.Limit = aux.Limit
	c.Offset = aux.Offset
	return nil
}
