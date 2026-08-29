package model

import (
	"cmp"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/utils/str"
)

// Line 是一行歌词。Start 为该行的起始毫秒数，
// 非同步歌词（纯文本）时为 nil。
type Line struct {
	Start *int64 `structs:"start,omitempty" json:"start,omitempty"`
	Value string `structs:"value"           json:"value"`
}

// Lyrics 是一份歌词。同一首曲目可有多份不同语言的歌词（见 LyricList）。
type Lyrics struct {
	DisplayArtist string `structs:"displayArtist,omitempty" json:"displayArtist,omitempty"` // 来自 LRC 的 [ar:] 标签
	DisplayTitle  string `structs:"displayTitle,omitempty"  json:"displayTitle,omitempty"`  // 来自 LRC 的 [ti:] 标签
	Lang          string `structs:"lang"                    json:"lang"`
	Line          []Line `structs:"line"                    json:"line"`
	Offset        *int64 `structs:"offset,omitempty"        json:"offset,omitempty"` // 全局时间偏移（毫秒），来自 [offset:] 标签
	Synced        bool   `structs:"synced"                  json:"synced"`           // 是否为带时间轴的同步歌词
}

// support the standard [mm:ss.mm], as well as [hh:*] and [*.mmm]
// 时间戳格式：标准 [mm:ss.mm]，同时兼容带小时的 [hh:mm:ss] 与三位毫秒 [mm:ss.mmm]
const timeRegexString = `\[([0-9]{1,2}:)?([0-9]{1,2}):([0-9]{1,2})(.[0-9]{1,3})?\]`

var (
	// Should either be at the beginning of file, or beginning of line
	// syncRegex 用于判定整份歌词是否为同步歌词：时间戳必须位于文件开头或行首
	syncRegex = regexp.MustCompile(`(^|\n)\s*` + timeRegexString)
	timeRegex = regexp.MustCompile(timeRegexString)
	// lrcIdRegex 匹配 LRC 元信息标签：艺人、标题、偏移、语言
	lrcIdRegex = regexp.MustCompile(`\[(ar|ti|offset|lang):([^]]+)]`)
)

// IsEmpty 判断歌词是否为空（无任何行）。
func (l Lyrics) IsEmpty() bool {
	return len(l.Line) == 0
}

// ToLyrics 把纯文本或 LRC 格式的歌词解析为结构化对象。
//
// 解析要点：
//  1. 先用 syncRegex 判定是否为同步歌词，非同步则每行直接作为一条无时间戳的 Line
//  2. 同步模式下采用"延迟提交"策略：读到时间戳时先不产出，
//     而是把随后的文本累积到 priorLine，直到遇到下一个时间戳才把上一行提交。
//     这样才能正确处理跨多行的歌词文本与行内换行
//  3. 支持一行多个时间戳（同一句歌词在多处重复，如副歌），
//     此时同一份文本会为每个时间戳各产出一条 Line
//  4. 出现重复时间戳意味着原文顺序不可靠，最后按时间排序修正
func ToLyrics(language, text string) (*Lyrics, error) {
	text = str.SanitizeText(text)

	lines := strings.Split(text, "\n")
	structuredLines := make([]Line, 0, len(lines)*2)

	artist := ""
	title := ""
	var offset *int64 = nil

	synced := syncRegex.MatchString(text)
	// priorLine 累积尚未提交的歌词文本；validLine 表示已读到过时间戳；
	// repeated 表示出现了一行多时间戳的情况；timestamps 为当前待提交行的时间戳集合
	priorLine := ""
	validLine := false
	repeated := false
	var timestamps []int64

	for _, line := range lines {
		line := strings.TrimSpace(line)
		if line == "" {
			// 空行不丢弃：作为换行并入当前累积文本，保留原有排版
			if validLine {
				priorLine += "\n"
			}
			continue
		}
		var text string
		var time *int64 = nil

		if synced {
			// 先尝试解析元信息标签，命中则本行不作为歌词
			idTag := lrcIdRegex.FindStringSubmatch(line)
			if idTag != nil {
				switch idTag[1] {
				case "ar":
					artist = str.SanitizeText(strings.TrimSpace(idTag[2]))
				case "lang":
					language = str.SanitizeText(strings.TrimSpace(idTag[2]))
				case "offset":
					{
						off, err := strconv.ParseInt(strings.TrimSpace(idTag[2]), 10, 64)
						if err != nil {
							log.Warn("Error parsing offset", "offset", idTag[2], "error", err)
						} else {
							offset = &off
						}
					}
				case "ti":
					title = str.SanitizeText(strings.TrimSpace(idTag[2]))
				}

				continue
			}

			times := timeRegex.FindAllStringSubmatchIndex(line, -1)
			if len(times) > 1 {
				repeated = true
			}

			// The second condition is for when there is a timestamp in the middle of
			// a line (after any text)
			// 无时间戳，或时间戳不在行首（说明它只是歌词文本的一部分），
			// 则整行并入当前累积文本
			if times == nil || times[0][0] != 0 {
				if validLine {
					priorLine += "\n" + line
				}
				continue
			}

			// 读到新的行首时间戳，先把上一行提交：
			// 每个待处理时间戳都产出一条内容相同的 Line
			if validLine {
				for idx := range timestamps {
					structuredLines = append(structuredLines, Line{
						Start: &timestamps[idx],
						Value: strings.TrimSpace(priorLine),
					})
				}
				timestamps = nil
			}

			end := 0

			// [fullStart, fullEnd, hourStart, hourEnd, minStart, minEnd, secStart, secEnd, msStart, msEnd]
			// 收集本行开头连续出现的多个时间戳
			for _, match := range times {
				// for multiple matches, we need to check that later matches are not
				// in the middle of the string
				// 若两个时间戳之间夹有文本，说明后者已属于歌词内容，停止收集
				if end != 0 {
					middle := strings.TrimSpace(line[end:match[0]])
					if middle != "" {
						break
					}
				}

				end = match[1]
				timeInMillis, err := parseTime(line, match)
				if err != nil {
					return nil, err
				}

				timestamps = append(timestamps, timeInMillis)
			}

			// 时间戳之后的部分即为本行歌词文本，作为新的累积起点
			if end >= len(line) {
				priorLine = ""
			} else {
				priorLine = strings.TrimSpace(line[end:])
			}

			validLine = true
		} else {
			text = line
			structuredLines = append(structuredLines, Line{
				Start: time,
				Value: text,
			})
		}
	}

	// 循环结束后提交最后一行（延迟提交策略的收尾）
	if validLine {
		for idx := range timestamps {
			structuredLines = append(structuredLines, Line{
				Start: &timestamps[idx],
				Value: strings.TrimSpace(priorLine),
			})
		}
	}

	// If there are repeated values, there is no guarantee that they are in order
	// In this, case, sort the lyrics by start time
	// 出现一行多时间戳时，产出顺序与实际播放顺序不一致，需按起始时间重排
	if repeated {
		slices.SortFunc(structuredLines, func(a, b Line) int {
			return cmp.Compare(*a.Start, *b.Start)
		})
	}

	lyrics := Lyrics{
		DisplayArtist: artist,
		DisplayTitle:  title,
		Lang:          language,
		Line:          structuredLines,
		Offset:        offset,
		Synced:        synced,
	}
	return &lyrics, nil
}

// parseTime 从正则匹配结果中还原时间戳的毫秒值。
// match 为 FindAllStringSubmatchIndex 的下标数组，布局见调用处注释。
// 小时与毫秒为可选分组（下标为 -1 表示未匹配）。
// 毫秒需按位数补齐：两位表示百分秒（×100 得毫秒计数基准），三位表示千分秒。
func parseTime(line string, match []int) (int64, error) {
	var hours, millis int64
	var err error

	hourStart := match[2]
	if hourStart != -1 {
		// subtract 1 because group has : at the end
		// 该分组包含结尾的冒号，故末位下标减一
		hourEnd := match[3] - 1
		hours, err = strconv.ParseInt(line[hourStart:hourEnd], 10, 64)
		if err != nil {
			return 0, err
		}
	}

	minutes, err := strconv.ParseInt(line[match[4]:match[5]], 10, 64)
	if err != nil {
		return 0, err
	}

	sec, err := strconv.ParseInt(line[match[6]:match[7]], 10, 64)
	if err != nil {
		return 0, err
	}

	msStart := match[8]
	if msStart != -1 {
		msEnd := match[9]
		// +1 offset since this capture group contains .
		// 该分组以小数点开头，故起始下标加一以跳过它
		millis, err = strconv.ParseInt(line[msStart+1:msEnd], 10, 64)
		if err != nil {
			return 0, err
		}

		// 按实际位数换算为毫秒：".5" → 500，".50" → 500，".500" → 500
		length := msEnd - msStart

		if length == 3 {
			millis *= 10
		} else if length == 2 {
			millis *= 100
		}
	}

	timeInMillis := (((((hours * 60) + minutes) * 60) + sec) * 1000) + millis
	return timeInMillis, nil
}

// LyricList 是同一曲目的多份歌词（可对应不同语言），
// 序列化为 JSON 后存入 MediaFile.Lyrics 字段。
type LyricList []Lyrics
