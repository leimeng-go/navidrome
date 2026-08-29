// Package hasher 提供带种子的字符串哈希，用于可复现的随机排序。
//
// 「随机排序」若每次查询结果都变，分页时会出现重复或遗漏条目。
// 因此以种子哈希曲目 ID 作为排序键：同一种子下顺序稳定，换种子即换一套顺序。
package hasher

import (
	"hash/maphash"
	"strconv"
	"sync"

	"github.com/navidrome/navidrome/utils/random"
)

var instance = NewHasher()

// Reseed 为指定 ID 重新生成随机种子，相当于「重新洗牌」。
func Reseed(id string) {
	instance.Reseed(id)
}

// SetSeed 显式设置种子，用于跨请求复现同一顺序。
func SetSeed(id string, seed string) {
	instance.SetSeed(id, seed)
}

// CurrentSeed 返回当前种子。
func CurrentSeed(id string) string {
	instance.mutex.RLock()
	defer instance.mutex.RUnlock()
	return instance.seeds[id]
}

func HashFunc() func(id, str string) uint64 {
	return instance.HashFunc()
}

// Hasher 按 ID 维护各自的种子。
type Hasher struct {
	seeds    map[string]string
	mutex    sync.RWMutex
	hashSeed maphash.Seed
}

func NewHasher() *Hasher {
	h := new(Hasher)
	h.seeds = make(map[string]string)
	h.hashSeed = maphash.MakeSeed()
	return h
}

// SetSeed sets a seed for the given id
func (h *Hasher) SetSeed(id string, seed string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.seeds[id] = seed
}

// Reseed generates a new random seed for the given id
func (h *Hasher) Reseed(id string) {
	_ = h.reseed(id)
}

func (h *Hasher) reseed(id string) string {
	seed := strconv.FormatUint(random.Uint64(), 36)
	h.SetSeed(id, seed)
	return seed
}

// HashFunc returns a function that hashes a string using the seed for the given id
// HashFunc 返回哈希函数，注册给 SQLite 作为 SEEDEDRAND。
// 种子不存在时自动生成，使调用方无需预先初始化。
func (h *Hasher) HashFunc() func(id, str string) uint64 {
	return func(id, str string) uint64 {
		h.mutex.RLock()
		seed, ok := h.seeds[id]
		h.mutex.RUnlock()
		if !ok {
			seed = h.reseed(id)
		}

		return maphash.Bytes(h.hashSeed, []byte(seed+str))
	}
}
