package scanner

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core"
	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/events"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/pl"
	"golang.org/x/time/rate"
)

var (
	// ErrAlreadyScanning 表示已有扫描在进行中，本次请求被拒绝。
	ErrAlreadyScanning = errors.New("already scanning")
)

// New 创建扫描控制器，它是所有扫描操作的统一入口。
//
// 只有进程内扫描才配置限流器：外部扫描进程的进度上报本身已是低频的，
// 无需再节流。限流用于抑制进度事件的推送频率，
// 否则大库扫描会向客户端刷出海量消息。
func New(rootCtx context.Context, ds model.DataStore, cw artwork.CacheWarmer, broker events.Broker,
	pls core.Playlists, m metrics.Metrics) model.Scanner {
	c := &controller{
		rootCtx: rootCtx,
		ds:      ds,
		cw:      cw,
		broker:  broker,
		pls:     pls,
		metrics: m,
	}
	if !conf.Server.DevExternalScanner {
		c.limiter = P(rate.Sometimes{Interval: conf.Server.DevActivityPanelUpdateRate})
	}
	return c
}

// getScanner 依配置选择扫描实现：
// 外部子进程（默认，可隔离扫描期间的内存占用）或进程内实现。
func (s *controller) getScanner() scanner {
	if conf.Server.DevExternalScanner {
		return &scannerExternal{}
	}
	return &scannerImpl{ds: s.ds, cw: s.cw, pls: s.pls}
}

// CallScan starts an in-process scan of specific library/folder pairs.
// If targets is empty, it scans all libraries.
// This is meant to be called from the command line (see cmd/scan.go).
//
// CallScan 在当前进程内发起扫描，供命令行调用。
// 命令行场景无需预热封面缓存（进程随即退出），故传入空实现。
func CallScan(ctx context.Context, ds model.DataStore, pls core.Playlists, fullScan bool, targets []model.ScanTarget) (<-chan *ProgressInfo, error) {
	release, err := lockScan(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	ctx = auth.WithAdminUser(ctx, ds)
	progress := make(chan *ProgressInfo, 100)
	go func() {
		defer close(progress)
		scanner := &scannerImpl{ds: ds, cw: artwork.NoopCacheWarmer(), pls: pls}
		scanner.scanFolders(ctx, fullScan, targets, progress)
	}()
	return progress, nil
}

// IsScanning 返回当前是否有扫描在进行。
func IsScanning() bool {
	return running.Load()
}

// ProgressInfo 是扫描过程中上报的一条进度消息。
// 同一结构承载多种含义：普通进度、警告、错误、变更标记，
// 由接收方按字段判别（见 controller.trackProgress）。
type ProgressInfo struct {
	LibID           int
	FileCount       uint32
	Path            string
	Phase           string
	ChangesDetected bool
	Warning         string
	Error           string
	ForceUpdate     bool // 绕过限流，强制推送本条进度
}

// scanner defines the interface for different scanner implementations.
// This allows for swapping between in-process and external scanners.
// scanner 抽象扫描实现，使进程内与外部子进程两种方式可互换。
type scanner interface {
	// scanFolders performs the actual scanning of folders. If targets is nil, it scans all libraries.
	scanFolders(ctx context.Context, fullScan bool, targets []model.ScanTarget, progress chan<- *ProgressInfo)
}

// controller 是扫描的对外门面，负责串行化扫描请求、
// 汇总进度并向客户端广播事件。
type controller struct {
	rootCtx         context.Context
	ds              model.DataStore
	cw              artwork.CacheWarmer
	broker          events.Broker
	metrics         metrics.Metrics
	pls             core.Playlists
	limiter         *rate.Sometimes
	count           atomic.Uint32
	folderCount     atomic.Uint32
	changesDetected bool
}

// getLastScanTime returns the most recent scan time across all libraries
// getLastScanTime 返回所有音乐库中最近的一次扫描完成时间。
func (s *controller) getLastScanTime(ctx context.Context) (time.Time, error) {
	libs, err := s.ds.Library(ctx).GetAll(model.QueryOptions{
		Sort:  "last_scan_at",
		Order: "desc",
		Max:   1,
	})
	if err != nil {
		return time.Time{}, fmt.Errorf("getting libraries: %w", err)
	}

	if len(libs) == 0 {
		return time.Time{}, nil
	}

	return libs[0].LastScanAt, nil
}

// getScanInfo retrieves scan status from the database
// getScanInfo 从数据库读取扫描类型、耗时与上次错误。
// 扫描进行中时耗时为「至今」；已结束时用最近完成时间减去开始时间，
// 得到上一次扫描的实际耗时。
func (s *controller) getScanInfo(ctx context.Context) (scanType string, elapsed time.Duration, lastErr string) {
	lastErr, _ = s.ds.Property(ctx).DefaultGet(consts.LastScanErrorKey, "")
	scanType, _ = s.ds.Property(ctx).DefaultGet(consts.LastScanTypeKey, "")
	startTimeStr, _ := s.ds.Property(ctx).DefaultGet(consts.LastScanStartTimeKey, "")

	if startTimeStr != "" {
		startTime, err := time.Parse(time.RFC3339, startTimeStr)
		if err == nil {
			if running.Load() {
				elapsed = time.Since(startTime)
			} else {
				// If scan is not running, calculate elapsed time using the most recent scan time
				lastScanTime, err := s.getLastScanTime(ctx)
				if err == nil && !lastScanTime.IsZero() {
					elapsed = lastScanTime.Sub(startTime)
				}
			}
		}
	}

	return scanType, elapsed, lastErr
}

// Status 返回扫描状态。
// 扫描进行中时计数取自内存中的实时累加值；
// 空闲时改用各库已持久化的统计，避免展示上一次扫描的残留数字。
func (s *controller) Status(ctx context.Context) (*model.ScannerStatus, error) {
	lastScanTime, err := s.getLastScanTime(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting last scan time: %w", err)
	}

	scanType, elapsed, lastErr := s.getScanInfo(ctx)

	if running.Load() {
		status := &model.ScannerStatus{
			Scanning:    true,
			LastScan:    lastScanTime,
			Count:       s.count.Load(),
			FolderCount: s.folderCount.Load(),
			LastError:   lastErr,
			ScanType:    scanType,
			ElapsedTime: elapsed,
		}
		return status, nil
	}

	count, folderCount, err := s.getCounters(ctx)
	if err != nil {
		return nil, fmt.Errorf("getting library stats: %w", err)
	}
	return &model.ScannerStatus{
		Scanning:    false,
		LastScan:    lastScanTime,
		Count:       uint32(count),
		FolderCount: uint32(folderCount),
		LastError:   lastErr,
		ScanType:    scanType,
		ElapsedTime: elapsed,
	}, nil
}

// getCounters 汇总各库已持久化的曲目数与文件夹数。
func (s *controller) getCounters(ctx context.Context) (int64, int64, error) {
	libs, err := s.ds.Library(ctx).GetAll()
	if err != nil {
		return 0, 0, fmt.Errorf("library count: %w", err)
	}
	var count, folderCount int64
	for _, l := range libs {
		count += int64(l.TotalSongs)
		folderCount += int64(l.TotalFolders)
	}
	return count, folderCount, nil
}

// ScanAll 扫描全部音乐库。
func (s *controller) ScanAll(requestCtx context.Context, fullScan bool) ([]string, error) {
	return s.ScanFolders(requestCtx, fullScan, nil)
}

// ScanFolders 扫描指定的「库 + 目录」，targets 为 nil 时扫描全部。
//
// 上下文由 rootCtx 与请求上下文合并而来：
// 扫描的生命周期应跟随服务而非单次 HTTP 请求（请求可能提前结束），
// 但又需保留请求中的追踪信息。
// 扫描本身在独立 goroutine 中运行，主流程转为消费进度并广播事件，
// 直到进度通道关闭。
func (s *controller) ScanFolders(requestCtx context.Context, fullScan bool, targets []model.ScanTarget) ([]string, error) {
	release, err := lockScan(requestCtx)
	if err != nil {
		return nil, err
	}
	defer release()

	// Prepare the context for the scan
	ctx := request.AddValues(s.rootCtx, requestCtx)
	ctx = auth.WithAdminUser(ctx, s.ds)

	// Send the initial scan status event
	s.sendMessage(ctx, &events.ScanStatus{Scanning: true, Count: 0, FolderCount: 0})
	progress := make(chan *ProgressInfo, 100)
	go func() {
		defer close(progress)
		scanner := s.getScanner()
		scanner.scanFolders(ctx, fullScan, targets, progress)
	}()

	// Wait for the scan to finish, sending progress events to all connected clients
	scanWarnings, scanError := s.trackProgress(ctx, progress)
	for _, w := range scanWarnings {
		log.Warn(ctx, fmt.Sprintf("Scan warning: %s", w))
	}
	// If changes were detected, send a refresh event to all clients
	if s.changesDetected {
		log.Debug(ctx, "Library changes imported. Sending refresh event")
		s.broker.SendBroadcastMessage(ctx, &events.RefreshResource{})
	}
	// Send the final scan status event, with totals
	if count, folderCount, err := s.getCounters(ctx); err != nil {
		s.metrics.WriteAfterScanMetrics(ctx, false)
		return scanWarnings, err
	} else {
		scanType, elapsed, lastErr := s.getScanInfo(ctx)
		s.metrics.WriteAfterScanMetrics(ctx, true)
		s.sendMessage(ctx, &events.ScanStatus{
			Scanning:    false,
			Count:       count,
			FolderCount: folderCount,
			Error:       lastErr,
			ScanType:    scanType,
			ElapsedTime: elapsed,
		})
	}
	return scanWarnings, scanError
}

// This is a global variable that is used to prevent multiple scans from running at the same time.
// "There can be only one" - https://youtu.be/sqcLjcSloXs?si=VlsjEOjTJZ68zIyg
// 全局互斥标志：同一时刻只允许一次扫描。
// 用包级变量而非控制器字段，因为命令行入口 CallScan 不经过控制器，
// 但同样需要参与互斥。
var running atomic.Bool

// lockScan 尝试获取扫描权，成功则返回释放函数。
// 已有扫描在进行时返回 ErrAlreadyScanning。
func lockScan(ctx context.Context) (func(), error) {
	if !running.CompareAndSwap(false, true) {
		log.Debug(ctx, "Scanner already running, ignoring request")
		return func() {}, ErrAlreadyScanning
	}
	return func() {
		running.Store(false)
	}, nil
}

// trackProgress 消费进度通道直至关闭，累计计数并向客户端广播状态。
//
// 警告与错误只收集不中断：单个目录出问题不应终止整轮扫描，
// 最终把所有错误合并返回。
// 文件夹计数只在该条进度含文件时递增，跳过空目录。
// 普通进度经限流器节流，ForceUpdate 的消息则立即推送。
func (s *controller) trackProgress(ctx context.Context, progress <-chan *ProgressInfo) ([]string, error) {
	s.count.Store(0)
	s.folderCount.Store(0)
	s.changesDetected = false

	var warnings []string
	var errs []error
	for p := range pl.ReadOrDone(ctx, progress) {
		if p.Error != "" {
			errs = append(errs, errors.New(p.Error))
			continue
		}
		if p.Warning != "" {
			warnings = append(warnings, p.Warning)
			continue
		}
		if p.ChangesDetected {
			s.changesDetected = true
			continue
		}
		s.count.Add(p.FileCount)
		if p.FileCount > 0 {
			s.folderCount.Add(1)
		}

		scanType, elapsed, lastErr := s.getScanInfo(ctx)
		status := &events.ScanStatus{
			Scanning:    true,
			Count:       int64(s.count.Load()),
			FolderCount: int64(s.folderCount.Load()),
			Error:       lastErr,
			ScanType:    scanType,
			ElapsedTime: elapsed,
		}
		if s.limiter != nil && !p.ForceUpdate {
			s.limiter.Do(func() { s.sendMessage(ctx, status) })
		} else {
			s.sendMessage(ctx, status)
		}
	}
	return warnings, errors.Join(errs...)
}

func (s *controller) sendMessage(ctx context.Context, status *events.ScanStatus) {
	s.broker.SendBroadcastMessage(ctx, status)
}
