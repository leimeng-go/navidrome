// Package id 提供实体标识符的生成算法。
//
// 两类 ID：
//   - NewRandom：随机 ID，用于无天然唯一键的实体（播放列表、分享等）
//   - NewHash：内容哈希 ID，用于需要「相同内容得到相同 ID」的实体
//     （曲目、专辑、艺人、标签），以便重复扫描时幂等。
//
// 两者都是 22 字符、62 进制字符集，长度一致便于统一存储与展示。
package id

import (
	"crypto/md5"
	"fmt"
	"math/big"
	"strings"

	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/navidrome/navidrome/log"
)

// NewRandom 生成 22 位随机 ID。
// 字符集限定为数字与字母，使 ID 可安全用于 URL 与文件名。
// 生成失败仅记录日志并返回空串——nanoid 只在系统熵源异常时失败，
// 此处不使调用方承担错误处理成本。
func NewRandom() string {
	id, err := gonanoid.Generate("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", 22)
	if err != nil {
		log.Error("Could not generate new ID", err)
	}
	return id
}

// NewHash 由任意多个字符串生成确定性 ID。
//
// 每段数据后追加零宽空格作为分隔符，避免拼接歧义：
// 否则 ("ab","c") 与 ("a","bc") 会得到相同哈希。
// 选零宽空格是因为它几乎不会出现在真实的标签或名称中。
//
// MD5 结果被当作大整数转为 62 进制并左补零到 22 位，
// 相比十六进制更短，且长度与 NewRandom 一致。
// 此处 MD5 仅用于生成标识符，不涉及安全用途。
func NewHash(data ...string) string {
	hash := md5.New()
	for _, d := range data {
		hash.Write([]byte(d))
		hash.Write([]byte(string('\u200b')))
	}
	h := hash.Sum(nil)
	bi := big.NewInt(0)
	bi.SetBytes(h)
	s := bi.Text(62)
	return fmt.Sprintf("%022s", s)
}

// NewTagID 生成标签 ID。名字与值都转小写，
// 使大小写不同的同一标签（如 "Rock" 与 "rock"）归并为同一条目。
func NewTagID(name, value string) string {
	return NewHash(strings.ToLower(name), strings.ToLower(value))
}
