package local

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"github.com/navidrome/navidrome/log"
	"github.com/rjeczalik/notify"
)

// Start starts a watcher on the whole FS and returns a channel to send detected changes.
// It uses `notify` to detect changes in the filesystem, so it may not work on all platforms/use-cases.
// Notoriously, it does not work on some networked mounts and Windows with WSL2.
//
// Start 启动文件系统监听，返回变更路径的通道。
//
// 用原子标志防止重复启动。
// started 通道用于等待监听真正就绪后再返回，
// 否则调用方可能错过启动瞬间发生的变更。
// 上报路径需把软链接真实路径换算回配置路径，使调用方看到的路径始终一致。
func (s *localStorage) Start(ctx context.Context) (<-chan string, error) {
	if !s.watching.CompareAndSwap(false, true) {
		return nil, errors.New("watcher already started")
	}
	input := make(chan notify.EventInfo, 1)
	output := make(chan string, 1)

	started := make(chan struct{})
	go func() {
		defer close(input)
		defer close(output)

		libPath := filepath.Join(s.u.Path, "...")
		log.Debug(ctx, "Starting watcher", "lib", libPath)
		err := notify.Watch(libPath, input, WatchEvents)
		if err != nil {
			log.Error("Error starting watcher", "lib", libPath, err)
			return
		}
		defer notify.Stop(input)
		close(started) // signals the main goroutine we have started

		for {
			select {
			case event := <-input:
				log.Trace(ctx, "Detected change", "event", event, "lib", s.u.Path)
				name := event.Path()
				name = strings.Replace(name, s.resolvedPath, s.u.Path, 1)
				output <- name
			case <-ctx.Done():
				log.Debug(ctx, "Stopping watcher", "path", s.u.Path)
				s.watching.Store(false)
				return
			}
		}
	}()
	select {
	case <-started:
	case <-ctx.Done():
	}
	return output, nil
}
