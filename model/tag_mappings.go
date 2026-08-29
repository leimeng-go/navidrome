package model

import (
	"cmp"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model/criteria"
	"github.com/navidrome/navidrome/resources"
	"gopkg.in/yaml.v3"
)

// mappingsConf 对应 resources/mappings.yaml 的整体结构。
// Main 为一等公民标签（有独立结构体字段），Additional 为附加标签（只存于 tags 列）；
// Roles/Artists 是角色与艺人字段的专用切分配置。
type mappingsConf struct {
	Main       tagMappings `yaml:"main"`
	Additional tagMappings `yaml:"additional"`
	Roles      TagConf     `yaml:"roles"`
	Artists    TagConf     `yaml:"artists"`
}

type tagMappings map[TagName]TagConf

// TagConf 是单个标签的映射配置。
type TagConf struct {
	// Aliases 是该标签在各音频格式中的原始字段名（如 TPE1、ARTIST），全部小写
	Aliases []string `yaml:"aliases"`
	Type    TagType  `yaml:"type"`
	// MaxLength 限制取值长度，超出则截断，防止异常标签占用过多空间
	MaxLength int `yaml:"maxLength"`
	// Split 是多值分隔符列表，用于把 "Rock; Pop" 拆成两个值
	Split []string `yaml:"split"`
	// Album 为 true 表示该标签同时具有专辑级语义，见 AlbumLevelTags
	Album bool `yaml:"album"`
	// SplitRx 是由 Split 预编译出的正则，yaml:"-" 表示不从配置读取
	SplitRx *regexp.Regexp `yaml:"-"`
}

// SplitTagValue splits a tag value by the split separators, but only if it has a single value.
// SplitTagValue 按配置的分隔符切分标签值，仅在原本只有单个值时才处理——
// 若文件本身已提供多值，说明格式支持原生多值，不应再二次切分。
//
// 实现技巧：先把所有分隔符统一替换为零宽空格，再按零宽空格切分。
// 这样一次替换即可支持多种分隔符，无需按分隔符逐轮切分。
func (c TagConf) SplitTagValue(values []string) []string {
	// If there's not exactly one value or no separators, return early.
	if len(values) != 1 || c.SplitRx == nil {
		return values
	}
	tag := values[0]

	// Replace all occurrences of any separator with the zero-width space.
	tag = c.SplitRx.ReplaceAllString(tag, consts.Zwsp)

	// Split by the zero-width space and trim each substring.
	parts := strings.Split(tag, consts.Zwsp)
	for i, part := range parts {
		parts[i] = strings.TrimSpace(part)
	}
	return parts
}

// TagType 是标签值的类型，决定入库前如何解析与校验。
type TagType string

const (
	TagTypeString  TagType = "string"
	TagTypeInteger TagType = "int"
	TagTypeFloat   TagType = "float"
	TagTypeDate    TagType = "date"
	TagTypeUUID    TagType = "uuid"
	TagTypePair    TagType = "pair" // "键=值" 形式的成对取值
)

// TagMappings 返回合并后的全部标签映射（Main + Additional）。
func TagMappings() map[TagName]TagConf {
	mappings, _ := parseMappings()
	return mappings
}

// TagRolesConf 返回角色字段的切分配置。
func TagRolesConf() TagConf {
	_, cfg := parseMappings()
	return cfg.Roles
}

// TagArtistsConf 返回艺人字段的切分配置。
func TagArtistsConf() TagConf {
	_, cfg := parseMappings()
	return cfg.Artists
}

// TagMainMappings 只返回一等公民标签的映射。
func TagMainMappings() map[TagName]TagConf {
	_, mappings := parseMappings()
	return mappings.Main
}

var _mappings mappingsConf

// parseMappings 归一化并合并标签映射，用 sync.OnceValues 保证只执行一次。
// 注意它依赖 loadTagMappings 已填充 _mappings（由 conf 钩子在配置加载后触发）。
var parseMappings = sync.OnceValues(func() (map[TagName]TagConf, mappingsConf) {
	_mappings.Artists.SplitRx = compileSplitRegex("artists", _mappings.Artists.Split)
	_mappings.Roles.SplitRx = compileSplitRegex("roles", _mappings.Roles.Split)

	normalized := tagMappings{}
	collectTags(_mappings.Main, normalized)
	_mappings.Main = normalized

	normalized = tagMappings{}
	collectTags(_mappings.Additional, normalized)
	_mappings.Additional = normalized

	// Merge main and additional mappings, log an error if a tag is found in both
	// 合并两组映射：normalized 此时已含 Additional，再叠加 Main（Main 优先）。
	// 同一标签重复定义属于配置错误，记日志但不中断启动
	for k, v := range _mappings.Main {
		if _, ok := _mappings.Additional[k]; ok {
			log.Error("Tag found in both main and additional mappings", "tag", k)
		}
		normalized[k] = v
	}
	return normalized, _mappings
})

// collectTags 归一化标签配置：标签名与别名统一转小写，并预编译切分正则。
// 非字符串类型不支持切分，遇到此类配置会记录错误并忽略 Split。
func collectTags(tagMappings, normalized map[TagName]TagConf) {
	for k, v := range tagMappings {
		var aliases []string
		for _, val := range v.Aliases {
			aliases = append(aliases, strings.ToLower(val))
		}
		if v.Split != nil {
			if v.Type != "" && v.Type != TagTypeString {
				log.Error("Tag splitting only available for string types", "tag", k, "split", v.Split,
					"type", string(v.Type))
				v.Split = nil
			} else {
				v.SplitRx = compileSplitRegex(k, v.Split)
			}
		}
		v.Aliases = aliases
		normalized[k.ToLower()] = v
	}
}

// compileSplitRegex 把分隔符列表编译成单个不区分大小写的正则。
// 分隔符会被 QuoteMeta 转义，因此可安全使用正则元字符（如 "|"、"."）作为分隔符。
// 无有效分隔符或编译失败时返回 nil，表示不做切分。
func compileSplitRegex(tagName TagName, split []string) *regexp.Regexp {
	// Build a list of escaped, non-empty separators.
	var escaped []string
	for _, s := range split {
		if s == "" {
			continue
		}
		escaped = append(escaped, regexp.QuoteMeta(s))
	}
	// If no valid separators remain, return the original value.
	if len(escaped) == 0 {
		if len(split) > 0 {
			log.Warn("No valid separators found in split list", "split", split, "tag", tagName)
		}
		return nil
	}

	// Create one regex that matches any of the separators (case-insensitive).
	pattern := "(?i)(" + strings.Join(escaped, "|") + ")"
	re, err := regexp.Compile(pattern)
	if err != nil {
		log.Warn("Error compiling regexp for split list", "pattern", pattern, "tag", tagName, "split", split, err)
		return nil
	}
	return re
}

// tagNames 返回全部标签名，供智能播放列表的可用字段列表使用。
func tagNames() []string {
	mappings := TagMappings()
	names := make([]string, 0, len(mappings))
	for k := range mappings {
		names = append(names, string(k))
	}
	return names
}

// numericTagNames 只返回数值类型的标签名，
// 智能播放列表据此决定该字段可用哪些比较运算符（如大于、小于）。
func numericTagNames() []string {
	mappings := TagMappings()
	names := make([]string, 0)
	for k, cfg := range mappings {
		if cfg.Type == TagTypeInteger || cfg.Type == TagTypeFloat {
			names = append(names, string(k))
		}
	}
	return names
}

// loadTagMappings 从内嵌的 mappings.yaml 载入默认映射，
// 再叠加用户配置中的 Tags 覆盖项。解析失败只记录错误不中断启动。
func loadTagMappings() {
	mappingsFile, err := resources.FS().Open("mappings.yaml")
	if err != nil {
		log.Error("Error opening mappings.yaml", err)
	}
	decoder := yaml.NewDecoder(mappingsFile)
	err = decoder.Decode(&_mappings)
	if err != nil {
		log.Error("Error decoding mappings.yaml", err)
	}
	if len(_mappings.Main) == 0 {
		log.Error("No tag mappings found in mappings.yaml, check the format")
	}

	// Use Scanner.GenreSeparators if specified and Tags.genre is not defined
	// 兼容已废弃的 Scanner.GenreSeparators 配置：仅当用户未通过新的
	// Tags.genre 方式配置时才生效。该配置按单字符切分
	if conf.Server.Scanner.GenreSeparators != "" && len(conf.Server.Tags["genre"].Aliases) == 0 {
		genreConf := _mappings.Main[TagName("genre")]
		genreConf.Split = strings.Split(conf.Server.Scanner.GenreSeparators, "")
		genreConf.SplitRx = compileSplitRegex("genre", genreConf.Split)
		_mappings.Main[TagName("genre")] = genreConf
		log.Debug("Loading deprecated list of genre separators", "separators", genreConf.Split)
	}

	// Overwrite the default mappings with the ones from the config
	// 用用户配置覆盖默认映射：
	//   - Ignore 为 true 直接删除该标签，使其不被导入
	//   - 未显式给出的字段（别名、切分符等）沿用默认值，实现按字段级增量覆盖
	//   - 覆盖后写回原所属分组（Main 或 Additional），不改变标签的层级归属
	for tag, cfg := range conf.Server.Tags {
		if cfg.Ignore {
			delete(_mappings.Main, TagName(tag))
			delete(_mappings.Additional, TagName(tag))
			continue
		}
		oldValue, ok := _mappings.Main[TagName(tag)]
		if !ok {
			oldValue = _mappings.Additional[TagName(tag)]
		}
		aliases := cfg.Aliases
		if len(aliases) == 0 {
			aliases = oldValue.Aliases
		}
		split := cfg.Split
		if split == nil {
			split = oldValue.Split
		}
		c := TagConf{
			Aliases:   aliases,
			Split:     split,
			Type:      cmp.Or(TagType(cfg.Type), oldValue.Type),
			MaxLength: cmp.Or(cfg.MaxLength, oldValue.MaxLength),
			Album:     cmp.Or(cfg.Album, oldValue.Album),
		}
		c.SplitRx = compileSplitRegex(TagName(tag), c.Split)
		if _, ok := _mappings.Main[TagName(tag)]; ok {
			_mappings.Main[TagName(tag)] = c
		} else {
			_mappings.Additional[TagName(tag)] = c
		}
	}
}

// init 注册配置加载后的钩子。之所以放在钩子里而非包初始化时直接执行，
// 是因为标签映射依赖用户配置，必须等配置就绪后才能计算。
func init() {
	conf.AddHook(func() {
		loadTagMappings()

		// This is here to avoid cyclic imports. The criteria package needs to know all tag names, so they can be
		// used in smart playlists
		// 放在此处是为了打破循环依赖：criteria 包需要知道全部标签名与角色名
		// 才能校验智能播放列表规则，但它不能反向导入 model 包，
		// 因此改由 model 在初始化时把这些信息注册进去
		criteria.AddRoles(slices.Collect(maps.Keys(AllRoles)))
		criteria.AddTagNames(tagNames())
		criteria.AddNumericTags(numericTagNames())
	})
}
