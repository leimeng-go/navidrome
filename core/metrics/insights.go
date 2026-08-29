package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/google/uuid"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/core/metrics/insights"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/plugins/schema"
	"github.com/navidrome/navidrome/utils/singleton"
)

// Insights 是匿名使用统计上报服务，用于帮助项目了解各类配置的实际使用情况。
// 详见 https://navidrome.org/docs/getting-started/insights
type Insights interface {
	Run(ctx context.Context)
	LastRun(ctx context.Context) (timestamp time.Time, success bool)
}

var (
	insightsID string
)

// insightsCollector 采集并上报统计数据。
// lastRun/lastStatus 用原子变量，因为要被后台协程写、被 HTTP 请求读。
type insightsCollector struct {
	ds           model.DataStore
	pluginLoader PluginLoader
	lastRun      atomic.Int64
	lastStatus   atomic.Bool
}

// PluginLoader defines an interface for loading plugins
// PluginLoader 提供已安装插件清单。
type PluginLoader interface {
	PluginList() map[string]schema.PluginManifest
}

// GetInstance 返回统计服务单例。
// 首次运行时生成一个随机 ID 并持久化，用于在不暴露身份的前提下去重统计。
func GetInstance(ds model.DataStore, pluginLoader PluginLoader) Insights {
	return singleton.GetInstance(func() *insightsCollector {
		id, err := ds.Property(context.TODO()).Get(consts.InsightsIDKey)
		if err != nil {
			log.Trace("Could not get Insights ID from DB. Creating one", err)
			id = uuid.NewString()
			err = ds.Property(context.TODO()).Put(consts.InsightsIDKey, id)
			if err != nil {
				log.Trace("Could not save Insights ID to DB", err)
			}
		}
		insightsID = id
		return &insightsCollector{ds: ds, pluginLoader: pluginLoader}
	})
}

// Run 按固定间隔循环上报，直到上下文取消。
// 每轮都重新获取管理员上下文：首次启动时可能尚未创建管理员用户。
func (c *insightsCollector) Run(ctx context.Context) {
	for {
		// Refresh admin context on each iteration to handle cases where
		// admin user wasn't available on previous runs
		insightsCtx := auth.WithAdminUser(ctx, c.ds)
		u, _ := request.UserFrom(insightsCtx)
		if !u.IsAdmin {
			log.Trace(insightsCtx, "No admin user available, skipping insights collection")
		} else {
			c.sendInsights(insightsCtx)
		}
		select {
		case <-time.After(consts.InsightsUpdateInterval):
			continue
		case <-ctx.Done():
			return
		}
	}
}

// LastRun 返回最近一次上报的时间与是否成功。
func (c *insightsCollector) LastRun(context.Context) (timestamp time.Time, success bool) {
	t := c.lastRun.Load()
	return time.UnixMilli(t), c.lastStatus.Load()
}

// sendInsights 采集并 POST 统计数据。
// 无用户时跳过——尚未完成初始化的实例不具备统计意义。
// 上报的完整内容会写入日志，便于用户自行核查发送了什么。
func (c *insightsCollector) sendInsights(ctx context.Context) {
	count, err := c.ds.User(ctx).CountAll(model.QueryOptions{})
	if err != nil {
		log.Trace(ctx, "Could not check user count", err)
		return
	}
	if count == 0 {
		log.Trace(ctx, "No users found, skipping Insights data collection")
		return
	}
	hc := &http.Client{
		Timeout: consts.DefaultHttpClientTimeOut,
	}
	data := c.collect(ctx)
	if data == nil {
		return
	}
	body := bytes.NewReader(data)
	req, err := http.NewRequestWithContext(ctx, "POST", consts.InsightsEndpoint, body)
	if err != nil {
		log.Trace(ctx, "Could not create Insights request", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		log.Trace(ctx, "Could not send Insights data", err)
		return
	}
	log.Info(ctx, "Sent Insights data (for details see http://navidrome.org/docs/getting-started/insights", "data",
		string(data), "server", consts.InsightsEndpoint, "status", resp.Status)
	c.lastRun.Store(time.Now().UnixMilli())
	c.lastStatus.Store(resp.StatusCode < 300)
	resp.Body.Close()
}

// buildInfo 读取编译期嵌入的构建信息与 Go 版本。
func buildInfo() (map[string]string, string) {
	bInfo := map[string]string{}
	var version string
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Value == "" {
				continue
			}
			bInfo[setting.Key] = setting.Value
		}
		version = info.GoVersion
	}
	return bInfo, version
}

// getFSInfo 探测指定路径所在文件系统的类型。
// 仅上报类型（如 ext4/nfs），不上报路径本身，避免泄露隐私。
func getFSInfo(path string) *insights.FSInfo {
	var info insights.FSInfo

	// Normalize the path
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	absPath = filepath.Clean(absPath)

	fsType, err := getFilesystemType(absPath)
	if err != nil {
		return nil
	}
	info.Type = fsType
	return &info
}

// staticData 汇集运行期间不变的信息（版本、系统、文件系统、配置开关），只计算一次。
// 配置项只上报「是否启用」等布尔或数值，不上报密钥、路径等敏感内容。
var staticData = sync.OnceValue(func() insights.Data {
	// Basic info
	data := insights.Data{
		InsightsID: insightsID,
		Version:    consts.Version,
	}

	// Build info
	data.Build.Settings, data.Build.GoVersion = buildInfo()
	data.OS.Containerized = consts.InContainer

	// Install info
	packageFilename := filepath.Join(conf.Server.DataFolder, ".package")
	packageFileData, err := os.ReadFile(packageFilename)
	if err == nil {
		data.OS.Package = string(packageFileData)
	}

	// OS info
	data.OS.Type = runtime.GOOS
	data.OS.Arch = runtime.GOARCH
	data.OS.NumCPU = runtime.NumCPU()
	data.OS.Version, data.OS.Distro = getOSVersion()

	// FS info
	data.FS.Music = getFSInfo(conf.Server.MusicFolder)
	data.FS.Data = getFSInfo(conf.Server.DataFolder)
	if conf.Server.CacheFolder != "" {
		data.FS.Cache = getFSInfo(conf.Server.CacheFolder)
	}
	if conf.Server.Backup.Path != "" {
		data.FS.Backup = getFSInfo(conf.Server.Backup.Path)
	}

	// Config info
	data.Config.LogLevel = conf.Server.LogLevel
	data.Config.LogFileConfigured = conf.Server.LogFile != ""
	data.Config.TLSConfigured = conf.Server.TLSCert != "" && conf.Server.TLSKey != ""
	data.Config.DefaultBackgroundURLSet = conf.Server.UILoginBackgroundURL == consts.DefaultUILoginBackgroundURL
	data.Config.EnableArtworkPrecache = conf.Server.EnableArtworkPrecache
	data.Config.EnableCoverAnimation = conf.Server.EnableCoverAnimation
	data.Config.EnableNowPlaying = conf.Server.EnableNowPlaying
	data.Config.EnableDownloads = conf.Server.EnableDownloads
	data.Config.EnableSharing = conf.Server.EnableSharing
	data.Config.EnableStarRating = conf.Server.EnableStarRating
	data.Config.EnableLastFM = conf.Server.LastFM.Enabled && conf.Server.LastFM.ApiKey != "" && conf.Server.LastFM.Secret != ""
	data.Config.EnableSpotify = conf.Server.Spotify.ID != "" && conf.Server.Spotify.Secret != ""
	data.Config.EnableListenBrainz = conf.Server.ListenBrainz.Enabled
	data.Config.EnableDeezer = conf.Server.Deezer.Enabled
	data.Config.EnableMediaFileCoverArt = conf.Server.EnableMediaFileCoverArt
	data.Config.EnableJukebox = conf.Server.Jukebox.Enabled
	data.Config.EnablePrometheus = conf.Server.Prometheus.Enabled
	data.Config.TranscodingCacheSize = conf.Server.TranscodingCacheSize
	data.Config.ImageCacheSize = conf.Server.ImageCacheSize
	data.Config.SessionTimeout = uint64(math.Trunc(conf.Server.SessionTimeout.Seconds()))
	data.Config.SearchFullString = conf.Server.SearchFullString
	data.Config.RecentlyAddedByModTime = conf.Server.RecentlyAddedByModTime
	data.Config.PreferSortTags = conf.Server.PreferSortTags
	data.Config.BackupSchedule = conf.Server.Backup.Schedule
	data.Config.BackupCount = conf.Server.Backup.Count
	data.Config.DevActivityPanel = conf.Server.DevActivityPanel
	data.Config.ScannerEnabled = conf.Server.Scanner.Enabled
	data.Config.ScanSchedule = conf.Server.Scanner.Schedule
	data.Config.ScanWatcherWait = uint64(math.Trunc(conf.Server.Scanner.WatcherWait.Seconds()))
	data.Config.ScanOnStartup = conf.Server.Scanner.ScanOnStartup
	data.Config.ReverseProxyConfigured = conf.Server.ExtAuth.TrustedSources != ""
	data.Config.HasCustomPID = conf.Server.PID.Track != "" || conf.Server.PID.Album != ""
	data.Config.HasCustomTags = len(conf.Server.Tags) > 0

	return data
})

// collect 在静态信息基础上补充动态数据（媒体库规模、内存、运行时长），序列化为 JSON。
// 各项统计失败只记 Trace 日志继续，缺一两项不应导致整次上报失败。
// 播放器与插件信息受独立开关控制，默认不采集。
func (c *insightsCollector) collect(ctx context.Context) []byte {
	data := staticData()
	data.Uptime = time.Since(consts.ServerStart).Milliseconds() / 1000

	// Library info
	var err error
	data.Library.Tracks, err = c.ds.MediaFile(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading tracks count", err)
	}
	data.Library.Albums, err = c.ds.Album(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading albums count", err)
	}
	data.Library.Artists, err = c.ds.Artist(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading artists count", err)
	}
	data.Library.Playlists, err = c.ds.Playlist(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading playlists count", err)
	}
	data.Library.Shares, err = c.ds.Share(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading shares count", err)
	}
	data.Library.Radios, err = c.ds.Radio(ctx).Count()
	if err != nil {
		log.Trace(ctx, "Error reading radios count", err)
	}
	data.Library.Libraries, err = c.ds.Library(ctx).CountAll()
	if err != nil {
		log.Trace(ctx, "Error reading libraries count", err)
	}
	data.Library.ActiveUsers, err = c.ds.User(ctx).CountAll(model.QueryOptions{
		Filters: squirrel.Gt{"last_access_at": time.Now().Add(-7 * 24 * time.Hour)},
	})
	if err != nil {
		log.Trace(ctx, "Error reading active users count", err)
	}

	// Check for smart playlists
	data.Config.HasSmartPlaylists, err = c.hasSmartPlaylists(ctx)
	if err != nil {
		log.Trace(ctx, "Error checking for smart playlists", err)
	}

	// Collect plugins if permitted and enabled
	if conf.Server.DevEnablePluginsInsights && conf.Server.Plugins.Enabled {
		data.Plugins = c.collectPlugins(ctx)
	}

	// Collect active players if permitted
	if conf.Server.DevEnablePlayerInsights {
		data.Library.ActivePlayers, err = c.ds.Player(ctx).CountByClient(model.QueryOptions{
			Filters: squirrel.Gt{"last_seen": time.Now().Add(-7 * 24 * time.Hour)},
		})
		if err != nil {
			log.Trace(ctx, "Error reading active players count", err)
		}
	}

	// Memory info
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	data.Mem.Alloc = m.Alloc
	data.Mem.TotalAlloc = m.TotalAlloc
	data.Mem.Sys = m.Sys
	data.Mem.NumGC = m.NumGC

	// Marshal to JSON
	resp, err := json.Marshal(data)
	if err != nil {
		log.Trace(ctx, "Could not marshal Insights data", err)
		return nil
	}
	return resp
}

// hasSmartPlaylists checks if there are any smart playlists (playlists with rules)
// hasSmartPlaylists 判断是否存在智能歌单（带筛选规则的歌单）。
func (c *insightsCollector) hasSmartPlaylists(ctx context.Context) (bool, error) {
	count, err := c.ds.Playlist(ctx).CountAll(model.QueryOptions{
		Filters: squirrel.And{squirrel.NotEq{"rules": ""}, squirrel.NotEq{"rules": nil}},
	})
	return count > 0, err
}

// collectPlugins collects information about installed plugins
// collectPlugins 收集已安装插件的名称与版本。
func (c *insightsCollector) collectPlugins(_ context.Context) map[string]insights.PluginInfo {
	plugins := make(map[string]insights.PluginInfo)
	for id, manifest := range c.pluginLoader.PluginList() {
		plugins[id] = insights.PluginInfo{
			Name:    manifest.Name,
			Version: manifest.Version,
		}
	}
	return plugins
}
