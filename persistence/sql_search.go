package persistence

import (
	"strings"

	. "github.com/Masterminds/squirrel"
	"github.com/google/uuid"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/str"
)

// 本文件实现基于 full_text 冗余列的搜索。
//
// 各主表都有一个 full_text 列，存放该记录所有可搜索字段规整后的拼接结果
// （小写、去音调符号、去标点）。搜索时对该列做 LIKE 匹配，
// 避免跨多列 OR 查询，也让「Beyoncé」能被「beyonce」搜到。

// formatFullText 生成 full_text 列的值。
// 前导空格是关键：配合 fullTextExpr 中的 "% 词%" 模式，
// 可实现「词首匹配」——即匹配任意单词的开头，而非任意位置的子串。
func formatFullText(text ...string) string {
	fullText := str.SanitizeStrings(text...)
	return " " + fullText
}

// doSearch performs a full-text search with the specified parameters.
// The naturalOrder is used to sort results when no full-text filter is applied. It is useful for cases like
// OpenSubsonic, where an empty search query should return all results in a natural order. Normally the parameter
// should be `tableName + ".rowid"`, but some repositories (ex: artist) may use a different natural order.
//
// doSearch 执行全文搜索。
//
// 关键词不足 2 字符时直接返回空：单字符匹配几乎等于全表扫描，
// 且结果无参考价值。
//
// 关键词为空（清理后无有效内容）时按 naturalOrder 排序返回全部——
// OpenSubsonic 的 search3 允许空查询，此时按自然序（通常是 rowid）
// 比按相关度排序快得多。
//
// 最后统一排除 missing 记录：这些是文件已不存在但保留了用户数据的条目，
// 不应出现在搜索结果中。
func (r sqlRepository) doSearch(sq SelectBuilder, q string, offset, size int, results any, naturalOrder string, orderBys ...string) error {
	q = strings.TrimSpace(q)
	// 部分客户端会自动追加 *（通配符习惯），此处剥离
	q = strings.TrimSuffix(q, "*")
	if len(q) < 2 {
		return nil
	}

	filter := fullTextExpr(r.tableName, q)
	if filter != nil {
		sq = sq.Where(filter)
		sq = sq.OrderBy(orderBys...)
	} else {
		// This is to speed up the results of `search3?query=""`, for OpenSubsonic
		// If the filter is empty, we sort by the specified natural order.
		sq = sq.OrderBy(naturalOrder)
	}
	sq = sq.Where(Eq{r.tableName + ".missing": false})
	sq = sq.Limit(uint64(size)).Offset(uint64(offset))
	return r.queryAll(sq, results, model.QueryOptions{Offset: offset})
}

// searchByMBID 按 MusicBrainz ID 精确查找。
func (r sqlRepository) searchByMBID(sq SelectBuilder, mbid string, mbidFields []string, results any) error {
	sq = sq.Where(mbidExpr(r.tableName, mbid, mbidFields...))
	sq = sq.Where(Eq{r.tableName + ".missing": false})

	return r.queryAll(sq, results)
}

// mbidExpr 构造按 MBID 匹配的条件，可跨多个 MBID 列（OR 关系）。
// 关键词不是合法 UUID 时返回 nil，交由调用方回退到全文搜索。
func mbidExpr(tableName, mbid string, mbidFields ...string) Sqlizer {
	if uuid.Validate(mbid) != nil || len(mbidFields) == 0 {
		return nil
	}
	mbid = strings.ToLower(mbid)
	var cond []Sqlizer
	for _, mbidField := range mbidFields {
		cond = append(cond, Eq{tableName + "." + mbidField: mbid})
	}
	return Or(cond)
}

// fullTextExpr 构造全文匹配条件：关键词按空格拆分，要求全部命中（AND），
// 从而支持词序无关的多词搜索。
//
// 默认在每个词前加空格（"% 词%"），即只匹配单词开头；
// 开启 SearchFullString 后去掉该空格，改为任意位置的子串匹配，
// 这对中日韩等不以空格分词的语言是必需的。
func fullTextExpr(tableName string, s string) Sqlizer {
	q := str.SanitizeStrings(s)
	if q == "" {
		return nil
	}
	var sep string
	if !conf.Server.SearchFullString {
		sep = " "
	}
	parts := strings.Split(q, " ")
	filters := And{}
	for _, part := range parts {
		filters = append(filters, Like{tableName + ".full_text": "%" + sep + part + "%"})
	}
	return filters
}
