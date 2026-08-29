// Package metadata 负责把音频文件中读出的原始标签转换为领域模型。
//
// 数据流为：
//
//	扫描器读取文件 → Info（原始标签 + 文件信息 + 音频属性）
//	  → New() 经 clean() 规范化 → Metadata
//	  → ToMediaFile() 映射为 model.MediaFile
//
// clean() 是本包的核心：它依据 mappings.yaml 的配置把千奇百怪的
// 格式专属标签名（ID3v2 / Vorbis / MP4 等）归一为统一的标签名，
// 并完成拆分、去重、类型校验与截断。
package metadata

import (
	"cmp"
	"io/fs"
	"math"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
)

// Info 是标签提取器（taglib 等适配器）的输出，即文件的原始元数据。
type Info struct {
	FileInfo        FileInfo
	Tags            model.RawTags
	AudioProperties AudioProperties
	HasPicture      bool
}

// FileInfo 在标准 fs.FileInfo 之上增加创建时间。
// 创建时间用于推断曲目的「加入时间」，比修改时间更贴近用户预期。
type FileInfo interface {
	fs.FileInfo
	BirthTime() time.Time
}

// AudioProperties 是从音频流本身解析出的技术属性（非标签）。
type AudioProperties struct {
	Duration   time.Duration
	BitRate    int
	BitDepth   int
	SampleRate int
	Channels   int
}

// Date 是形如 "2006"、"2006-01" 或 "2006-01-02" 的日期字符串。
// 保留字符串而非 time.Time，是因为音乐标签中的日期精度不一，
// 很多只有年份，转换为 time.Time 会凭空补出不存在的月日。
type Date string

// Year 取日期中的年份部分，无值时返回 0。
func (d Date) Year() int {
	if d == "" {
		return 0
	}
	y, _ := strconv.Atoi(string(d[:4]))
	return y
}

// Pair 表示「键值对」型标签的一个条目，如参与者标签 "guitar → John"。
// 内部用零宽空格（Zwsp）分隔键与值，因为该字符几乎不会出现在真实标签中，
// 可安全地把键值对塞进单个字符串，从而复用普通标签的存储结构。
type Pair string

func (p Pair) Key() string   { return p.parse(0) }
func (p Pair) Value() string { return p.parse(1) }
func (p Pair) parse(i int) string {
	parts := strings.SplitN(string(p), consts.Zwsp, 2)
	if len(parts) > i {
		return parts[i]
	}
	return ""
}
func (p Pair) String() string {
	return string(p)
}

// NewPair 用零宽空格拼接键值对。
func NewPair(key, value string) string {
	return key + consts.Zwsp + value
}

// New 构造 Metadata，构造时即完成标签规范化（clean）。
func New(filePath string, info Info) Metadata {
	return Metadata{
		filePath:   filePath,
		fileInfo:   info.FileInfo,
		tags:       clean(filePath, info.Tags),
		audioProps: info.AudioProperties,
		hasPicture: info.HasPicture,
	}
}

// Metadata 是规范化后的文件元数据。
// 字段全部私有，只通过下面一组带类型的取值方法访问，
// 使调用方无需关心标签值都是字符串这一底层事实。
type Metadata struct {
	filePath   string
	fileInfo   FileInfo
	tags       model.Tags
	audioProps AudioProperties
	hasPicture bool
}

// 以下是按类型取值的便捷方法。标签值在底层都是字符串，
// 解析失败时统一返回零值而不报错——元数据是「尽力而为」的，
// 单个标签格式错误不应导致整个文件无法导入。
func (md Metadata) FilePath() string     { return md.filePath }
func (md Metadata) ModTime() time.Time   { return md.fileInfo.ModTime() }
func (md Metadata) BirthTime() time.Time { return md.fileInfo.BirthTime() }
func (md Metadata) Size() int64          { return md.fileInfo.Size() }

// Suffix 返回小写且不含点的扩展名，用作曲目格式标识。
func (md Metadata) Suffix() string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(md.filePath), "."))
}
func (md Metadata) AudioProperties() AudioProperties { return md.audioProps }

// Length 返回以秒为单位的时长（由毫秒换算，保留小数）。
func (md Metadata) Length() float32                          { return float32(md.audioProps.Duration.Milliseconds()) / 1000 }
func (md Metadata) HasPicture() bool                         { return md.hasPicture }
func (md Metadata) All() model.Tags                          { return md.tags }
func (md Metadata) Strings(key model.TagName) []string       { return md.tags[key] }
func (md Metadata) String(key model.TagName) string          { return md.first(key) }
func (md Metadata) Int(key model.TagName) int64              { v, _ := strconv.Atoi(md.first(key)); return int64(v) }
func (md Metadata) Bool(key model.TagName) bool              { v, _ := strconv.ParseBool(md.first(key)); return v }
func (md Metadata) Date(key model.TagName) Date              { return md.date(key) }
func (md Metadata) NumAndTotal(key model.TagName) (int, int) { return md.tuple(key) }
func (md Metadata) Float(key model.TagName, def ...float64) float64 {
	return float(md.first(key), def...)
}
func (md Metadata) NullableFloat(key model.TagName) *float64 { return nullableFloat(md.first(key)) }

// Gain 解析 ReplayGain 增益值。标签中常带 "dB" 后缀（如 "-7.50 dB"），
// 需先剥离才能解析为数字。返回指针以区分「无此标签」与「增益为 0」。
func (md Metadata) Gain(key model.TagName) *float64 {
	v := strings.TrimSpace(strings.Replace(md.first(key), "dB", "", 1))
	return nullableFloat(v)
}

// Pairs 返回键值对型标签的全部条目。
func (md Metadata) Pairs(key model.TagName) []Pair {
	values := md.tags[key]
	return slice.Map(values, func(v string) Pair { return Pair(v) })
}

// first 取标签的首个值。标签天然多值，单值语义的字段取第一个。
func (md Metadata) first(key model.TagName) string {
	if v, ok := md.tags[key]; ok && len(v) > 0 {
		return v[0]
	}
	return ""
}

// float 解析浮点数，失败时用给定默认值，未给默认值则返回 0。
func float(value string, def ...float64) float64 {
	v := nullableFloat(value)
	if v != nil {
		return *v
	}
	if len(def) > 0 {
		return def[0]
	}
	return 0
}

// nullableFloat 解析浮点数，无效值返回 nil。
// 除解析失败外，Inf 与 NaN 也视为无效——它们无法写入数据库，
// 且多来自损坏的标签，放行会污染后续计算。
func nullableFloat(value string) *float64 {
	v, err := strconv.ParseFloat(value, 64)
	if err != nil || v == math.Inf(-1) || math.IsInf(v, 1) || math.IsNaN(v) {
		return nil
	}
	return &v
}

// Used for tracks and discs
// tuple 解析「序号/总数」型标签，用于音轨号与碟号。
// 支持两种写法：值内含斜杠（"3/12"），
// 或总数放在单独的 xxxtotal 标签中（如 tracktotal）。
func (md Metadata) tuple(key model.TagName) (int, int) {
	tag := md.first(key)
	if tag == "" {
		return 0, 0
	}
	tuple := strings.Split(tag, "/")
	t1, t2 := 0, 0
	t1, _ = strconv.Atoi(tuple[0])
	if len(tuple) > 1 {
		t2, _ = strconv.Atoi(tuple[1])
	} else {
		t2tag := md.first(key + "total")
		t2, _ = strconv.Atoi(t2tag)
	}
	return t1, t2
}

// dateRegex 匹配 1000–2999 范围内的四位年份，用于从杂乱日期串中兜底提取年份。
var dateRegex = regexp.MustCompile(`([12]\d\d\d)`)

func (md Metadata) date(tagName model.TagName) Date {
	return Date(md.first(tagName))
}

// date tries to parse a date from a tag, it tries to get at least the year. See the tests for examples.
// parseDate 尽力从标签值中解析日期，至少保证拿到年份。
//
// 策略是逐步降级：先用正则确认存在四位年份（拿不到就彻底放弃），
// 若原值就只有年份则直接返回；否则截断到 10 字符后尝试完整日期与年月两种格式，
// 都失败就退回到仅年份。这样既能吃下 "2006-01-02T00:00:00Z" 这类冗长值，
// 也不会因月日无法解析而丢掉已经拿到的年份。
func parseDate(filePath string, tagName model.TagName, tagValue string) string {
	if len(tagValue) < 4 {
		return ""
	}

	// first get just the year
	// 先确认能提取出年份，提取不到说明该值不是日期
	match := dateRegex.FindStringSubmatch(tagValue)
	if len(match) == 0 {
		log.Debug("Error parsing date", "file", filePath, "tag", tagName, "date", tagValue)
		return ""
	}

	// if the tag is just the year, return it
	// 仅有年份，直接返回
	if len(tagValue) < 5 {
		return match[1]
	}

	// if the tag is too long, truncate it
	// 过长的值截断到 YYYY-MM-DD 长度，丢弃时刻与时区部分
	tagValue = tagValue[:min(10, len(tagValue))]

	// then try to parse the full date
	// 依次尝试完整日期与年月，都失败则降级为仅年份
	for _, mask := range []string{"2006-01-02", "2006-01"} {
		_, err := time.Parse(mask, tagValue)
		if err == nil {
			return tagValue
		}
	}
	log.Debug("Error parsing month and day from date", "file", filePath, "tag", tagName, "date", tagValue)
	return match[1]
}

// clean filters out tags that are not in the mappings or are empty,
// combine equivalent tags and remove duplicated values.
// It keeps the order of the tags names as they are defined in the mappings.
// clean 依据 mappings.yaml 把原始标签规范化：
// 丢弃未在映射中声明的标签、合并等价标签（多个别名归到同一标签名）、
// 拆分多值、去重去空，最后逐值做类型校验与截断。
//
// 这一步是「白名单」语义：只有映射表中定义的标签才会被保留，
// 从而保证下游拿到的标签集合是可预期的。
func clean(filePath string, tags model.RawTags) model.Tags {
	// 标签名大小写在各格式中不统一，先统一转小写再按别名匹配
	lowered := lowerTags(tags)
	mappings := model.TagMappings()
	cleaned := make(model.Tags, len(mappings))

	for name, mapping := range mappings {
		var values []string
		switch mapping.Type {
		case model.TagTypePair:
			values = processPairMapping(name, mapping, lowered)
		default:
			values = processRegularMapping(mapping, lowered)
		}
		cleaned[name] = values
	}

	cleaned = filterEmptyTags(cleaned)
	return sanitizeAll(filePath, cleaned)
}

// processRegularMapping 收集普通标签：按声明顺序遍历所有别名，
// 命中的值经 SplitTagValue 拆分后依次追加，因此别名顺序决定了值的优先次序。
func processRegularMapping(mapping model.TagConf, lowered model.Tags) []string {
	var values []string
	for _, alias := range mapping.Aliases {
		if vs, ok := lowered[model.TagName(alias)]; ok {
			splitValues := mapping.SplitTagValue(vs)
			values = append(values, splitValues...)
		}
	}
	return values
}

// lowerTags 把原始标签名统一转为小写，以便与映射表中的别名匹配。
func lowerTags(tags model.RawTags) model.Tags {
	lowered := make(model.Tags, len(tags))
	for k, v := range tags {
		lowered[model.TagName(strings.ToLower(k))] = v
	}
	return lowered
}

// processPairMapping 收集键值对型标签（如参与者、歌词）。
// 两种来源格式并存：ID3 用 "标签名:键" 的形式拆成多个标签，
// Vorbis 则把键写在值的括号里，故需分别解析后合并。
func processPairMapping(name model.TagName, mapping model.TagConf, lowered model.Tags) []string {
	var aliasValues []string
	for _, alias := range mapping.Aliases {
		if vs, ok := lowered[model.TagName(alias)]; ok {
			aliasValues = append(aliasValues, vs...)
		}
	}

	// always parse id3 pairs. For lyrics, Taglib appears to always provide lyrics:xxx
	// Prefer that over format-specific tags
	// ID3 风格始终解析并置于前面：taglib 对歌词等标签总是输出 "lyrics:xxx" 形式，
	// 优先采信它，格式专属标签作为补充
	id3Base := parseID3Pairs(name, lowered)

	if len(aliasValues) > 0 {
		id3Base = append(id3Base, parseVorbisPairs(aliasValues)...)
	}
	return id3Base
}

// parseID3Pairs 解析 "标签名:键" 形式的标签，如 "performer:guitar"。
// 键与标签名相同时（如 "lyrics:lyrics"）视为无键，置空以表示默认条目。
func parseID3Pairs(name model.TagName, lowered model.Tags) []string {
	var pairs []string
	prefix := string(name) + ":"
	for tagKey, tagValues := range lowered {
		keyStr := string(tagKey)
		if strings.HasPrefix(keyStr, prefix) {
			keyPart := strings.TrimPrefix(keyStr, prefix)
			if keyPart == string(name) {
				keyPart = ""
			}
			for _, v := range tagValues {
				pairs = append(pairs, NewPair(keyPart, v))
			}
		}
	}
	return pairs
}

// vorbisPairRegex 匹配值末尾括号中的键，且允许键内再嵌一层括号，
// 以正确处理 "drums (drum set) and organ" 这类嵌套写法。
var vorbisPairRegex = regexp.MustCompile(`\(([^()]+(?:\([^()]*\)[^()]*)*)\)`)

// parseVorbisPairs, from
//
//	"Salaam Remi (drums (drum set) and organ)",
//
// to
//
//	"drums (drum set) and organ" -> "Salaam Remi",
//
// parseVorbisPairs 解析 Vorbis 风格的键值对：键写在括号内，括号外为值。
// 无括号时视为无键条目。键统一转小写以便后续按角色归类。
func parseVorbisPairs(values []string) []string {
	pairs := make([]string, 0, len(values))
	for _, value := range values {
		matches := vorbisPairRegex.FindAllStringSubmatch(value, -1)
		if len(matches) == 0 {
			pairs = append(pairs, NewPair("", value))
			continue
		}
		key := strings.TrimSpace(matches[0][1])
		key = strings.ToLower(key)
		valueWithoutKey := strings.TrimSpace(strings.Replace(value, "("+matches[0][1]+")", "", 1))
		pairs = append(pairs, NewPair(key, valueWithoutKey))
	}
	return pairs
}

// filterEmptyTags 去重去空，并删除清理后无值的标签键。
// 直接就地修改传入的 map（在 clean 中该 map 是本地新建的，安全）。
func filterEmptyTags(tags model.Tags) model.Tags {
	for k, v := range tags {
		clean := filterDuplicatedOrEmptyValues(v)
		if len(clean) == 0 {
			delete(tags, k)
		} else {
			tags[k] = clean
		}
	}
	return tags
}

// filterDuplicatedOrEmptyValues 保序去重并剔除空值。
// 保序很重要：首个值会被当作单值语义字段的取值（见 Metadata.first）。
func filterDuplicatedOrEmptyValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	var result []string
	for _, v := range values {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		result = append(result, v)
	}
	return result
}

// sanitizeAll 对每个标签的每个值做校验与截断，
// 校验失败的值被丢弃，全部值都无效的标签整体删除。
func sanitizeAll(filePath string, tags model.Tags) model.Tags {
	cleaned := model.Tags{}
	for k, v := range tags {
		tag, found := model.TagMappings()[k]
		if !found {
			continue
		}

		var values []string
		for _, value := range v {
			cleanedValue := sanitize(filePath, k, tag, value)
			if cleanedValue != "" {
				values = append(values, cleanedValue)
			}
		}
		if len(values) > 0 {
			cleaned[k] = values
		}
	}
	return cleaned
}

// defaultMaxTagLength 是标签值的默认长度上限，防止损坏文件中的超长标签
// 撑爆数据库或界面。映射表中可按标签单独覆盖。
const defaultMaxTagLength = 1024

// sanitize 校验并规整单个标签值。
//
// 先截断长度，再按声明类型校验：
// 日期类做尽力解析（失败仍可能保留年份），
// 整数/浮点/UUID 类校验失败则直接丢弃该值——
// 这些类型的脏数据会导致下游查询或排序出错，宁可不要。
func sanitize(filePath string, tagName model.TagName, tag model.TagConf, value string) string {
	// First truncate the value to the maximum length
	// 先按上限截断，cmp.Or 在未配置时回退到默认值
	maxLength := cmp.Or(tag.MaxLength, defaultMaxTagLength)
	if len(value) > maxLength {
		log.Trace("Truncated tag value", "tag", tagName, "value", value, "length", len(value), "maxLength", maxLength)
		value = value[:maxLength]
	}

	switch tag.Type {
	case model.TagTypeDate:
		value = parseDate(filePath, tagName, value)
		if value == "" {
			log.Trace("Invalid date tag value", "tag", tagName, "value", value)
		}
	case model.TagTypeInteger:
		_, err := strconv.Atoi(value)
		if err != nil {
			log.Trace("Invalid integer tag value", "tag", tagName, "value", value)
			return ""
		}
	case model.TagTypeFloat:
		_, err := strconv.ParseFloat(value, 64)
		if err != nil {
			log.Trace("Invalid float tag value", "tag", tagName, "value", value)
			return ""
		}
	case model.TagTypeUUID:
		_, err := uuid.Parse(value)
		if err != nil {
			log.Trace("Invalid UUID tag value", "tag", tagName, "value", value)
			return ""
		}
	}
	return value
}
