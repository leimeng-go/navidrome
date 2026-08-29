package log

import (
	"fmt"
	"io"
	"iter"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/navidrome/navidrome/utils/slice"
)

// ShortDur 按量级选择精度输出时长：日志里 "1.5s" 比 "1.503289471s" 更易读，
// 且不同量级需要的有效位数不同。
func ShortDur(d time.Duration) string {
	var s string
	switch {
	case d > time.Hour:
		s = d.Round(time.Minute).String()
	case d > time.Minute:
		s = d.Round(time.Second).String()
	case d > time.Second:
		s = d.Round(10 * time.Millisecond).String()
	case d > time.Millisecond:
		s = d.Round(100 * time.Microsecond).String()
	default:
		s = d.String()
	}
	s = strings.TrimSuffix(s, "0s")
	return strings.TrimSuffix(s, "0m")
}

// StringerValue 安全调用 String()，空指针返回 "nil" 而不是 panic。
func StringerValue(s fmt.Stringer) string {
	v := reflect.ValueOf(s)
	if v.Kind() == reflect.Pointer && v.IsNil() {
		return "nil"
	}
	return s.String()
}

// formatSeq 格式化迭代器序列。
func formatSeq[T any](v iter.Seq[T]) string {
	return formatSlice(slices.Collect(v))
}

// formatSlice 用反引号包裹每个元素，便于辨认含空格或逗号的值。
func formatSlice[T any](v []T) string {
	s := slice.Map(v, func(x T) string { return fmt.Sprintf("%v", x) })
	return fmt.Sprintf("[`%s`]", strings.Join(s, "`,`"))
}

// CRLFWriter 把 LF 补成 CRLF，供 Windows 下输出使用。
func CRLFWriter(w io.Writer) io.Writer {
	return &crlfWriter{w: w}
}

type crlfWriter struct {
	w        io.Writer
	lastByte byte
}

// Write 逐字节转换换行符。记录上一字节是为了避免把已有的 CRLF 变成 CRCRLF。
func (cw *crlfWriter) Write(p []byte) (int, error) {
	var written int
	for _, b := range p {
		if b == '\n' && cw.lastByte != '\r' {
			if _, err := cw.w.Write([]byte{'\r'}); err != nil {
				return written, err
			}
		}
		if _, err := cw.w.Write([]byte{b}); err != nil {
			return written, err
		}
		written++
		cw.lastByte = b
	}
	return written, nil
}
