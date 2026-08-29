package mpv

// Audio-playback using mpv media-server. See mpv.io
// https://github.com/dexterlb/mpvipc
// https://mpv.io/manual/master/#json-ipc
// https://mpv.io/manual/master/#properties

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/dexterlb/mpvipc"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
)

// MpvTrack 是通过 mpv 的 JSON IPC 接口控制的一路音轨。
// CloseCalled 用于区分「主动关闭」与「自然播放结束」，避免主动关闭时误报播放完成。
type MpvTrack struct {
	MediaFile     model.MediaFile
	PlaybackDone  chan bool
	Conn          *mpvipc.Connection
	IPCSocketName string
	Exe           *Executor
	CloseCalled   bool
}

// NewTrack 启动一个 mpv 进程加载指定曲目，并建立 IPC 连接。
//
// mpv 启动后才会创建控制 socket，故需轮询等待其出现（最多 3 秒）后再连接。
// 另起协程监听连接关闭：连接断开即代表播放结束，据此通知设备切下一首。
func NewTrack(ctx context.Context, playbackDoneChannel chan bool, deviceName string, mf model.MediaFile) (*MpvTrack, error) {
	log.Debug("Loading track", "trackPath", mf.Path, "mediaType", mf.ContentType())

	if _, err := mpvCommand(); err != nil {
		return nil, err
	}

	tmpSocketName := socketName("mpv-ctrl-", ".socket")

	args := createMPVCommand(deviceName, mf.AbsolutePath(), tmpSocketName)
	if len(args) == 0 {
		return nil, fmt.Errorf("no mpv command arguments provided")
	}
	exe, err := start(ctx, args)
	if err != nil {
		log.Error("Error starting mpv process", err)
		return nil, err
	}

	// wait for socket to show up
	err = waitForSocket(tmpSocketName, 3*time.Second, 100*time.Millisecond)
	if err != nil {
		log.Error("Error or timeout waiting for control socket", "socketname", tmpSocketName, err)
		return nil, err
	}

	conn := mpvipc.NewConnection(tmpSocketName)
	err = conn.Open()

	if err != nil {
		log.Error("Error opening new connection", err)
		return nil, err
	}

	theTrack := &MpvTrack{MediaFile: mf, PlaybackDone: playbackDoneChannel, Conn: conn, IPCSocketName: tmpSocketName, Exe: &exe, CloseCalled: false}

	go func() {
		conn.WaitUntilClosed()
		log.Info("Hitting end-of-stream, signalling on channel")
		if !theTrack.CloseCalled {
			playbackDoneChannel <- true
		}
	}()

	return theTrack, nil
}

func (t *MpvTrack) String() string {
	return fmt.Sprintf("Name: %s, Socket: %s", t.MediaFile.Path, t.IPCSocketName)
}

// Used to control the playback volume. A float value between 0.0 and 1.0.
//
// SetVolume 设置音量。入参为 0.0~1.0，mpv 的 volume 属性为 0~100，需要换算。
func (t *MpvTrack) SetVolume(value float32) {
	// mpv's volume as described in the --volume parameter:
	// Set the startup volume. 0 means silence, 100 means no volume reduction or amplification.
	//  Negative values can be passed for compatibility, but are treated as 0.
	log.Debug("Setting volume", "volume", value, "track", t)
	vol := int(value * 100)

	err := t.Conn.Set("volume", vol)
	if err != nil {
		log.Error("Error setting volume", "volume", value, "track", t, err)
	}
}

// Unpause 恢复播放。
func (t *MpvTrack) Unpause() {
	log.Debug("Unpausing track", "track", t)
	err := t.Conn.Set("pause", false)
	if err != nil {
		log.Error("Error unpausing track", "track", t, err)
	}
}

// Pause 暂停播放。
func (t *MpvTrack) Pause() {
	log.Debug("Pausing track", "track", t)
	err := t.Conn.Set("pause", true)
	if err != nil {
		log.Error("Error pausing track", "track", t, err)
	}
}

// Close 释放音轨资源。
//
// 先置 CloseCalled，防止连接断开时触发「播放完成」的误切歌。
// 优先通过 IPC 发 quit 让 mpv 优雅退出；失败则直接杀进程，
// 最后清理 socket 文件，避免残留。
func (t *MpvTrack) Close() {
	log.Debug("Closing resources", "track", t)
	t.CloseCalled = true
	// trying to shutdown mpv process using socket
	if t.isSocketFilePresent() {
		log.Debug("sending shutdown command")
		_, err := t.Conn.Call("quit")
		if err != nil {
			log.Warn("Error sending quit command to mpv-ipc socket", err)

			if t.Exe != nil {
				log.Debug("cancelling executor")
				err = t.Exe.Cancel()
				if err != nil {
					log.Warn("Error canceling executor", err)
				}
			}
		}
	}

	if t.isSocketFilePresent() {
		removeSocket(t.IPCSocketName)
	}
}

// isSocketFilePresent 判断 IPC socket 文件是否仍存在。
func (t *MpvTrack) isSocketFilePresent() bool {
	if len(t.IPCSocketName) < 1 {
		return false
	}

	fileInfo, err := os.Stat(t.IPCSocketName)
	return err == nil && fileInfo != nil && !fileInfo.IsDir()
}

// Position returns the playback position in seconds.
// Every now and then the mpv IPC interface returns "mpv error: property unavailable"
// in this case we have to retry
func (t *MpvTrack) Position() int {
	retryCount := 0
	for {
		position, err := t.Conn.Get("time-pos")
		if err != nil && err.Error() == "mpv error: property unavailable" {
			retryCount += 1
			log.Debug("Got mpv error, retrying...", "retries", retryCount, err)
			if retryCount > 5 {
				return 0
			}
			time.Sleep(time.Duration(retryCount) * time.Millisecond)
			continue
		}

		if err != nil {
			log.Error("Error getting position in track", "track", t, err)
			return 0
		}

		pos, ok := position.(float64)
		if !ok {
			log.Error("Could not cast position from mpv into float64", "position", position, "track", t)
			return 0
		} else {
			return int(pos)
		}
	}
}

// SetPosition 跳转到指定秒数。位置相同则跳过，避免无谓的 seek 造成卡顿。
func (t *MpvTrack) SetPosition(offset int) error {
	log.Debug("Setting position", "offset", offset, "track", t)
	pos := t.Position()
	if pos == offset {
		log.Debug("No position difference, skipping operation", "track", t)
		return nil
	}
	err := t.Conn.Set("time-pos", float64(offset))
	if err != nil {
		log.Error("Could not set the position in track", "track", t, "offset", offset, err)
		return err
	}
	return nil
}

// IsPlaying 查询是否处于播放中（即未暂停）。
func (t *MpvTrack) IsPlaying() bool {
	log.Debug("Checking if track is playing", "track", t)
	pausing, err := t.Conn.Get("pause")
	if err != nil {
		log.Error("Problem getting paused status", "track", t, err)
		return false
	}

	pause, ok := pausing.(bool)
	if !ok {
		log.Error("Could not cast pausing to boolean", "track", t, "value", pausing)
		return false
	}
	return !pause
}

// waitForSocket 轮询等待 socket 文件出现，超时则报错。
// mpv 进程启动到创建 socket 之间有延迟，需要等待而非立即连接。
func waitForSocket(path string, timeout time.Duration, pause time.Duration) error {
	start := time.Now()
	end := start.Add(timeout)
	var retries int = 0

	for {
		fileInfo, err := os.Stat(path)
		if err == nil && fileInfo != nil && !fileInfo.IsDir() {
			log.Debug("Socket found", "retries", retries, "waitTime", time.Since(start))
			return nil
		}
		if time.Now().After(end) {
			return fmt.Errorf("timeout reached: %s", timeout)
		}
		time.Sleep(pause)
		retries += 1
	}
}
