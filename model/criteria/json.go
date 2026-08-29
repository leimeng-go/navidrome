package criteria

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件负责规则表达式树与 JSON 的双向转换。
// JSON 中每个表达式都是单键对象，键为操作符名（如 "is"、"all"），
// 值为该操作符的参数，从而可无限嵌套。

// unmarshalConjunctionType 是连接词（all/any）子表达式列表的反序列化中间类型。
// 单独定义类型是为了挂载 UnmarshalJSON，实现按操作符名分派构造具体表达式。
type unmarshalConjunctionType []Expression

// UnmarshalJSON 逐个解析子表达式。
// 先尝试按叶子操作符解析，失败再尝试按连接词解析（递归），
// 两者都不匹配则说明操作符名非法。
func (uc *unmarshalConjunctionType) UnmarshalJSON(data []byte) error {
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var es unmarshalConjunctionType
	for _, e := range raw {
		for k, v := range e {
			// 操作符名统一转小写比较，使 JSON 中的 isNot / isnot 都能识别
			k = strings.ToLower(k)
			expr := unmarshalExpression(k, v)
			if expr == nil {
				expr = unmarshalConjunction(k, v)
			}
			if expr == nil {
				return fmt.Errorf(`invalid expression key '%s'`, k)
			}
			es = append(es, expr)
		}
	}
	*uc = es
	return nil
}

// unmarshalExpression 按操作符名构造叶子表达式，不认识的名字返回 nil
// 交由调用方继续尝试解析为连接词。
func unmarshalExpression(opName string, rawValue json.RawMessage) Expression {
	m := make(map[string]any)
	err := json.Unmarshal(rawValue, &m)
	if err != nil {
		return nil
	}
	switch opName {
	case "is":
		return Is(m)
	case "isnot":
		return IsNot(m)
	case "gt":
		return Gt(m)
	case "lt":
		return Lt(m)
	case "contains":
		return Contains(m)
	case "notcontains":
		return NotContains(m)
	case "startswith":
		return StartsWith(m)
	case "endswith":
		return EndsWith(m)
	case "intherange":
		return InTheRange(m)
	case "before":
		return Before(m)
	case "after":
		return After(m)
	case "inthelast":
		return InTheLast(m)
	case "notinthelast":
		return NotInTheLast(m)
	case "inplaylist":
		return InPlaylist(m)
	case "notinplaylist":
		return NotInPlaylist(m)
	}
	return nil
}

// unmarshalConjunction 按连接词名构造 All/Any，其子表达式会递归解析。
func unmarshalConjunction(conjName string, rawValue json.RawMessage) Expression {
	var items unmarshalConjunctionType
	err := json.Unmarshal(rawValue, &items)
	if err != nil {
		return nil
	}
	switch conjName {
	case "any":
		return Any(items)
	case "all":
		return All(items)
	}
	return nil
}

// marshalExpression 把单字段操作符序列化为 {"操作符":{"字段":值}}。
// 手工拼接字符串而不用 json.Marshal(map)，是为了避免 map 序列化时
// 的键排序处理并保持输出结构可控；因约定只有一个字段，故取首个后即 break。
func marshalExpression(name string, value map[string]any) ([]byte, error) {
	if len(value) != 1 {
		return nil, fmt.Errorf(`invalid %s expression length %d for values %v`, name, len(value), value)
	}
	b := strings.Builder{}
	b.WriteString(`{"` + name + `":{`)
	for f, v := range value {
		j, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		b.WriteString(`"` + f + `":`)
		b.Write(j)
		break
	}
	b.WriteString("}}")
	return []byte(b.String()), nil
}

// marshalConjunction 把连接词序列化为 {"all":[...]} 或 {"any":[...]}。
// 借助 omitempty 保证只输出实际使用的那个键。
func marshalConjunction(name string, conj []Expression) ([]byte, error) {
	aux := struct {
		All []Expression `json:"all,omitempty"`
		Any []Expression `json:"any,omitempty"`
	}{}
	if name == "any" {
		aux.Any = conj
	} else {
		aux.All = conj
	}
	return json.Marshal(aux)
}
