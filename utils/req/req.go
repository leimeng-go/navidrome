// Package req 提供 HTTP 查询参数的读取与类型转换。
//
// Subsonic 客户端实现参差不齐，参数常缺失或格式错误，
// 故大量接口需要「取不到就用默认值」的宽容语义，这里统一提供 XxxOr 形式的方法。
package req

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/navidrome/navidrome/log"
)

// Values 包装请求以提供带类型的参数读取。
type Values struct {
	*http.Request
}

// Params 从请求构造参数读取器。
func Params(r *http.Request) *Values {
	return &Values{r}
}

var (
	ErrMissingParam = errors.New("missing parameter")
	ErrInvalidParam = errors.New("invalid parameter")
)

func newError(err error, param string) error {
	return fmt.Errorf("%w: '%s'", err, param)
}

// String 读取字符串参数，空值视为缺失。
func (r *Values) String(param string) (string, error) {
	v := r.URL.Query().Get(param)
	if v == "" {
		return "", newError(ErrMissingParam, param)
	}
	return v, nil
}

// StringPtr 区分「未传该参数」与「传了空字符串」，用于部分更新场景。
func (r *Values) StringPtr(param string) *string {
	var v *string
	if _, exists := r.URL.Query()[param]; exists {
		s := r.URL.Query().Get(param)
		v = &s
	}
	return v
}

// BoolPtr 同 StringPtr，区分未传与传空。
func (r *Values) BoolPtr(param string) *bool {
	var v *bool
	if _, exists := r.URL.Query()[param]; exists {
		s := r.URL.Query().Get(param)
		b := strings.Contains("/true/on/1/", "/"+strings.ToLower(s)+"/")
		v = &b
	}
	return v
}

func (r *Values) StringOr(param, def string) string {
	v, _ := r.String(param)
	if v == "" {
		return def
	}
	return v
}

func (r *Values) Strings(param string) ([]string, error) {
	values := r.URL.Query()[param]
	if len(values) == 0 {
		return nil, newError(ErrMissingParam, param)
	}
	return values, nil
}

// TimeOr 读取毫秒时间戳。
// -1 是客户端表示「无」的惯用值；
// 早于 1970-01-02 的值多半是把秒当成了毫秒，一并按缺省处理。
func (r *Values) TimeOr(param string, def time.Time) time.Time {
	v, _ := r.String(param)
	if v == "" || v == "-1" {
		return def
	}
	value, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return def
	}
	t := time.UnixMilli(value)
	if t.Before(time.Date(1970, time.January, 2, 0, 0, 0, 0, time.UTC)) {
		return def
	}
	return t
}

// Times 读取多个时间戳。单个值无法解析时以当前时间代替并记录警告，
// 以保证返回的时间数量与 ID 一一对应。
func (r *Values) Times(param string) ([]time.Time, error) {
	pStr, err := r.Strings(param)
	if err != nil {
		return nil, err
	}
	times := make([]time.Time, len(pStr))
	for i, t := range pStr {
		ti, err := strconv.ParseInt(t, 10, 64)
		if err != nil {
			log.Warn(r.Context(), "Ignoring invalid time param", "time", t, err)
			times[i] = time.Now()
			continue
		}
		times[i] = time.UnixMilli(ti)
	}
	return times, nil
}

func (r *Values) Int64(param string) (int64, error) {
	v, err := r.String(param)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w '%s': expected integer, got '%s'", ErrInvalidParam, param, v)
	}
	return value, nil
}

func (r *Values) Int(param string) (int, error) {
	v, err := r.Int64(param)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

func (r *Values) IntOr(param string, def int) int {
	v, err := r.Int64(param)
	if err != nil {
		return def
	}
	return int(v)
}

func (r *Values) Int64Or(param string, def int64) int64 {
	v, err := r.Int64(param)
	if err != nil {
		return def
	}
	return v
}

// Ints 读取多个整数，无法解析的项直接跳过。
func (r *Values) Ints(param string) ([]int, error) {
	pStr, err := r.Strings(param)
	if err != nil {
		return nil, err
	}
	ints := make([]int, 0, len(pStr))
	for _, s := range pStr {
		i, err := strconv.ParseInt(s, 10, 64)
		if err == nil {
			ints = append(ints, int(i))
		}
	}
	return ints, nil
}

// Bool 解析布尔值，接受 true/on/1 三种写法（不同客户端习惯不同）。
func (r *Values) Bool(param string) (bool, error) {
	v, err := r.String(param)
	if err != nil {
		return false, err
	}
	return strings.Contains("/true/on/1/", "/"+strings.ToLower(v)+"/"), nil
}

func (r *Values) BoolOr(param string, def bool) bool {
	v, err := r.Bool(param)
	if err != nil {
		return def
	}
	return v
}
