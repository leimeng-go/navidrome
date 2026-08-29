package persistence

import (
	"cmp"
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/fatih/structs"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// 本文件把 REST 层（deluan/rest）的查询参数翻译为 SQL 条件，
// 并维护字段白名单——这是防止 SQL 注入的关键：
// 列名无法参数化，只能拼接进 SQL，因此必须限定在模型真实字段范围内。

// filterFunc 是把「字段 + 值」翻译为 SQL 条件的函数。
type filterFunc = func(field string, value any) Sqlizer

// parseRestFilters 把 REST 请求中的过滤参数翻译为 SQL 条件。
//
// 匹配顺序：自定义过滤函数 → 白名单校验 → 默认规则。
// 默认规则中，以 id 结尾的字段用精确匹配（ID 前缀匹配无意义），
// 其余字段用前缀匹配（便于做搜索框的即时筛选）。
// 未命中白名单的字段会被丢弃并告警，而不是报错——
// 前端可能传来无关参数，不应因此使整个请求失败。
func (r *sqlRepository) parseRestFilters(ctx context.Context, options rest.QueryOptions) Sqlizer {
	if len(options.Filters) == 0 {
		return nil
	}
	filters := And{}
	for f, v := range options.Filters {
		// Ignore filters with empty values
		if v == "" {
			continue
		}
		// Look for a custom filter function
		f = strings.ToLower(f)
		if ff, ok := r.filterMappings[f]; ok {
			if filter := ff(f, v); filter != nil {
				filters = append(filters, filter)
			}
			continue
		}
		// Ignore invalid filters (not based on a field or filter function)
		if r.isFieldWhiteListed != nil && !r.isFieldWhiteListed(f) {
			log.Warn(ctx, "Ignoring filter not whitelisted", "filter", f, "table", r.tableName)
			continue
		}
		// For fields ending in "id", use an exact match
		if strings.HasSuffix(f, "id") {
			filters = append(filters, eqFilter(f, v))
			continue
		}
		// Default to a "starts with" filter
		filters = append(filters, startsWithFilter(f, v))
	}
	return filters
}

// parseRestOptions 把 REST 查询参数转换为内部的 QueryOptions。
// seed 参数需先取出并从过滤条件中删除：它控制随机排序的种子，
// 不是一个数据库字段，留在 Filters 中会被当作非法过滤而告警。
func (r *sqlRepository) parseRestOptions(ctx context.Context, options ...rest.QueryOptions) model.QueryOptions {
	qo := model.QueryOptions{}
	if len(options) > 0 {
		qo.Sort, qo.Order = r.sanitizeSort(options[0].Sort, options[0].Order)
		qo.Max = options[0].Max
		qo.Offset = options[0].Offset
		if seed, ok := options[0].Filters["seed"].(string); ok {
			qo.Seed = seed
			delete(options[0].Filters, "seed")
		}
		qo.Filters = r.parseRestFilters(ctx, options[0])
	}
	return qo
}

// sanitizeSort 校验排序参数：字段须命中映射表或白名单，
// 否则清空排序（而非报错）；方向只允许 asc/desc。
// 这里同样是防注入的必要检查，因为排序字段会被拼进 SQL。
func (r sqlRepository) sanitizeSort(sort, order string) (string, string) {
	if sort != "" {
		sort = toSnakeCase(sort)
		if mapped, ok := r.sortMappings[sort]; ok {
			sort = mapped
		} else {
			if !r.isFieldWhiteListed(sort) {
				log.Warn(r.ctx, "Ignoring sort not whitelisted", "sort", sort, "table", r.tableName)
				sort = ""
			}
		}
	}
	if order != "" {
		order = strings.ToLower(order)
		if order != "desc" {
			order = "asc"
		}
	}
	return sort, order
}

// 以下是一组可复用的过滤函数构造器，供各仓储在 registerModel 时声明。

// eqFilter 精确匹配。
func eqFilter(field string, value any) Sqlizer {
	return Eq{field: value}
}

// startsWithFilter 前缀匹配。
func startsWithFilter(field string, value any) Sqlizer {
	return Like{field: fmt.Sprintf("%s%%", value)}
}

// containsFilter 子串匹配。返回闭包以便把列名固定下来，
// 使过滤可作用于与请求参数名不同的列。
func containsFilter(field string) func(string, any) Sqlizer {
	return func(_ string, value any) Sqlizer {
		return Like{field: fmt.Sprintf("%%%s%%", value)}
	}
}

// booleanFilter 布尔匹配。查询参数总是字符串，故只把 "true" 视为真。
func booleanFilter(field string, value any) Sqlizer {
	v := strings.ToLower(value.(string))
	return Eq{field: v == "true"}
}

// fullTextFilter 全文搜索。若关键词本身是 MusicBrainz ID，
// 则优先按 MBID 精确匹配，否则退回全文索引匹配。
func fullTextFilter(tableName string, mbidFields ...string) func(string, any) Sqlizer {
	return func(field string, value any) Sqlizer {
		v := strings.ToLower(value.(string))
		cond := cmp.Or(
			mbidExpr(tableName, v, mbidFields...),
			fullTextExpr(tableName, v),
		)
		return cond
	}
}

// substringFilter 按空格拆词后要求全部命中（AND 语义），
// 使「披头士 白色」这类多词查询能匹配词序不同的记录。
func substringFilter(field string, value any) Sqlizer {
	parts := strings.Fields(value.(string))
	filters := And{}
	for _, part := range parts {
		filters = append(filters, Like{field: "%" + part + "%"})
	}
	return filters
}

// idFilter 按主键匹配，带表名前缀以避免 JOIN 时的列名歧义。
func idFilter(tableName string) func(string, any) Sqlizer {
	return func(field string, value any) Sqlizer { return Eq{tableName + ".id": value} }
}

// invalidFilter 用于显式禁用某个过滤参数：
// 记录告警并返回恒假条件，使查询安全地返回空结果。
func invalidFilter(ctx context.Context) func(string, any) Sqlizer {
	return func(field string, value any) Sqlizer {
		log.Warn(ctx, "Invalid filter", "fieldName", field, "value", value)
		return Eq{"1": "0"}
	}
}

// whiteList 缓存各模型的合法字段集合，按模型类型名索引。
// 全局共享故需加锁；只在首次注册时写入，之后均为读操作。
var (
	whiteList = map[string]map[string]struct{}{}
	mutex     sync.RWMutex
)

// registerModelWhiteList 为模型建立字段白名单并返回查询函数。
func registerModelWhiteList(instance any) fieldWhiteListedFunc {
	name := reflect.TypeOf(instance).String()
	registerFieldWhiteList(name, instance)
	return getFieldWhiteListedFunc(name)
}

// registerFieldWhiteList 反射提取模型字段名并转为蛇形存入白名单。
// 已注册则直接返回，保证幂等（多个仓储实例共用同一模型）。
func registerFieldWhiteList(name string, instance any) {
	mutex.Lock()
	defer mutex.Unlock()
	if whiteList[name] != nil {
		return
	}
	m := structs.Map(instance)
	whiteList[name] = map[string]struct{}{}
	for k := range m {
		whiteList[name][toSnakeCase(k)] = struct{}{}
	}
	// 标注字段（收藏、评分、播放次数）存于 annotation 表，
	// 不属于模型自身，但可用于过滤与排序，故一并加入白名单
	ma := structs.Map(model.Annotations{})
	for k := range ma {
		whiteList[name][toSnakeCase(k)] = struct{}{}
	}
}

// fieldWhiteListedFunc 判断字段是否允许用于过滤或排序。
type fieldWhiteListedFunc func(field string) bool

// getFieldWhiteListedFunc 返回针对指定模型的白名单查询函数。
// 模型未注册时一律返回 false，即默认拒绝。
func getFieldWhiteListedFunc(tableName string) fieldWhiteListedFunc {
	return func(field string) bool {
		mutex.RLock()
		defer mutex.RUnlock()
		if _, ok := whiteList[tableName]; !ok {
			return false
		}
		_, ok := whiteList[tableName][field]
		return ok
	}
}
