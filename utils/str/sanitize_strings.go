package str

import (
	"html"
	"regexp"
	"slices"
	"strings"

	"github.com/deluan/sanitize"
	"github.com/microcosm-cc/bluemonday"
	"github.com/navidrome/navidrome/conf"
)

var ignoredCharsRegex = regexp.MustCompile("[“”‘’'\"\\[({\\])},]")
var slashRemover = strings.NewReplacer("\\", " ", "/", " ")

// SanitizeStrings 生成用于全文检索的规范化文本：
// 去重音、转小写、剔除标点与斜杠、切词后排序去重。
// 排序去重让「同样的词集合」得到同样的结果，检索时不必在意词序与重复。
func SanitizeStrings(text ...string) string {
	// Concatenate all strings, removing extra spaces
	sanitizedText := strings.Builder{}
	for _, txt := range text {
		sanitizedText.WriteString(strings.TrimSpace(txt))
		sanitizedText.WriteByte(' ')
	}

	// Remove special symbols, accents, extra spaces and slashes
	sanitizedStrings := slashRemover.Replace(Clear(sanitizedText.String()))
	sanitizedStrings = sanitize.Accents(strings.ToLower(sanitizedStrings))
	sanitizedStrings = ignoredCharsRegex.ReplaceAllString(sanitizedStrings, "")
	fullText := strings.Fields(sanitizedStrings)

	// Remove duplicated words
	slices.Sort(fullText)
	fullText = slices.Compact(fullText)

	// Returns the sanitized text as a single string
	return strings.Join(fullText, " ")
}

// policy 用于清洗外部来源的富文本（如艺人简介），防止 XSS。
var policy = bluemonday.UGCPolicy()

// SanitizeText 清洗 HTML 并还原实体字符。
func SanitizeText(text string) string {
	s := policy.Sanitize(text)
	return html.UnescapeString(s)
}

// SanitizeFieldForSorting 生成排序键：去重音转小写，使排序不受大小写与重音影响。
func SanitizeFieldForSorting(originalValue string) string {
	v := strings.TrimSpace(sanitize.Accents(originalValue))
	return Clear(strings.ToLower(v))
}

// SanitizeFieldForSortingNoArticle 排序键额外去掉冠词，
// 让 "The Beatles" 排在 B 而不是 T。
func SanitizeFieldForSortingNoArticle(originalValue string) string {
	v := strings.TrimSpace(sanitize.Accents(originalValue))
	return Clear(strings.ToLower(strings.TrimSpace(RemoveArticle(v))))
}

// RemoveArticle 去掉配置中列出的前置冠词。
func RemoveArticle(name string) string {
	articles := strings.Split(conf.Server.IgnoredArticles, " ")
	for _, a := range articles {
		n := strings.TrimPrefix(name, a+" ")
		if n != name {
			return n
		}
	}
	return name
}
