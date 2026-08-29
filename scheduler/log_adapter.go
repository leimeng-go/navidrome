package scheduler

import (
	"github.com/navidrome/navidrome/log"
)

// logger 把 cron 的日志接入 Navidrome 日志系统。
// cron 的 Info 级日志过于琐碎，降为 Debug 输出。
type logger struct{}

func (l *logger) Info(msg string, keysAndValues ...interface{}) {
	args := []interface{}{
		"Scheduler: " + msg,
	}
	args = append(args, keysAndValues...)
	log.Debug(args...)
}

func (l *logger) Error(err error, msg string, keysAndValues ...interface{}) {
	args := []interface{}{
		"Scheduler: " + msg,
	}
	args = append(args, keysAndValues...)
	args = append(args, err)
	log.Error(args...)
}
