package mpv

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/kballard/go-shellquote"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
)

// start 启动 mpv 子进程，输出经管道返回。
func start(ctx context.Context, args []string) (Executor, error) {
	if len(args) == 0 {
		return Executor{}, fmt.Errorf("no command arguments provided")
	}
	log.Debug("Executing mpv command", "cmd", args)
	j := Executor{args: args}
	j.PipeReader, j.out = io.Pipe()
	err := j.start(ctx)
	if err != nil {
		return Executor{}, err
	}
	go j.wait()
	return j, nil
}

// Cancel 终止 mpv 子进程。
func (j *Executor) Cancel() error {
	if j.cmd != nil {
		return j.cmd.Cancel()
	}
	return fmt.Errorf("there is non command to cancel")
}

// Executor 封装一个 mpv 子进程，其标准输出通过内嵌的管道读端暴露。
type Executor struct {
	*io.PipeReader
	out  *io.PipeWriter
	args []string
	cmd  *exec.Cmd
}

// start 启动子进程。
// 仅在 Trace 级别才透传 stderr，避免 mpv 的冗余输出污染正常日志。
func (j *Executor) start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, j.args[0], j.args[1:]...) // #nosec
	cmd.Stdout = j.out
	if log.IsGreaterOrEqualTo(log.LevelTrace) {
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stderr = io.Discard
	}
	j.cmd = cmd

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting cmd: %w", err)
	}
	return nil
}

// wait 等待子进程退出，并把退出原因通过管道传给读端，
// 使读方能感知失败而非只看到 EOF。
func (j *Executor) wait() {
	if err := j.cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			_ = j.out.CloseWithError(fmt.Errorf("%s exited with non-zero status code: %d", j.args[0], exitErr.ExitCode()))
		} else {
			_ = j.out.CloseWithError(fmt.Errorf("waiting %s cmd: %w", j.args[0], err))
		}
		return
	}
	_ = j.out.Close()
}

// Path will always be an absolute path
//
// createMPVCommand 依据 MPVCmdTemplate 模板生成命令行参数。
// 用 shellquote 先按 shell 规则切分，再逐个参数替换占位符
// （%d 设备名、%f 文件路径、%s IPC socket），
// 这样替换进去的值即便含空格也不会被再次切分。
func createMPVCommand(deviceName string, filename string, socketName string) []string {
	// Parse the template structure using shell parsing to handle quoted arguments
	templateArgs, err := shellquote.Split(conf.Server.MPVCmdTemplate)
	if err != nil {
		log.Error("Failed to parse MPV command template", "template", conf.Server.MPVCmdTemplate, err)
		return nil
	}

	// Replace placeholders in each parsed argument to preserve spaces in substituted values
	for i, arg := range templateArgs {
		arg = strings.ReplaceAll(arg, "%d", deviceName)
		arg = strings.ReplaceAll(arg, "%f", filename)
		arg = strings.ReplaceAll(arg, "%s", socketName)
		templateArgs[i] = arg
	}

	// Replace mpv executable references with the configured path
	if len(templateArgs) > 0 {
		cmdPath, err := mpvCommand()
		if err == nil {
			if templateArgs[0] == "mpv" || templateArgs[0] == "mpv.exe" {
				templateArgs[0] = cmdPath
			}
		}
	}

	return templateArgs
}

// This is a 1:1 copy of the stuff in ffmpeg.go, need to be unified.
//
// mpvCommand 解析 mpv 可执行文件路径，结果只计算一次并缓存。
// ErrDot 表示只在当前目录找到，需显式写成 "./mpv" 才能执行。
func mpvCommand() (string, error) {
	mpvOnce.Do(func() {
		if conf.Server.MPVPath != "" {
			mpvPath = conf.Server.MPVPath
			mpvPath, mpvErr = exec.LookPath(mpvPath)
		} else {
			mpvPath, mpvErr = exec.LookPath("mpv")
			if errors.Is(mpvErr, exec.ErrDot) {
				log.Trace("mpv found in current folder '.'")
				mpvPath, mpvErr = exec.LookPath("./mpv")
			}
		}
		if mpvErr == nil {
			log.Info("Found mpv", "path", mpvPath)
			return
		}
	})
	return mpvPath, mpvErr
}

var (
	mpvOnce sync.Once
	mpvPath string
	mpvErr  error
)
