//go:build !windows && !plan9

package cmd

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/navidrome/navidrome/log"
)

// triggerScanSignal 是触发手动扫描的信号，便于外部脚本在拷贝完音乐后通知服务。
const triggerScanSignal = syscall.SIGUSR1

// startSignaller 监听扫描信号并触发扫描，直到 context 取消。
func startSignaller(ctx context.Context) func() error {
	log.Info(ctx, "Starting signaler")
	scanner := CreateScanner(ctx)

	return func() error {
		var sigChan = make(chan os.Signal, 1)
		signal.Notify(sigChan, triggerScanSignal)

		for {
			select {
			case sig := <-sigChan:
				log.Info(ctx, "Received signal, triggering a new scan", "signal", sig)
				start := time.Now()
				_, err := scanner.ScanAll(ctx, false)
				if err != nil {
					log.Error(ctx, "Error scanning", err)
				}
				log.Info(ctx, "Triggered scan complete", "elapsed", time.Since(start))
			case <-ctx.Done():
				return nil
			}
		}
	}
}
