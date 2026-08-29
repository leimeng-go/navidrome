package persistence

import (
	"database/sql/driver"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/fatih/structs"
)

// PostMapper 允许模型在结构体转换为 SQL 参数后做自定义调整，
// 例如剔除非数据库字段或补充派生列。
type PostMapper interface {
	PostMapArgs(map[string]any) error
}

// toSQLArgs 把模型结构体转换为「列名 → 值」的映射。
//
// 需要两类特殊处理：
//   - *time.Time：解引用为值，否则驱动会收到指针而无法编码
//   - driver.Valuer：调用 Value() 取得驱动可识别的底层值
//     （模型中的 JSON 字段、自定义类型多实现该接口）
//
// 最后给模型一次自定义修正的机会（PostMapper）。
func toSQLArgs(rec interface{}) (map[string]interface{}, error) {
	m := structs.Map(rec)
	for k, v := range m {
		switch t := v.(type) {
		case *time.Time:
			if t != nil {
				m[k] = *t
			}
		case driver.Valuer:
			var err error
			m[k], err = t.Value()
			if err != nil {
				return nil, err
			}
		}
	}
	if r, ok := rec.(PostMapper); ok {
		err := r.PostMapArgs(m)
		if err != nil {
			return nil, err
		}
	}
	return m, nil
}

// 两个正则配合完成驼峰转蛇形：
// 前者处理「小写/数字 + 大写开头的词」，后者处理连续大写的边界，
// 使 "MbzAlbumID" 能正确转为 "mbz_album_id"。
var matchFirstCap = regexp.MustCompile("(.)([A-Z][a-z]+)")
var matchAllCap = regexp.MustCompile("([a-z0-9])([A-Z])")

// toSnakeCase 把驼峰命名（Go 字段名）转为蛇形（数据库列名）。
func toSnakeCase(str string) string {
	snake := matchFirstCap.ReplaceAllString(str, "${1}_${2}")
	snake = matchAllCap.ReplaceAllString(snake, "${1}_${2}")
	return strings.ToLower(snake)
}

var matchUnderscore = regexp.MustCompile("_([A-Za-z])")

// toCamelCase 把蛇形转为驼峰，用于匹配以驼峰书写的排序映射键。
func toCamelCase(str string) string {
	return matchUnderscore.ReplaceAllStringFunc(str, func(s string) string {
		return strings.ToUpper(strings.Replace(s, "_", "", -1))
	})
}

// Exists 构造 EXISTS 子查询条件。
func Exists(subTable string, cond squirrel.Sqlizer) existsCond {
	return existsCond{subTable: subTable, cond: cond, not: false}
}

// NotExists 构造 NOT EXISTS 子查询条件。
func NotExists(subTable string, cond squirrel.Sqlizer) existsCond {
	return existsCond{subTable: subTable, cond: cond, not: true}
}

// existsCond 是 EXISTS/NOT EXISTS 子查询条件，实现 squirrel.Sqlizer。
// 用于「关联表中存在/不存在满足条件的行」这类判断，
// 相比 JOIN 不会产生重复行，也不需要 DISTINCT。
type existsCond struct {
	subTable string
	cond     squirrel.Sqlizer
	not      bool
}

// ToSql 生成 EXISTS 子查询片段。
func (e existsCond) ToSql() (string, []interface{}, error) {
	sql, args, err := e.cond.ToSql()
	sql = fmt.Sprintf("exists (select 1 from %s where %s)", e.subTable, sql)
	if e.not {
		sql = "not " + sql
	}
	return sql, args, err
}

var sortOrderRegex = regexp.MustCompile(`order_([a-z_]+)`)

// Convert the order_* columns to an expression using sort_* columns. Example:
// sort_album_name -> (coalesce(nullif(sort_album_name,”),order_album_name) collate nocase)
// It finds order column names anywhere in the substring
//
// mapSortOrder 把 order_* 列改写为「优先用 sort_* 标签」的表达式。
//
// sort_* 来自文件中的排序标签（如 "Beatles, The"），order_* 是系统规整出的排序名。
// 用 nullif 把空串视为 NULL，再由 coalesce 回退到 order_*，
// 从而在有排序标签时尊重标签、没有时用系统规则。
// collate nocase 保证大小写不影响排序次序。
func mapSortOrder(tableName, order string) string {
	order = strings.ToLower(order)
	repl := fmt.Sprintf("(coalesce(nullif(%[1]s.sort_$1,''),%[1]s.order_$1) collate nocase)", tableName)
	return sortOrderRegex.ReplaceAllString(order, repl)
}
