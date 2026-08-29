package criteria

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/log"
)

// fieldMap 是智能播放列表字段名到数据库列的映射表。
// 键统一为小写。除这里预置的固定字段外，运行时还会由
// AddRoles/AddTagNames/AddNumericTags 动态注入角色与标签字段（见 model 包的 init 钩子）。
var fieldMap = map[string]*mappedField{
	"title":       {field: "media_file.title"},
	"album":       {field: "media_file.album"},
	"hascoverart": {field: "media_file.has_cover_art"},
	"tracknumber": {field: "media_file.track_number"},
	"discnumber":  {field: "media_file.disc_number"},
	"year":        {field: "media_file.year"},
	"date":        {field: "media_file.date", alias: "recordingdate"}, "originalyear": {field: "media_file.original_year"},
	"originaldate":    {field: "media_file.original_date"},
	"releaseyear":     {field: "media_file.release_year"},
	"releasedate":     {field: "media_file.release_date"},
	"size":            {field: "media_file.size"},
	"compilation":     {field: "media_file.compilation"},
	"dateadded":       {field: "media_file.created_at"},
	"datemodified":    {field: "media_file.updated_at"},
	"discsubtitle":    {field: "media_file.disc_subtitle"},
	"comment":         {field: "media_file.comment"},
	"lyrics":          {field: "media_file.lyrics"},
	"sorttitle":       {field: "media_file.sort_title"},
	"sortalbum":       {field: "media_file.sort_album_name"},
	"sortartist":      {field: "media_file.sort_artist_name"},
	"sortalbumartist": {field: "media_file.sort_album_artist_name"},
	"albumcomment":    {field: "media_file.mbz_album_comment"},
	"catalognumber":   {field: "media_file.catalog_num"},
	"filepath":        {field: "media_file.path"},
	"filetype":        {field: "media_file.suffix"},
	"duration":        {field: "media_file.duration"},
	"bitrate":         {field: "media_file.bit_rate"},
	"bitdepth":        {field: "media_file.bit_depth"},
	"bpm":             {field: "media_file.bpm"},
	"channels":        {field: "media_file.channels"},
	// 以下字段来自 annotation 表（用户标注）。用 COALESCE 兜默认值，
	// 使从未播放/评分过的曲目也能参与比较，而不会因 NULL 被过滤掉
	"loved":                {field: "COALESCE(annotation.starred, false)"},
	"dateloved":            {field: "annotation.starred_at"},
	"lastplayed":           {field: "annotation.play_date"},
	"daterated":            {field: "annotation.rated_at"},
	"playcount":            {field: "COALESCE(annotation.play_count, 0)"},
	"rating":               {field: "COALESCE(annotation.rating, 0)"},
	"mbz_album_id":         {field: "media_file.mbz_album_id"},
	"mbz_album_artist_id":  {field: "media_file.mbz_album_artist_id"},
	"mbz_artist_id":        {field: "media_file.mbz_artist_id"},
	"mbz_recording_id":     {field: "media_file.mbz_recording_id"},
	"mbz_release_track_id": {field: "media_file.mbz_release_track_id"},
	"mbz_release_group_id": {field: "media_file.mbz_release_group_id"},
	"library_id":           {field: "media_file.library_id", numeric: true},

	// Backward compatibility: albumtype is an alias for releasetype tag
	"albumtype": {field: "releasetype", isTag: true},

	// special fields
	// 伪字段：不对应真实列
	"random": {field: "", order: "random()"}, // pseudo-field for random sorting
	"value":  {field: "value"},               // pseudo-field for tag and roles values
	// random 仅用于排序；value 是标签/角色表达式内部的占位列名，见 mapExpr
}

// mappedField 描述一个可用于智能播放列表的字段如何映射到 SQL。
type mappedField struct {
	field   string // 数据库列名或 SQL 表达式
	order   string // 排序时使用的专用表达式，非空则优先于 field
	isRole  bool   // true if the field is a role (e.g. "artist", "composer", "conductor", etc.)
	isTag   bool   // true if the field is a tag imported from the file metadata
	alias   string // name from `mappings.yml` that may differ from the name used in the smart playlist
	numeric bool   // true if the field/tag should be treated as numeric
	// isRole/isTag 决定生成 JSON 查询而非普通列比较；
	// alias 用于兼容 mappings.yaml 中与播放列表字段名不一致的情况；
	// numeric 会让比较与排序前加 CAST，避免文本按字典序比较
}

// mapFields 把用户书写的字段名替换为数据库列名。
// 无法识别的字段只记录错误并丢弃，不会中断整条规则的求值。
func mapFields(expr map[string]any) map[string]any {
	m := make(map[string]any)
	for f, v := range expr {
		if dbf := fieldMap[strings.ToLower(f)]; dbf != nil && dbf.field != "" {
			m[dbf.field] = v
		} else {
			log.Error("Invalid field in criteria", "field", f)
		}
	}
	return m
}

// mapExpr maps a normal field expression to a specific type of expression (tag or role).
// This is required because tags are handled differently than other fields,
// as they are stored as a JSON column in the database.
// mapExpr 把普通字段表达式改写为标签/角色专用表达式。
// 之所以需要改写，是因为标签与参与者存放在 JSON 列中，
// 无法像普通列那样直接比较，必须生成 json_tree 子查询。
//
// 实现上用反射就地改写 map：把原来的「字段名 → 值」替换为「"value" → 值」，
// 使 squirrel 生成的 SQL 片段里列名变成 value，
// 从而可嵌入 json_tree 子查询（子查询中 value 是 json_tree 提供的列）。
// 原字段名则作为标签名/角色名返回给外层构造子查询。
// 注意只取第一个键：每个操作符表达式按约定只含单个字段。
func mapExpr(expr squirrel.Sqlizer, negate bool, exprFunc func(string, squirrel.Sqlizer, bool) squirrel.Sqlizer) squirrel.Sqlizer {
	rv := reflect.ValueOf(expr)
	if rv.Kind() != reflect.Map || rv.Type().Key().Kind() != reflect.String {
		log.Fatal(fmt.Sprintf("expr is not a map-based operator: %T", expr))
	}

	// Extract into a generic map
	var k string
	m := make(map[string]any, rv.Len())
	for _, key := range rv.MapKeys() {
		// Save the key to build the expression, and use the provided keyName as the key
		k = key.String()
		m["value"] = rv.MapIndex(key).Interface()
		break // only one key is expected (and supported)
	}

	// Clear the original map
	for _, key := range rv.MapKeys() {
		rv.SetMapIndex(key, reflect.Value{})
	}

	// Write the updated map back into the original variable
	for key, val := range m {
		rv.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(val))
	}

	return exprFunc(k, expr, negate)
}

// mapTagExpr maps a normal field expression to a tag expression.
// mapTagExpr 把普通字段表达式改写为标签表达式。
func mapTagExpr(expr squirrel.Sqlizer, negate bool) squirrel.Sqlizer {
	return mapExpr(expr, negate, tagExpr)
}

// mapRoleExpr maps a normal field expression to an artist role expression.
// mapRoleExpr 把普通字段表达式改写为艺人角色表达式。
func mapRoleExpr(expr squirrel.Sqlizer, negate bool) squirrel.Sqlizer {
	return mapExpr(expr, negate, roleExpr)
}

// isTagExpr 判断表达式的字段是否为标签字段。
func isTagExpr(expr map[string]any) bool {
	for f := range expr {
		if f2, ok := fieldMap[strings.ToLower(f)]; ok && f2.isTag {
			return true
		}
	}
	return false
}

// isRoleExpr 判断表达式的字段是否为角色字段。
func isRoleExpr(expr map[string]any) bool {
	for f := range expr {
		if f2, ok := fieldMap[strings.ToLower(f)]; ok && f2.isRole {
			return true
		}
	}
	return false
}

// tagExpr 构造标签条件表达式。
func tagExpr(tag string, cond squirrel.Sqlizer, negate bool) squirrel.Sqlizer {
	return tagCond{tag: tag, cond: cond, not: negate}
}

// tagCond 是针对 tags JSON 列的条件，实现 squirrel.Sqlizer。
type tagCond struct {
	tag  string
	cond squirrel.Sqlizer
	not  bool
}

// ToSql 生成 json_tree 存在性子查询：
// 展开 tags 列中该标签的所有取值，只要有一个满足条件即命中。
// 用 EXISTS 而非 JOIN 是因为标签天然多值，JOIN 会产生重复行。
func (e tagCond) ToSql() (string, []any, error) {
	cond, args, err := e.cond.ToSql()

	// Resolve the actual tag name (handles aliases like albumtype -> releasetype)
	// 解析真实标签名以支持别名（如 albumtype → releasetype）
	tagName := e.tag
	if fm, ok := fieldMap[e.tag]; ok {
		if fm.field != "" {
			tagName = fm.field
		}
		if fm.numeric {
			// JSON 中的值均为文本，数值标签需替换为 CAST 后再比较
			cond = strings.ReplaceAll(cond, "value", "CAST(value AS REAL)")
		}
	}

	cond = fmt.Sprintf("exists (select 1 from json_tree(tags, '$.%s') where key='value' and %s)",
		tagName, cond)
	if e.not {
		cond = "not " + cond
	}
	return cond, args, err
}

// roleExpr 构造角色条件表达式。
func roleExpr(role string, cond squirrel.Sqlizer, negate bool) squirrel.Sqlizer {
	return roleCond{role: role, cond: cond, not: negate}
}

// roleCond 是针对 participants JSON 列的条件，实现 squirrel.Sqlizer。
type roleCond struct {
	role string
	cond squirrel.Sqlizer
	not  bool
}

// ToSql 生成 json_tree 存在性子查询，匹配该角色下任一参与者的名字。
func (e roleCond) ToSql() (string, []any, error) {
	cond, args, err := e.cond.ToSql()
	cond = fmt.Sprintf(`exists (select 1 from json_tree(participants, '$.%s') where key='name' and %s)`,
		e.role, cond)
	if e.not {
		cond = "not " + cond
	}
	return cond, args, err
}

// AddRoles adds roles to the field map. This is used to add all artist roles to the field map, so they can be used in
// smart playlists. If a role already exists in the field map, it is ignored, so calls to this function are idempotent.
// AddRoles 把艺人角色注册为可用字段。已存在的字段不会被覆盖，因此可幂等调用。
// 由 model 包在配置加载后调用，以打破 criteria 与 model 之间的循环依赖。
func AddRoles(roles []string) {
	for _, role := range roles {
		name := strings.ToLower(role)
		if _, ok := fieldMap[name]; ok {
			continue
		}
		fieldMap[name] = &mappedField{field: name, isRole: true}
	}
}

// AddTagNames adds tag names to the field map. This is used to add all tags mapped in the `mappings.yml`
// file to the field map, so they can be used in smart playlists.
// If a tag name already exists in the field map, it is ignored, so calls to this function are idempotent.
// AddTagNames 把 mappings.yaml 中的标签注册为可用字段，可幂等调用。
// 注册前先扫描是否有字段声明了该名字为 alias，若有则复用同一映射，
// 使别名与本名指向同一列（例如 recordingdate 复用 date 的映射）。
func AddTagNames(tagNames []string) {
	for _, name := range tagNames {
		name := strings.ToLower(name)
		if _, ok := fieldMap[name]; ok {
			continue
		}
		for _, fm := range fieldMap {
			if fm.alias == name {
				fieldMap[name] = fm
				break
			}
		}
		if _, ok := fieldMap[name]; !ok {
			fieldMap[name] = &mappedField{field: name, isTag: true}
		}
	}
}

// AddNumericTags marks the given tag names as numeric so they can be cast
// when used in comparisons or sorting.
// AddNumericTags 把指定标签标记为数值类型，使其在比较与排序时被 CAST 为数字，
// 避免 "10" < "9" 这类字典序错误。字段不存在时会顺带注册。
func AddNumericTags(tagNames []string) {
	for _, name := range tagNames {
		name := strings.ToLower(name)
		if fm, ok := fieldMap[name]; ok {
			fm.numeric = true
		} else {
			fieldMap[name] = &mappedField{field: name, isTag: true, numeric: true}
		}
	}
}
