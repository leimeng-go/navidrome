package scheduler

import (
	"context"

	"github.com/navidrome/navidrome/utils/singleton"
	"github.com/robfig/cron/v3"
)

// Scheduler 封装 cron，用于注册周期任务（扫描、备份、数据库优化、插件定时任务等）。
type Scheduler interface {
	Run(ctx context.Context)
	Add(crontab string, cmd func()) (int, error)
	Remove(id int)
}

// GetInstance 返回全局调度器单例，全部周期任务共用一个 cron 实例。
func GetInstance() Scheduler {
	return singleton.GetInstance(func() *scheduler {
		c := cron.New(cron.WithLogger(&logger{}))
		return &scheduler{
			c: c,
		}
	})
}

type scheduler struct {
	c *cron.Cron
}

// Run 启动调度并阻塞至 context 取消，取消后停止接受新任务。
func (s *scheduler) Run(ctx context.Context) {
	s.c.Start()
	<-ctx.Done()
	s.c.Stop()
}

// Add 注册 cron 任务，返回可用于撤销的条目 ID。
func (s *scheduler) Add(crontab string, cmd func()) (int, error) {
	entryID, err := s.c.AddFunc(crontab, cmd)
	if err != nil {
		return 0, err
	}
	return int(entryID), nil
}

// Remove 撤销任务。
func (s *scheduler) Remove(id int) {
	s.c.Remove(cron.EntryID(id))
}
