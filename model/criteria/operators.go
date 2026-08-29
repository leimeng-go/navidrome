package criteria

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/Masterminds/squirrel"
)

// 本文件定义智能播放列表的全部操作符。每个操作符都是 map 类型并实现
// squirrel.Sqlizer（ToSql）与 json.Marshaler（MarshalJSON），因此既能翻译为 SQL，
// 又能与 JSON 规则互转。
//
// 共同模式：ToSql 中先判断字段是否为角色/标签，若是则改写为 JSON 子查询
// （见 fields.go 的 mapRoleExpr/mapTagExpr），否则走普通列比较。

// All / And 是逻辑与连接词，其下所有子表达式都必须满足。
type (
	All squirrel.And
	And = All
)

func (all All) ToSql() (sql string, args []any, err error) {
	return squirrel.And(all).ToSql()
}

func (all All) MarshalJSON() ([]byte, error) {
	return marshalConjunction("all", all)
}

// ChildPlaylistIds 递归收集子表达式中引用的播放列表 ID。
func (all All) ChildPlaylistIds() (ids []string) {
	return extractPlaylistIds(all)
}

// Any / Or 是逻辑或连接词，其下任一子表达式满足即可。
type (
	Any squirrel.Or
	Or  = Any
)

func (any Any) ToSql() (sql string, args []any, err error) {
	return squirrel.Or(any).ToSql()
}

func (any Any) MarshalJSON() ([]byte, error) {
	return marshalConjunction("any", any)
}

// ChildPlaylistIds 递归收集子表达式中引用的播放列表 ID。
func (any Any) ChildPlaylistIds() (ids []string) {
	return extractPlaylistIds(any)
}

// Is / Eq 是相等比较。
type Is squirrel.Eq
type Eq = Is

func (is Is) ToSql() (sql string, args []any, err error) {
	if isRoleExpr(is) {
		return mapRoleExpr(is, false).ToSql()
	}
	if isTagExpr(is) {
		return mapTagExpr(is, false).ToSql()
	}
	return squirrel.Eq(mapFields(is)).ToSql()
}

func (is Is) MarshalJSON() ([]byte, error) {
	return marshalExpression("is", is)
}

// IsNot 是不等比较。
// 注意角色/标签场景下传入的是 Eq 并置 negate=true，而非 NotEq：
// 因为 JSON 子查询要表达的是「不存在满足相等条件的值」，
// 而 NotEq 会变成「存在不相等的值」，语义完全不同。
type IsNot squirrel.NotEq

func (in IsNot) ToSql() (sql string, args []any, err error) {
	if isRoleExpr(in) {
		return mapRoleExpr(squirrel.Eq(in), true).ToSql()
	}
	if isTagExpr(in) {
		return mapTagExpr(squirrel.Eq(in), true).ToSql()
	}
	return squirrel.NotEq(mapFields(in)).ToSql()
}

func (in IsNot) MarshalJSON() ([]byte, error) {
	return marshalExpression("isNot", in)
}

// Gt 是大于比较。标签场景下会转为 JSON 子查询，数值标签自动 CAST。
type Gt squirrel.Gt

func (gt Gt) ToSql() (sql string, args []any, err error) {
	if isTagExpr(gt) {
		return mapTagExpr(gt, false).ToSql()
	}
	return squirrel.Gt(mapFields(gt)).ToSql()
}

func (gt Gt) MarshalJSON() ([]byte, error) {
	return marshalExpression("gt", gt)
}

// Lt 是小于比较。
type Lt squirrel.Lt

func (lt Lt) ToSql() (sql string, args []any, err error) {
	if isTagExpr(lt) {
		return mapTagExpr(squirrel.Lt(lt), false).ToSql()
	}
	return squirrel.Lt(mapFields(lt)).ToSql()
}

func (lt Lt) MarshalJSON() ([]byte, error) {
	return marshalExpression("lt", lt)
}

// Before 是日期早于比较，语义上等同 Lt，
// 单独定义只为在 JSON 规则中保留更贴合日期语义的操作符名。
type Before squirrel.Lt

func (bf Before) ToSql() (sql string, args []any, err error) {
	return Lt(bf).ToSql()
}

func (bf Before) MarshalJSON() ([]byte, error) {
	return marshalExpression("before", bf)
}

// After 是日期晚于比较，语义上等同 Gt。
type After Gt

func (af After) ToSql() (sql string, args []any, err error) {
	return Gt(af).ToSql()
}

func (af After) MarshalJSON() ([]byte, error) {
	return marshalExpression("after", af)
}

// Contains 是子串匹配，生成 LIKE '%值%'。
type Contains map[string]any

func (ct Contains) ToSql() (sql string, args []any, err error) {
	lk := squirrel.Like{}
	for f, v := range mapFields(ct) {
		lk[f] = fmt.Sprintf("%%%s%%", v)
	}
	if isRoleExpr(ct) {
		return mapRoleExpr(lk, false).ToSql()
	}
	if isTagExpr(ct) {
		return mapTagExpr(lk, false).ToSql()
	}
	return lk.ToSql()
}

func (ct Contains) MarshalJSON() ([]byte, error) {
	return marshalExpression("contains", ct)
}

// NotContains 是子串不匹配。
// 与 IsNot 同理，角色/标签场景传入 Like 并 negate=true，
// 表达「不存在包含该子串的值」，而不是「存在不包含的值」。
type NotContains map[string]any

func (nct NotContains) ToSql() (sql string, args []any, err error) {
	lk := squirrel.NotLike{}
	for f, v := range mapFields(nct) {
		lk[f] = fmt.Sprintf("%%%s%%", v)
	}
	if isRoleExpr(nct) {
		return mapRoleExpr(squirrel.Like(lk), true).ToSql()
	}
	if isTagExpr(nct) {
		return mapTagExpr(squirrel.Like(lk), true).ToSql()
	}
	return lk.ToSql()
}

func (nct NotContains) MarshalJSON() ([]byte, error) {
	return marshalExpression("notContains", nct)
}

// StartsWith 是前缀匹配，生成 LIKE '值%'。
type StartsWith map[string]any

func (sw StartsWith) ToSql() (sql string, args []any, err error) {
	lk := squirrel.Like{}
	for f, v := range mapFields(sw) {
		lk[f] = fmt.Sprintf("%s%%", v)
	}
	if isRoleExpr(sw) {
		return mapRoleExpr(lk, false).ToSql()
	}
	if isTagExpr(sw) {
		return mapTagExpr(lk, false).ToSql()
	}
	return lk.ToSql()
}

func (sw StartsWith) MarshalJSON() ([]byte, error) {
	return marshalExpression("startsWith", sw)
}

// EndsWith 是后缀匹配，生成 LIKE '%值'。
type EndsWith map[string]any

func (sw EndsWith) ToSql() (sql string, args []any, err error) {
	lk := squirrel.Like{}
	for f, v := range mapFields(sw) {
		lk[f] = fmt.Sprintf("%%%s", v)
	}
	if isRoleExpr(sw) {
		return mapRoleExpr(lk, false).ToSql()
	}
	if isTagExpr(sw) {
		return mapTagExpr(lk, false).ToSql()
	}
	return lk.ToSql()
}

func (sw EndsWith) MarshalJSON() ([]byte, error) {
	return marshalExpression("endsWith", sw)
}

// InTheRange 是闭区间匹配，值必须是长度为 2 的切片 [下限, 上限]。
// 展开为 field >= 下限 AND field <= 上限。
type InTheRange map[string]any

func (itr InTheRange) ToSql() (sql string, args []any, err error) {
	and := squirrel.And{}
	for f, v := range mapFields(itr) {
		// 用反射校验值确实是二元切片，JSON 反序列化后类型不定故不能直接断言
		s := reflect.ValueOf(v)
		if s.Kind() != reflect.Slice || s.Len() != 2 {
			return "", nil, fmt.Errorf("invalid range for 'in' operator: %s", v)
		}
		and = append(and,
			squirrel.GtOrEq{f: s.Index(0).Interface()},
			squirrel.LtOrEq{f: s.Index(1).Interface()},
		)
	}
	return and.ToSql()
}

func (itr InTheRange) MarshalJSON() ([]byte, error) {
	return marshalExpression("inTheRange", itr)
}

// InTheLast 匹配最近 N 天内的日期字段，值为天数。
type InTheLast map[string]any

func (itl InTheLast) ToSql() (sql string, args []any, err error) {
	exp, err := inPeriod(itl, false)
	if err != nil {
		return "", nil, err
	}
	return exp.ToSql()
}

func (itl InTheLast) MarshalJSON() ([]byte, error) {
	return marshalExpression("inTheLast", itl)
}

// NotInTheLast 匹配最近 N 天之外的日期字段。
type NotInTheLast map[string]any

func (nitl NotInTheLast) ToSql() (sql string, args []any, err error) {
	exp, err := inPeriod(nitl, true)
	if err != nil {
		return "", nil, err
	}
	return exp.ToSql()
}

func (nitl NotInTheLast) MarshalJSON() ([]byte, error) {
	return marshalExpression("notInTheLast", nitl)
}

// inPeriod 构造「最近 N 天内/外」的条件。
// 天数经 JSON 反序列化后可能是数字或字符串，故先格式化再解析为整数。
//
// 取反时不能简单写成 NOT (field > 起始日)：
// 从未播放的记录该字段为 NULL，而 SQL 中 NULL 参与比较结果为 NULL 而非 true，
// 会被 WHERE 过滤掉。因此显式加上 field IS NULL 分支，
// 让「从未播放过」也算作「不在最近 N 天内」。
func inPeriod(m map[string]any, negate bool) (Expression, error) {
	var field string
	var value any
	for f, v := range mapFields(m) {
		field, value = f, v
		break
	}
	str := fmt.Sprintf("%v", value)
	v, err := strconv.ParseInt(str, 10, 64)
	if err != nil {
		return nil, err
	}
	firstDate := startOfPeriod(v, time.Now())

	if negate {
		return Or{
			squirrel.Lt{field: firstDate},
			squirrel.Eq{field: nil},
		}, nil
	}
	return squirrel.Gt{field: firstDate}, nil
}

// startOfPeriod 返回 from 之前 numDays 天的日期字符串（YYYY-MM-DD）。
// 只保留日期部分，使区间边界落在当天零点，避免因时刻差异导致结果不稳定。
func startOfPeriod(numDays int64, from time.Time) string {
	return from.Add(time.Duration(-24*numDays) * time.Hour).Format("2006-01-02")
}

// InPlaylist 匹配包含在指定播放列表中的曲目。
type InPlaylist map[string]any

func (ipl InPlaylist) ToSql() (sql string, args []any, err error) {
	return inList(ipl, false)
}

func (ipl InPlaylist) MarshalJSON() ([]byte, error) {
	return marshalExpression("inPlaylist", ipl)
}

// NotInPlaylist 匹配不在指定播放列表中的曲目。
type NotInPlaylist map[string]any

func (ipl NotInPlaylist) ToSql() (sql string, args []any, err error) {
	return inList(ipl, true)
}

func (ipl NotInPlaylist) MarshalJSON() ([]byte, error) {
	return marshalExpression("notInPlaylist", ipl)
}

// inList 生成基于播放列表成员关系的 IN/NOT IN 子查询。
//
// 安全约束：子查询强制要求 playlist.public = 1。
// 智能列表可能被其他用户查看，若允许引用私有列表会造成信息泄露，
// 因此非公开列表在此处求值为空集。
func inList(m map[string]any, negate bool) (sql string, args []any, err error) {
	var playlistid string
	var ok bool
	if playlistid, ok = m["id"].(string); !ok {
		return "", nil, errors.New("playlist id not given")
	}

	// Subquery to fetch all media files that are contained in given playlist
	// Only evaluate playlist if it is public
	// 子查询取出该播放列表下的所有曲目 ID；
	// PlaceholderFormat(Question) 确保占位符风格与外层 SQL 一致
	subQuery := squirrel.Select("media_file_id").
		From("playlist_tracks pl").
		LeftJoin("playlist on pl.playlist_id = playlist.id").
		Where(squirrel.And{
			squirrel.Eq{"pl.playlist_id": playlistid},
			squirrel.Eq{"playlist.public": 1}})
	subQText, subQArgs, err := subQuery.PlaceholderFormat(squirrel.Question).ToSql()

	if err != nil {
		return "", nil, err
	}
	if negate {
		return "media_file.id NOT IN (" + subQText + ")", subQArgs, nil
	} else {
		return "media_file.id IN (" + subQText + ")", subQArgs, nil
	}
}

// extractPlaylistIds 递归遍历表达式树，收集所有被引用的播放列表 ID。
// 用于循环引用检测，以及在被引用列表变更时触发依赖它的智能列表刷新。
func extractPlaylistIds(inputRule any) (ids []string) {
	var id string
	var ok bool

	switch rule := inputRule.(type) {
	case Any:
		for _, rules := range rule {
			ids = append(ids, extractPlaylistIds(rules)...)
		}
	case All:
		for _, rules := range rule {
			ids = append(ids, extractPlaylistIds(rules)...)
		}
	case InPlaylist:
		if id, ok = rule["id"].(string); ok {
			ids = append(ids, id)
		}
	case NotInPlaylist:
		if id, ok = rule["id"].(string); ok {
			ids = append(ids, id)
		}
	}

	return
}
