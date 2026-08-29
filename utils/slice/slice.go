// Package slice 提供切片与序列的常用操作。
package slice

import (
	"bufio"
	"bytes"
	"cmp"
	"io"
	"iter"
	"slices"

	"golang.org/x/exp/maps"
)

// Map 逐元素转换。
func Map[T any, R any](t []T, mapFunc func(T) R) []R {
	r := make([]R, len(t))
	for i, e := range t {
		r[i] = mapFunc(e)
	}
	return r
}

// MapWithArg 转换时附带一个共享参数（如 context），省去闭包捕获。
func MapWithArg[I any, O any, A any](t []I, arg A, mapFunc func(A, I) O) []O {
	return Map(t, func(e I) O {
		return mapFunc(arg, e)
	})
}

// Group 按键分组。
func Group[T any, K comparable](s []T, keyFunc func(T) K) map[K][]T {
	m := map[K][]T{}
	for _, item := range s {
		k := keyFunc(item)
		m[k] = append(m[k], item)
	}
	return m
}

// ToMap 转换为映射。
func ToMap[T any, K comparable, V any](s []T, transformFunc func(T) (K, V)) map[K]V {
	m := make(map[K]V, len(s))
	for _, item := range s {
		k, v := transformFunc(item)
		m[k] = v
	}
	return m
}

// CompactByFrequency 去重并按出现次数从多到少排序。
// 用于从多个来源的标签中挑出最主流的取值。
func CompactByFrequency[T comparable](list []T) []T {
	counters := make(map[T]int)
	for _, item := range list {
		counters[item]++
	}

	sorted := maps.Keys(counters)
	slices.SortFunc(sorted, func(i, j T) int {
		return cmp.Compare(counters[j], counters[i])
	})
	return sorted
}

// MostFrequent 返回出现次数最多的元素，零值不参与统计。
func MostFrequent[T comparable](list []T) T {
	var zero T
	if len(list) == 0 {
		return zero
	}

	counters := make(map[T]int)
	var topItem T
	var topCount int

	for _, value := range list {
		if value == zero {
			continue
		}
		counters[value]++
		if counters[value] > topCount {
			topItem = value
			topCount = counters[value]
		}
	}

	return topItem
}

// Insert 在指定下标插入元素。
func Insert[T any](slice []T, value T, index int) []T {
	return append(slice[:index], append([]T{value}, slice[index:]...)...)
}

// Remove 移除指定下标的元素。
func Remove[T any](slice []T, index int) []T {
	return append(slice[:index], slice[index+1:]...)
}

// Move 把元素从一个位置移到另一个位置，用于歌单排序。
func Move[T any](slice []T, srcIndex int, dstIndex int) []T {
	value := slice[srcIndex]
	return Insert(Remove(slice, srcIndex), value, dstIndex)
}

// Unique 去重并保持原有顺序。
func Unique[T comparable](list []T) []T {
	seen := make(map[T]struct{})
	var result []T
	for _, item := range list {
		if _, ok := seen[item]; !ok {
			seen[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

// LinesFrom returns a Seq that reads lines from the given reader
// LinesFrom 按行读取，惰性产出以免一次性载入大文件。
func LinesFrom(reader io.Reader) iter.Seq[string] {
	return func(yield func(string) bool) {
		scanner := bufio.NewScanner(reader)
		scanner.Split(scanLines)
		for scanner.Scan() {
			if !yield(scanner.Text()) {
				return
			}
		}
	}
}

// From https://stackoverflow.com/a/41433698
// scanLines 同时支持 LF、CR 与 CRLF 三种换行符。
// 标准库的 ScanLines 不认单独的 CR，而老式 Mac 生成的 m3u 歌单正是这种格式。
func scanLines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			// We have a line terminated by single newline.
			return i + 1, data[0:i], nil
		}
		advance = i + 1
		if len(data) > i+1 && data[i+1] == '\n' {
			advance += 1
		}
		return advance, data[0:i], nil
	}
	// If we're at EOF, we have a final, non-terminated line. Return it.
	if atEOF {
		return len(data), data, nil
	}
	// Request more data.
	return 0, nil, nil
}

// CollectChunks collects chunks of n elements from the input sequence and return a Seq of chunks
// CollectChunks 按 n 个一组切块，用于批量写库。
func CollectChunks[T any](it iter.Seq[T], n int) iter.Seq[[]T] {
	return func(yield func([]T) bool) {
		s := make([]T, 0, n)
		for x := range it {
			s = append(s, x)
			if len(s) >= n {
				if !yield(s) {
					return
				}
				s = make([]T, 0, n)
			}
		}
		if len(s) > 0 {
			yield(s)
		}
	}
}

// SeqFunc returns a Seq that iterates over the slice with the given mapping function
func SeqFunc[I, O any](s []I, f func(I) O) iter.Seq[O] {
	return func(yield func(O) bool) {
		for _, x := range s {
			if !yield(f(x)) {
				return
			}
		}
	}
}

// Filter returns a new slice containing only the elements of s for which filterFunc returns true
// Filter 过滤元素。
func Filter[T any](s []T, filterFunc func(T) bool) []T {
	var result []T
	for _, item := range s {
		if filterFunc(item) {
			result = append(result, item)
		}
	}
	return result
}
