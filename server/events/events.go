package events

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"time"
	"unicode"
)

type eventCtxKey string

const broadcastToAllKey eventCtxKey = "broadcastToAll"

// broadcastToAll is a context key that can be used to broadcast an event to all clients
func broadcastToAll(ctx context.Context) context.Context {
	return context.WithValue(ctx, broadcastToAllKey, true)
}

// Event 是可推送给前端的事件。
// Name/Data 接收事件自身作为参数，以便 baseEvent 用反射拿到具体类型。
type Event interface {
	Name(Event) string
	Data(Event) string
}

// baseEvent 为所有事件提供默认的名称与序列化实现，内嵌即可复用。
type baseEvent struct{}

// Name 由具体类型名推导事件名：去掉包前缀并首字母小写。
func (e *baseEvent) Name(evt Event) string {
	str := strings.TrimPrefix(reflect.TypeOf(evt).String(), "*events.")
	return str[:0] + string(unicode.ToLower(rune(str[0]))) + str[1:]
}

// Data 把事件序列化为 JSON。
func (e *baseEvent) Data(evt Event) string {
	data, _ := json.Marshal(evt)
	return string(data)
}

// ScanStatus 上报扫描进度。
type ScanStatus struct {
	baseEvent
	Scanning    bool          `json:"scanning"`
	Count       int64         `json:"count"`
	FolderCount int64         `json:"folderCount"`
	Error       string        `json:"error"`
	ScanType    string        `json:"scanType"`
	ElapsedTime time.Duration `json:"elapsedTime"`
}

// KeepAlive 是心跳事件，用于维持长连接。
type KeepAlive struct {
	baseEvent
	TS int64 `json:"ts"`
}

// ServerStart 在客户端接入时发送，便于前端识别服务端重启。
type ServerStart struct {
	baseEvent
	StartTime time.Time `json:"startTime"`
	Version   string    `json:"version"`
}

// Any 表示某类资源的全部条目。
const Any = "*"

// RefreshResource 通知前端某些资源已变更、需要重新拉取。
type RefreshResource struct {
	baseEvent
	resources map[string][]string
}

// NowPlayingCount 上报当前正在播放的客户端数量。
type NowPlayingCount struct {
	baseEvent
	Count int `json:"count"`
}

// With 追加需要刷新的资源。不指定 ID 时表示该类资源整体失效。
func (rr *RefreshResource) With(resource string, ids ...string) *RefreshResource {
	if rr.resources == nil {
		rr.resources = make(map[string][]string)
	}
	if len(ids) == 0 {
		rr.resources[resource] = append(rr.resources[resource], Any)
	}
	rr.resources[resource] = append(rr.resources[resource], ids...)
	return rr
}

// Data 序列化待刷新资源。未指定任何资源时退化为「全部刷新」。
func (rr *RefreshResource) Data(evt Event) string {
	if rr.resources == nil {
		return `{"*":"*"}`
	}
	r := evt.(*RefreshResource)
	data, _ := json.Marshal(r.resources)
	return string(data)
}
