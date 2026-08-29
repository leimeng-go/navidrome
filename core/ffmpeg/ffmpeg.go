package ffmpeg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
)

// FFmpeg 封装对外部 ffmpeg 可执行文件的调用。
type FFmpeg interface {
	Transcode(ctx context.Context, command, path string, maxBitRate, offset int) (io.ReadCloser, error)
	ExtractImage(ctx context.Context, path string) (io.ReadCloser, error)
	Probe(ctx context.Context, files []string) (string, error)
	CmdPath() (string, error)
	IsAvailable() bool
	Version() string
}

// New 创建 ffmpeg 封装实例。
func New() FFmpeg {
	return &ffmpeg{}
}

const (
	// extractImageCmd 提取内嵌封面：只取视频流中的附加图片（-map -0:V 排除真实视频流），
	// 直接拷贝编码不做转换，以 image2pipe 输出到标准输出。
	extractImageCmd = "ffmpeg -i %s -map 0:v -map -0:V -vcodec copy -f image2pipe -"
	// probeCmd 以 ffmetadata 格式输出文件元数据
	probeCmd = "ffmpeg %s -f ffmetadata"
)

type ffmpeg struct{}

// Transcode 按给定命令模板转码音频，返回可读的输出流。
func (e *ffmpeg) Transcode(ctx context.Context, command, path string, maxBitRate, offset int) (io.ReadCloser, error) {
	if _, err := ffmpegCmd(); err != nil {
		return nil, err
	}
	// First make sure the file exists
	if err := fileExists(path); err != nil {
		return nil, err
	}
	args := createFFmpegCommand(command, path, maxBitRate, offset)
	return e.start(ctx, args)
}

// ExtractImage 提取音频文件内嵌的封面图。
func (e *ffmpeg) ExtractImage(ctx context.Context, path string) (io.ReadCloser, error) {
	if _, err := ffmpegCmd(); err != nil {
		return nil, err
	}
	// First make sure the file exists
	if err := fileExists(path); err != nil {
		return nil, err
	}
	args := createFFmpegCommand(extractImageCmd, path, 0, 0)
	return e.start(ctx, args)
}

// fileExists 校验路径存在且为普通文件，提前拦截以避免 ffmpeg 报晦涩错误。
func fileExists(path string) error {
	s, err := os.Stat(path)
	if err != nil {
		return err
	}
	if s.IsDir() {
		return fmt.Errorf("'%s' is a directory", path)
	}
	return nil
}

// Probe 批量读取文件元数据。
// 忽略退出码：ffmpeg 无输出文件时必然以非零码退出，
// 但元数据已打印到 stderr，故只取合并输出。
func (e *ffmpeg) Probe(ctx context.Context, files []string) (string, error) {
	if _, err := ffmpegCmd(); err != nil {
		return "", err
	}
	args := createProbeCommand(probeCmd, files)
	log.Trace(ctx, "Executing ffmpeg command", "args", args)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec
	output, _ := cmd.CombinedOutput()
	return string(output), nil
}

// CmdPath 返回 ffmpeg 可执行文件路径。
func (e *ffmpeg) CmdPath() (string, error) {
	return ffmpegCmd()
}

// IsAvailable 判断系统中是否可用 ffmpeg。
func (e *ffmpeg) IsAvailable() bool {
	_, err := ffmpegCmd()
	return err == nil
}

// Version executes ffmpeg -version and extracts the version from the output.
// Sample output: ffmpeg version 6.0 Copyright (c) 2000-2023 the FFmpeg developers
//
// Version 执行 ffmpeg -version 并取输出的第三个词作为版本号。
// 任何失败一律返回 "N/A"，此信息仅用于展示。
func (e *ffmpeg) Version() string {
	cmd, err := ffmpegCmd()
	if err != nil {
		return "N/A"
	}
	out, err := exec.Command(cmd, "-version").CombinedOutput() // #nosec
	if err != nil {
		return "N/A"
	}
	parts := strings.Split(string(out), " ")
	if len(parts) < 3 {
		return "N/A"
	}
	return parts[2]
}

// start 启动 ffmpeg 子进程，通过管道流式返回其标准输出，
// 调用方无需等待转码完成即可开始读取。
func (e *ffmpeg) start(ctx context.Context, args []string) (io.ReadCloser, error) {
	log.Trace(ctx, "Executing ffmpeg command", "cmd", args)
	j := &ffCmd{args: args}
	j.PipeReader, j.out = io.Pipe()
	err := j.start(ctx)
	if err != nil {
		return nil, err
	}
	go j.wait()
	return j, nil
}

// ffCmd 是一次 ffmpeg 调用，本身即为 io.ReadCloser（管道读端）。
type ffCmd struct {
	*io.PipeReader
	out  *io.PipeWriter
	args []string
	cmd  *exec.Cmd
}

// start 启动子进程。仅在 Trace 级别透传 stderr，
// 否则 ffmpeg 的大量进度输出会淹没日志。
func (j *ffCmd) start(ctx context.Context) error {
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

// wait 等待子进程退出，并把失败转化为管道错误，
// 使读取端能通过 Read 感知转码异常终止。
func (j *ffCmd) wait() {
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
// createFFmpegCommand 把命令模板展开为参数列表。
// 占位符：%s 输入路径、%t 起始偏移、%b 最大比特率。
// 模板未使用 %t 但请求了偏移时，自动在输入之后插入 -ss，
// 兼容用户自定义的不含偏移占位符的转码命令。
func createFFmpegCommand(cmd, path string, maxBitRate, offset int) []string {
	var args []string
	for _, s := range fixCmd(cmd) {
		if strings.Contains(s, "%s") {
			s = strings.ReplaceAll(s, "%s", path)
			args = append(args, s)
			if offset > 0 && !strings.Contains(cmd, "%t") {
				args = append(args, "-ss", strconv.Itoa(offset))
			}
		} else {
			s = strings.ReplaceAll(s, "%t", strconv.Itoa(offset))
			s = strings.ReplaceAll(s, "%b", strconv.Itoa(maxBitRate))
			args = append(args, s)
		}
	}
	return args
}

// createProbeCommand 展开探测命令，把 %s 替换为多个 -i 输入参数，
// 从而一次调用探测多个文件。
func createProbeCommand(cmd string, inputs []string) []string {
	var args []string
	for _, s := range fixCmd(cmd) {
		if s == "%s" {
			for _, inp := range inputs {
				args = append(args, "-i", inp)
			}
		} else {
			args = append(args, s)
		}
	}
	return args
}

// fixCmd 按空白切分命令，并把其中的 "ffmpeg" 替换为实际可执行文件路径，
// 使用户配置的自定义路径也能生效。
func fixCmd(cmd string) []string {
	split := strings.Fields(cmd)
	cmdPath, _ := ffmpegCmd()
	for i, s := range split {
		if s == "ffmpeg" || s == "ffmpeg.exe" {
			split[i] = cmdPath
		}
	}
	return split
}

// ffmpegCmd 查找 ffmpeg 可执行文件路径，结果只解析一次并缓存。
// 未配置路径时从 PATH 查找；Go 因安全考虑会拒绝当前目录下的可执行文件（ErrDot），
// 故显式以 "./ffmpeg" 再试一次，兼容便携式部署。
func ffmpegCmd() (string, error) {
	ffOnce.Do(func() {
		if conf.Server.FFmpegPath != "" {
			ffmpegPath = conf.Server.FFmpegPath
			ffmpegPath, ffmpegErr = exec.LookPath(ffmpegPath)
		} else {
			ffmpegPath, ffmpegErr = exec.LookPath("ffmpeg")
			if errors.Is(ffmpegErr, exec.ErrDot) {
				log.Trace("ffmpeg found in current folder '.'")
				ffmpegPath, ffmpegErr = exec.LookPath("./ffmpeg")
			}
		}
		if ffmpegErr == nil {
			log.Info("Found ffmpeg", "path", ffmpegPath)
			return
		}
	})
	return ffmpegPath, ffmpegErr
}

// These variables are accessible here for tests. Do not use them directly in production code. Use ffmpegCmd() instead.
var (
	ffOnce     sync.Once
	ffmpegPath string
	ffmpegErr  error
)
