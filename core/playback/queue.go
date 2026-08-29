package playback

import (
	"fmt"
	"math/rand"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// Queue 是点唱机的播放队列。
// Index 为 -1 表示队列为空或无当前曲目。
type Queue struct {
	Index int
	Items model.MediaFiles
}

// NewQueue 创建空队列。
func NewQueue() *Queue {
	return &Queue{
		Index: -1,
		Items: model.MediaFiles{},
	}
}

func (pd *Queue) String() string {
	filenames := ""
	for idx, item := range pd.Items {
		filenames += fmt.Sprint(idx) + ":" + item.Path + " "
	}
	return fmt.Sprintf("#Items: %d, idx: %d, files: %s", len(pd.Items), pd.Index, filenames)
}

// returns the current mediafile or nil
// Current 返回当前曲目，队列为空或下标越界时返回 nil。
func (pd *Queue) Current() *model.MediaFile {
	if pd.Index == -1 {
		return nil
	}
	if pd.Index >= len(pd.Items) {
		log.Error("internal error: current song index out of bounds", "idx", pd.Index, "length", len(pd.Items))
		return nil
	}

	return &pd.Items[pd.Index]
}

// returns the whole queue
// Get 返回整个队列。
func (pd *Queue) Get() model.MediaFiles {
	return pd.Items
}

// Size 返回队列长度。
func (pd *Queue) Size() int {
	return len(pd.Items)
}

// IsEmpty 判断队列是否为空。
func (pd *Queue) IsEmpty() bool {
	return len(pd.Items) < 1
}

// set is similar to a clear followed by a add, but will not change the currently playing track.
// Set 用新列表替换队列内容。
func (pd *Queue) Set(items model.MediaFiles) {
	pd.Clear()
	pd.Items = append(pd.Items, items...)
}

// adding mediafiles to the queue
// Add 追加曲目。队列原本为空时自动把下标指向第一首。
func (pd *Queue) Add(items model.MediaFiles) {
	pd.Items = append(pd.Items, items...)
	if pd.Index == -1 && len(pd.Items) > 0 {
		pd.Index = 0
	}
}

// empties whole queue
// Clear 清空队列并重置下标。
func (pd *Queue) Clear() {
	pd.Index = -1
	pd.Items = nil
}

// idx Zero-based index of the song to skip to or remove.
//
// Remove 移除指定位置的曲目。
// 先记下当前曲目 ID，移除后按 ID 重新定位下标，
// 使「当前播放的是哪一首」不因下标位移而改变。
func (pd *Queue) Remove(idx int) {
	current := pd.Current()
	backupID := ""
	if current != nil {
		backupID = current.ID
	}

	pd.Items = append(pd.Items[:idx], pd.Items[idx+1:]...)

	var err error
	pd.Index, err = pd.getMediaFileIndexByID(backupID)
	if err != nil {
		// we seem to have deleted the current id, setting to default:
		pd.Index = -1
	}
}

// Shuffle 打乱队列，并按 ID 重新定位下标，使当前曲目继续播放不被打断。
func (pd *Queue) Shuffle() {
	current := pd.Current()
	backupID := ""
	if current != nil {
		backupID = current.ID
	}

	rand.Shuffle(len(pd.Items), func(i, j int) { pd.Items[i], pd.Items[j] = pd.Items[j], pd.Items[i] })

	var err error
	pd.Index, err = pd.getMediaFileIndexByID(backupID)
	if err != nil {
		log.Error("Could not find ID while shuffling: %s", backupID)
	}
}

// getMediaFileIndexByID 按 ID 查找曲目在队列中的位置。
func (pd *Queue) getMediaFileIndexByID(id string) (int, error) {
	for idx, item := range pd.Items {
		if item.ID == id {
			return idx, nil
		}
	}
	return -1, fmt.Errorf("ID not found in playlist: %s", id)
}

// Sets the index to a new, valid value inside the Items. Values lower than zero are going to be zero,
// values above will be limited by number of items.
//
// SetIndex 设置下标并自动钳制到合法范围，使调用方无需自行校验越界。
func (pd *Queue) SetIndex(idx int) {
	pd.Index = max(0, min(idx, len(pd.Items)-1))
}

// Are we at the last track?
// IsAtLastElement 判断是否已在最后一首。
func (pd *Queue) IsAtLastElement() bool {
	return (pd.Index + 1) >= len(pd.Items)
}

// Goto next index
// IncreaseIndex 前进到下一首，已在末尾则不动。
func (pd *Queue) IncreaseIndex() {
	if !pd.IsAtLastElement() {
		pd.SetIndex(pd.Index + 1)
	}
}
