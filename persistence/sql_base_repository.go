package persistence

import (
	"context"
	"crypto/md5"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"regexp"
	"strings"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	id2 "github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/hasher"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/pocketbase/dbx"
)

// sqlRepository is the base repository for all SQL repositories. It provides common functions to interact with the DB.
// When creating a new repository using this base, you must:
//
//   - Embed this struct.
//   - Set ctx and db fields. ctx should be the context passed to the constructor method, usually obtained from the request
//   - Call registerModel with the model instance and any possible filters.
//   - If the model has a different table name than the default (lowercase of the model name), it should be set manually
//     using the tableName field.
//   - Sort mappings must be set with setSortMappings method. If a sort field is not in the map, it will be used as the name of the column.
//
// All fields in filters and sortMappings must be in snake_case. Only sorts and filters based on real field names or
// defined in the mappings will be allowed.
//
// sqlRepository 是所有 SQL 仓储的基类，提供查询构建、分页、排序、
// 增删改、日志等通用能力，各具体仓储通过内嵌复用。
//
// 安全设计要点：排序字段与过滤字段都必须命中白名单
// （模型的真实字段名或显式声明的映射），否则拒绝。
// 这是因为列名无法参数化，只能拼进 SQL，白名单是防注入的关键防线。
//
// 使用约定见上方英文说明；filters 与 sortMappings 的键一律用 snake_case。
type sqlRepository struct {
	ctx       context.Context
	tableName string
	db        dbx.Builder

	// Do not set these fields manually, they are set by the registerModel method
	// 以下字段由 registerModel 设置，不要手工赋值
	filterMappings     map[string]filterFunc
	isFieldWhiteListed fieldWhiteListedFunc
	// Do not set this field manually, it is set by the setSortMappings method
	// 由 setSortMappings 设置
	sortMappings map[string]string
}

// invalidUserId 表示上下文中没有登录用户。
// 用一个不可能匹配任何真实用户的 ID，使权限过滤自然地返回空结果，
// 而不必在每处判断 nil。
const invalidUserId = "-1"

// loggedUser 取当前登录用户，未登录时返回带无效 ID 的空用户。
func loggedUser(ctx context.Context) *model.User {
	if user, ok := request.UserFrom(ctx); !ok {
		return &model.User{ID: invalidUserId}
	} else {
		return &user
	}
}

// registerModel 绑定模型：推断表名并构建字段白名单。
// 未显式设置 tableName 时，由模型类型名去掉 "*model." 前缀再转蛇形得到。
func (r *sqlRepository) registerModel(instance any, filters map[string]filterFunc) {
	if r.tableName == "" {
		r.tableName = strings.TrimPrefix(reflect.TypeOf(instance).String(), "*model.")
		r.tableName = toSnakeCase(r.tableName)
	}
	r.tableName = strings.ToLower(r.tableName)
	r.isFieldWhiteListed = registerModelWhiteList(instance)
	r.filterMappings = filters
}

// setSortMappings sets the mappings for the sort fields. If the sort field is not in the map, it will be used as is.
//
// If PreferSortTags is enabled, it will map the order fields to the corresponding sort expression,
// which gives precedence to sort tags.
// Ex: order_title => (coalesce(nullif(sort_title,”),order_title) collate nocase)
// To avoid performance issues, indexes should be created for these sort expressions
//
// NOTE: if an individual item has spaces, it should be wrapped in parentheses. For example,
// you should write "(lyrics != '[]')". This prevents the item being split unexpectedly.
// Without parentheses, "lyrics != '[]'" would be mapped as simply "lyrics"
//
// setSortMappings 设置排序字段到 SQL 表达式的映射，未映射的字段直接当列名用。
//
// 开启 PreferSortTags 时，order_* 字段会被改写为「优先取 sort 标签」的表达式，
// 例如 order_title → (coalesce(nullif(sort_title,”),order_title) collate nocase)。
// 这类表达式需要建立对应的表达式索引，否则排序会全表扫描。
//
// 注意：含空格的表达式必须用括号包起来，否则会被按空格切分而只取到第一个词。
func (r *sqlRepository) setSortMappings(mappings map[string]string, tableName ...string) {
	tn := r.tableName
	if len(tableName) > 0 {
		tn = tableName[0]
	}
	if conf.Server.PreferSortTags {
		for k, v := range mappings {
			v = mapSortOrder(tn, v)
			mappings[k] = v
		}
	}
	r.sortMappings = mappings
}

// newSelect 构建基础查询：应用分页、排序与过滤条件。
func (r sqlRepository) newSelect(options ...model.QueryOptions) SelectBuilder {
	sq := Select().From(r.tableName)
	if len(options) > 0 {
		r.resetSeededRandom(options)
		sq = r.applyOptions(sq, options...)
		sq = r.applyFilters(sq, options...)
	}
	return sq
}

// applyOptions 应用分页与排序选项。
func (r sqlRepository) applyOptions(sq SelectBuilder, options ...model.QueryOptions) SelectBuilder {
	if len(options) > 0 {
		if options[0].Max > 0 {
			sq = sq.Limit(uint64(options[0].Max))
		}
		if options[0].Offset > 0 {
			sq = sq.Offset(uint64(options[0].Offset))
		}
		if options[0].Sort != "" {
			sq = sq.OrderBy(r.buildSortOrder(options[0].Sort, options[0].Order))
		}
	}
	return sq
}

// TODO Change all sortMappings to have a consistent case
// sortMapping 查找排序字段对应的 SQL 表达式。
// 依次尝试原样、驼峰、蛇形三种形式，是为兼容各处调用方
// 传入的命名风格不统一（API 用驼峰、内部用蛇形）。
// 都未命中则把字段名转蛇形后直接当列名——
// 此时仍受 registerModel 建立的白名单约束，不会造成注入。
func (r sqlRepository) sortMapping(sort string) string {
	if mapping, ok := r.sortMappings[sort]; ok {
		return mapping
	}
	if mapping, ok := r.sortMappings[toCamelCase(sort)]; ok {
		return mapping
	}
	sort = toSnakeCase(sort)
	if mapping, ok := r.sortMappings[sort]; ok {
		return mapping
	}
	return sort
}

// buildSortOrder 生成 ORDER BY 子句。
//
// 映射后的表达式本身可能已含多个字段及各自的方向
// （如 "album asc, disc_number, track_number"）。
// 此处按逗号切分逐段处理：未写方向的段用请求方向，
// 已写 asc 的段用请求方向，已写 desc 的段用相反方向——
// 这样映射中定义的「相对次序」在正序与倒序下都能保持一致。
func (r sqlRepository) buildSortOrder(sort, order string) string {
	sort = r.sortMapping(sort)
	order = strings.ToLower(strings.TrimSpace(order))
	var reverseOrder string
	if order == "desc" {
		reverseOrder = "asc"
	} else {
		order = "asc"
		reverseOrder = "desc"
	}

	parts := strings.FieldsFunc(sort, splitFunc(','))
	newSort := make([]string, 0, len(parts))
	for _, p := range parts {
		f := strings.FieldsFunc(p, splitFunc(' '))
		newField := make([]string, 1, len(f))
		newField[0] = f[0]
		if len(f) == 1 {
			newField = append(newField, order)
		} else {
			if f[1] == "asc" {
				newField = append(newField, order)
			} else {
				newField = append(newField, reverseOrder)
			}
		}
		newSort = append(newSort, strings.Join(newField, " "))
	}
	return strings.Join(newSort, ", ")
}

// splitFunc 返回一个「括号感知」的分隔判定函数。
// 排序表达式中常含函数调用（如 coalesce(a, b)），
// 直接按逗号或空格切分会把函数参数拆断，
// 故用计数器跟踪括号深度，深度大于 0 时不做切分。
func splitFunc(delimiter rune) func(c rune) bool {
	open := 0
	return func(c rune) bool {
		if c == '(' {
			open++
			return false
		}
		if open > 0 {
			if c == ')' {
				open--
			}
			return false
		}
		return c == delimiter
	}
}

// applyFilters 追加调用方传入的过滤条件。
func (r sqlRepository) applyFilters(sq SelectBuilder, options ...model.QueryOptions) SelectBuilder {
	if len(options) > 0 && options[0].Filters != nil {
		sq = sq.Where(options[0].Filters)
	}
	return sq
}

// withTableName 给过滤条件的字段名加上表名前缀，
// 避免多表 JOIN 时出现列名歧义。
func (r *sqlRepository) withTableName(filter filterFunc) filterFunc {
	return func(field string, value any) Sqlizer {
		if r.tableName != "" {
			field = r.tableName + "." + field
		}
		return filter(field, value)
	}
}

// libraryIdFilter is a filter function to be added to resources that have a library_id column.
func libraryIdFilter(_ string, value interface{}) Sqlizer {
	return Eq{"library_id": value}
}

// applyLibraryFilter adds library filtering to queries for tables that have a library_id column
// This ensures users only see content from libraries they have access to
// applyLibraryFilter 追加音乐库权限过滤，确保用户只能看到有权访问的库中的内容。
//
// 管理员与「无登录用户」两种情况跳过过滤：
// 前者本就有全部权限；后者出现在扫描器等内部调用中，
// 这些场景不经过 HTTP 请求，没有用户上下文。
func (r sqlRepository) applyLibraryFilter(sq SelectBuilder, tableName ...string) SelectBuilder {
	user := loggedUser(r.ctx)

	// If the user is an admin, or the user ID is invalid (e.g., when no user is logged in), skip the library filter
	if user.IsAdmin || user.ID == invalidUserId {
		return sq
	}

	table := r.tableName
	if len(tableName) > 0 {
		table = tableName[0]
	}

	// Get user's accessible library IDs
	// Use subquery to filter by user's library access
	// 用子查询而非预先查出 ID 列表：避免多一次往返，
	// 也不受 SQL 变量个数上限的限制
	return sq.Where(Expr(table+".library_id IN ("+
		"SELECT ul.library_id FROM user_library ul WHERE ul.user_id = ?)", user.ID))
}

// seedKey 生成随机排序的种子键，按「表名 + 用户」隔离，
// 使不同用户、不同资源的随机序列互不干扰。
func (r sqlRepository) seedKey() string {
	// Seed keys must be all lowercase, or else SQLite3 will encode it, making it not match the seed
	// used in the query. Hashing the user ID and converting it to a hex string will do the trick
	// 种子键必须全小写，否则 SQLite3 会对其编码，导致与查询中使用的种子对不上。
	// 把用户 ID 取 MD5 后转十六进制字符串即可保证全小写
	userIDHash := md5.Sum([]byte(loggedUser(r.ctx).ID))
	return fmt.Sprintf("%s|%x", r.tableName, userIDHash)
}

// resetSeededRandom 把 "random" 排序替换为带种子的自定义 SQL 函数。
//
// 用种子随机而非 SQLite 的 random()，是为了让分页结果稳定：
// 翻页时若每次都重新随机，会出现重复或遗漏的条目。
//
// 种子策略：调用方显式给了 Seed 就用它（客户端可据此复现同一序列）；
// 否则仅在请求第一页（Offset == 0）时重新播种，
// 后续翻页沿用同一种子，从而保证整轮分页看到的是同一个随机序列。
func (r sqlRepository) resetSeededRandom(options []model.QueryOptions) {
	if len(options) == 0 || options[0].Sort != "random" {
		return
	}
	options[0].Sort = fmt.Sprintf("SEEDEDRAND('%s', %s.id)", r.seedKey(), r.tableName)
	if options[0].Seed != "" {
		hasher.SetSeed(r.seedKey(), options[0].Seed)
		return
	}
	if options[0].Offset == 0 {
		hasher.Reseed(r.seedKey())
	}
}

// executeSQL 执行写操作（INSERT/UPDATE/DELETE），返回受影响行数。
func (r sqlRepository) executeSQL(sq Sqlizer) (int64, error) {
	query, args, err := r.toSQL(sq)
	if err != nil {
		return 0, err
	}
	start := time.Now()
	var c int64
	res, err := r.db.NewQuery(query).Bind(args).WithContext(r.ctx).Execute()
	if res != nil {
		c, _ = res.RowsAffected()
	}
	r.logSQL(query, args, err, c, start)
	if err != nil {
		// 部分驱动不支持 LastInsertId，但语句其实已执行成功，
		// 这类报错不应向上传播
		if err.Error() != "LastInsertId is not supported by this driver" {
			return 0, err
		}
	}
	return c, err
}

var placeholderRegex = regexp.MustCompile(`\?`)

// toSQL 把 squirrel 生成的 SQL 转换为 dbx 所需的具名参数形式。
// squirrel 用 ? 占位，dbx 用 {:pN}，故按出现顺序逐个替换并建立参数表。
func (r sqlRepository) toSQL(sq Sqlizer) (string, dbx.Params, error) {
	query, args, err := sq.ToSql()
	if err != nil {
		return "", nil, err
	}
	// Replace query placeholders with named params
	// 按顺序把 ? 替换为 {:pN} 并收集参数
	params := make(dbx.Params, len(args))
	counter := 0
	result := placeholderRegex.ReplaceAllStringFunc(query, func(_ string) string {
		p := fmt.Sprintf("p%d", counter)
		params[p] = args[counter]
		counter++
		return "{:" + p + "}"
	})
	return result, params, nil
}

// queryOne 查询单行并扫描到 response，无结果时返回 model.ErrNotFound。
// 把 sql.ErrNoRows 转换为领域错误，使上层不依赖 database/sql 包。
func (r sqlRepository) queryOne(sq Sqlizer, response interface{}) error {
	query, args, err := r.toSQL(sq)
	if err != nil {
		return err
	}
	start := time.Now()
	err = r.db.NewQuery(query).Bind(args).WithContext(r.ctx).One(response)
	if errors.Is(err, sql.ErrNoRows) {
		r.logSQL(query, args, nil, 0, start)
		return model.ErrNotFound
	}
	r.logSQL(query, args, err, 1, start)
	return err
}

// queryWithStableResults is a helper function to execute a query and return an iterator that will yield its results
// from a cursor, guaranteeing that the results will be stable, even if the underlying data changes.
// queryWithStableResults 以游标方式流式返回结果，返回 Go 1.23 的迭代器。
//
// 相比一次性载入全部结果，游标方式内存占用恒定，适合扫描等大结果集场景；
// 同时基于同一快照读取，即便底层数据在遍历期间变化，结果也保持稳定。
//
// 迭代器在 yield 返回 false 或出错时立即结束，
// rows.Close 由 defer 保证释放。
func queryWithStableResults[T any](r sqlRepository, sq SelectBuilder, options ...model.QueryOptions) (iter.Seq2[T, error], error) {
	if len(options) > 0 && options[0].Offset > 0 {
		sq = r.optimizePagination(sq, options[0])
	}
	query, args, err := r.toSQL(sq)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	rows, err := r.db.NewQuery(query).Bind(args).WithContext(r.ctx).Rows()
	r.logSQL(query, args, err, -1, start)
	if err != nil {
		return nil, err
	}
	return func(yield func(T, error) bool) {
		defer rows.Close()
		for rows.Next() {
			var row T
			err := rows.ScanStruct(&row)
			if !yield(row, err) || err != nil {
				return
			}
		}
		if err := rows.Err(); err != nil {
			var empty T
			yield(empty, err)
		}
	}, nil
}

// queryAll 查询多行到切片。
func (r sqlRepository) queryAll(sq SelectBuilder, response interface{}, options ...model.QueryOptions) error {
	if len(options) > 0 && options[0].Offset > 0 {
		sq = r.optimizePagination(sq, options[0])
	}
	query, args, err := r.toSQL(sq)
	if err != nil {
		return err
	}
	start := time.Now()
	err = r.db.NewQuery(query).Bind(args).WithContext(r.ctx).All(response)
	if errors.Is(err, sql.ErrNoRows) {
		r.logSQL(query, args, nil, -1, start)
		return model.ErrNotFound
	}
	r.logSQL(query, args, err, int64(reflect.ValueOf(response).Elem().Len()), start)
	return err
}

// queryAllSlice is a helper function to query a single column and return the result in a slice
// queryAllSlice 查询单列并把结果收集为切片（如只取一批 ID）。
func (r sqlRepository) queryAllSlice(sq SelectBuilder, response interface{}) error {
	query, args, err := r.toSQL(sq)
	if err != nil {
		return err
	}
	start := time.Now()
	err = r.db.NewQuery(query).Bind(args).WithContext(r.ctx).Column(response)
	if errors.Is(err, sql.ErrNoRows) {
		r.logSQL(query, args, nil, -1, start)
		return model.ErrNotFound
	}
	r.logSQL(query, args, err, int64(reflect.ValueOf(response).Elem().Len()), start)
	return err
}

// optimizePagination uses a less inefficient pagination, by not using OFFSET.
// See https://gist.github.com/ssokolow/262503
// optimizePagination 用「排除前 N 行的 rowid」代替 OFFSET 来做深分页。
//
// SQLite 的 OFFSET 需要真正读取并丢弃前 N 行，偏移越大越慢。
// 改为先用一个只取 rowid 的轻量子查询定位前 N 行，
// 再在主查询中用 NOT IN 排除，可避免在主查询中做昂贵的行读取。
//
// 仅在偏移超过阈值时启用：小偏移下子查询的开销反而更大。
func (r sqlRepository) optimizePagination(sq SelectBuilder, options model.QueryOptions) SelectBuilder {
	if options.Offset > conf.Server.DevOffsetOptimize {
		sq = sq.RemoveOffset()
		rowidSq := sq.RemoveColumns().Columns(r.tableName + ".rowid")
		rowidSq = rowidSq.Limit(uint64(options.Offset))
		rowidSql, args, _ := rowidSq.ToSql()
		sq = sq.Where(r.tableName+".rowid not in ("+rowidSql+")", args...)
	}
	return sq
}

// exists 判断满足条件的记录是否存在。
func (r sqlRepository) exists(cond Sqlizer) (bool, error) {
	existsQuery := Select("count(*) as exist").From(r.tableName).Where(cond)
	var res struct{ Exist int64 }
	err := r.queryOne(existsQuery, &res)
	return res.Exist > 0, err
}

// count 统计满足条件的记录数。
//
// 复用调用方已构建的查询（含 JOIN 与过滤），但替换掉 SELECT 列、
// 清除分页；用 distinct id 计数以消除 JOIN 带来的重复行。
// ORDER BY 改为主键：既覆盖掉原有的排序子句（计数不需要排序、
// 且排序会显著拖慢查询），又保持语句合法。
func (r sqlRepository) count(countQuery SelectBuilder, options ...model.QueryOptions) (int64, error) {
	countQuery = countQuery.
		RemoveColumns().Columns("count(distinct " + r.tableName + ".id) as count").
		RemoveOffset().RemoveLimit().
		OrderBy(r.tableName + ".id"). // To remove any ORDER BY clause that could slow down the query
		From(r.tableName)
	countQuery = r.applyFilters(countQuery, options...)
	var res struct{ Count int64 }
	err := r.queryOne(countQuery, &res)
	return res.Count, err
}

// putByMatch 按业务条件定位既有记录后写入：
// 已知 ID 时直接写，否则先按 filter 查出 ID 再写。
// 用于有天然唯一键但 ID 由系统生成的场景。
func (r sqlRepository) putByMatch(filter Sqlizer, id string, m interface{}, colsToUpdate ...string) (string, error) {
	if id != "" {
		return r.put(id, m, colsToUpdate...)
	}
	existsQuery := r.newSelect().Columns("id").From(r.tableName).Where(filter)

	var res struct{ ID string }
	err := r.queryOne(existsQuery, &res)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return "", err
	}
	return r.put(res.ID, m, colsToUpdate...)
}

// put 写入记录（upsert 语义）。
//
// 策略是「先尝试 UPDATE，影响行数为 0 再 INSERT」，
// 这样既支持系统生成 ID 的新增，也支持外部指定 ID 的新增
// （如扫描器算出的 PID：ID 已知但记录可能尚不存在）。
//
// colsToUpdate 为空表示更新全部字段，否则只更新指定列。
func (r sqlRepository) put(id string, m interface{}, colsToUpdate ...string) (newId string, err error) {
	values, err := toSQLArgs(m)
	if err != nil {
		return "", fmt.Errorf("error preparing values to write to DB: %w", err)
	}
	// If there's an ID, try to update first
	// 有 ID 就先尝试更新
	if id != "" {
		updateValues := map[string]interface{}{}

		// This is a map of the columns that need to be updated, if specified
		// 转成集合便于查找；键统一为蛇形以匹配列名
		c2upd := slice.ToMap(colsToUpdate, func(s string) (string, struct{}) {
			return toSnakeCase(s), struct{}{}
		})
		for k, v := range values {
			if _, found := c2upd[k]; len(c2upd) == 0 || found {
				updateValues[k] = v
			}
		}

		updateValues["id"] = id
		// 创建时间只在插入时确定，更新时必须保留原值
		delete(updateValues, "created_at")
		// To avoid updating the media_file birth_time on each scan. Not the best solution, but it works for now
		// TODO move to mediafile_repository when each repo has its own upsert method
		// 同理保留文件创建时间，避免每次扫描都被刷新
		delete(updateValues, "birth_time")
		update := Update(r.tableName).Where(Eq{"id": id}).SetMap(updateValues)
		count, err := r.executeSQL(update)
		if err != nil {
			return "", err
		}
		if count > 0 {
			return id, nil
		}
	}
	// If it does not have an ID OR the ID was not found (when it is a new record with predefined id)
	// 无 ID 则生成一个；若 ID 已指定但更新未命中，则沿用该 ID 插入
	if id == "" {
		id = id2.NewRandom()
		values["id"] = id
	}
	insert := Insert(r.tableName).SetMap(values)
	_, err = r.executeSQL(insert)
	return id, err
}

// delete 按条件删除记录。
func (r sqlRepository) delete(cond Sqlizer) error {
	del := Delete(r.tableName).Where(cond)
	_, err := r.executeSQL(del)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ErrNotFound
	}
	return err
}

// logSQL 记录 SQL 执行日志。
// context.Canceled 按 Trace 级别记录：客户端主动断开是正常现象，
// 不应产生错误噪声。
func (r sqlRepository) logSQL(sql string, args dbx.Params, err error, rowsAffected int64, start time.Time) {
	elapsed := time.Since(start)
	if err == nil || errors.Is(err, context.Canceled) {
		log.Trace(r.ctx, "SQL: `"+sql+"`", "args", args, "rowsAffected", rowsAffected, "elapsedTime", elapsed, err)
	} else {
		log.Error(r.ctx, "SQL: `"+sql+"`", "args", args, "rowsAffected", rowsAffected, "elapsedTime", elapsed, err)
	}
}
