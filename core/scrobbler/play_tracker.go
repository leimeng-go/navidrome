package scrobbler

import (
	"context"
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/utils/cache"
	"github.com/navidrome/navidrome/utils/singleton"
)

// NowPlayingInfo 是一条「正在播放」记录，按播放器 ID 维护。
type NowPlayingInfo struct {
	MediaFile  model.MediaFile
	Start      time.Time
	Position   int
	Username   string
	PlayerId   string
	PlayerName string
}

// Submission 是一条播放上报，Timestamp 为客户端记录的播放时刻。
type Submission struct {
	TrackID   string
	Timestamp time.Time
}

// nowPlayingEntry 是待推送给外部 scrobbler 的队列项。
type nowPlayingEntry struct {
	ctx      context.Context
	userId   string
	track    *model.MediaFile
	position int
}

// PlayTracker 跟踪播放状态：维护「正在播放」列表、累加播放次数，
// 并把播放信息转发给各外部 scrobbler（Last.fm、ListenBrainz 等）。
type PlayTracker interface {
	NowPlaying(ctx context.Context, playerId string, playerName string, trackId string, position int) error
	GetNowPlaying(ctx context.Context) ([]NowPlayingInfo, error)
	Submit(ctx context.Context, submissions []Submission) error
}

// PluginLoader is a minimal interface for plugin manager usage in PlayTracker
// (avoids import cycles)
//
// PluginLoader 是插件管理器的最小接口，此处重新声明以避免循环导入。
type PluginLoader interface {
	PluginNames(capability string) []string
	LoadScrobbler(name string) (Scrobbler, bool)
}

// playTracker 是 PlayTracker 的实现。
//
// 「正在播放」用带 TTL 的缓存维护，超时自动淘汰，无需显式的「停止播放」通知。
// 对外推送走队列 + 后台协程：外部服务可能很慢，不能阻塞播放请求。
type playTracker struct {
	ds                model.DataStore
	broker            events.Broker
	playMap           cache.SimpleCache[string, NowPlayingInfo]
	builtinScrobblers map[string]Scrobbler
	pluginScrobblers  map[string]Scrobbler
	pluginLoader      PluginLoader
	mu                sync.RWMutex
	npQueue           map[string]nowPlayingEntry
	npMu              sync.Mutex
	npSignal          chan struct{}
	shutdown          chan struct{}
	workerDone        chan struct{}
}

// GetPlayTracker 返回 PlayTracker 单例。
func GetPlayTracker(ds model.DataStore, broker events.Broker, pluginManager PluginLoader) PlayTracker {
	return singleton.GetInstance(func() *playTracker {
		return newPlayTracker(ds, broker, pluginManager)
	})
}

// This constructor only exists for testing. For normal usage, the PlayTracker has to be a singleton, returned by
// the GetPlayTracker function above
//
// newPlayTracker 构造实例并启动后台推送协程。
// 内置 scrobbler 一律套一层缓冲装饰器，使外部服务不可用时能重试而不丢失记录。
func newPlayTracker(ds model.DataStore, broker events.Broker, pluginManager PluginLoader) *playTracker {
	m := cache.NewSimpleCache[string, NowPlayingInfo]()
	p := &playTracker{
		ds:                ds,
		playMap:           m,
		broker:            broker,
		builtinScrobblers: make(map[string]Scrobbler),
		pluginScrobblers:  make(map[string]Scrobbler),
		pluginLoader:      pluginManager,
		npQueue:           make(map[string]nowPlayingEntry),
		npSignal:          make(chan struct{}, 1),
		shutdown:          make(chan struct{}),
		workerDone:        make(chan struct{}),
	}
	if conf.Server.EnableNowPlaying {
		m.OnExpiration(func(_ string, _ NowPlayingInfo) {
			broker.SendBroadcastMessage(context.Background(), &events.NowPlayingCount{Count: m.Len()})
		})
	}

	var enabled []string
	for name, constructor := range constructors {
		s := constructor(ds)
		if s == nil {
			log.Debug("Scrobbler not available. Missing configuration?", "name", name)
			continue
		}
		enabled = append(enabled, name)
		s = newBufferedScrobbler(ds, s, name)
		p.builtinScrobblers[name] = s
	}
	log.Debug("List of builtin scrobblers enabled", "names", enabled)
	go p.nowPlayingWorker()
	return p
}

// stopNowPlayingWorker stops the background worker. This is primarily for testing.
func (p *playTracker) stopNowPlayingWorker() {
	close(p.shutdown)
	<-p.workerDone // Wait for worker to finish
}

// pluginNamesMatchScrobblers returns true if the set of pluginNames matches the keys in pluginScrobblers
// pluginNamesMatchScrobblers 判断插件集合是否与已加载的 scrobbler 完全一致，用于跳过无变化的刷新。
func pluginNamesMatchScrobblers(pluginNames []string, scrobblers map[string]Scrobbler) bool {
	if len(pluginNames) != len(scrobblers) {
		return false
	}
	for _, name := range pluginNames {
		if _, ok := scrobblers[name]; !ok {
			return false
		}
	}
	return true
}

// refreshPluginScrobblers updates the pluginScrobblers map to match the current set of plugin scrobblers
//
// refreshPluginScrobblers 同步插件 scrobbler 列表，支持插件热插拔。
// 已存在的实例保持不动，以免丢失其缓冲区中尚未提交的记录；
// 移除时若实例支持 Stop 则先优雅停止。
func (p *playTracker) refreshPluginScrobblers() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pluginLoader == nil {
		return
	}

	// Get the list of available plugin names
	pluginNames := p.pluginLoader.PluginNames("Scrobbler")

	// Early return if plugin names match existing scrobblers (no change)
	if pluginNamesMatchScrobblers(pluginNames, p.pluginScrobblers) {
		return
	}

	// Build a set of current plugins for faster lookups
	current := make(map[string]struct{}, len(pluginNames))

	// Process additions - add new plugins
	for _, name := range pluginNames {
		current[name] = struct{}{}
		// Only create a new scrobbler if it doesn't exist
		if _, exists := p.pluginScrobblers[name]; !exists {
			s, ok := p.pluginLoader.LoadScrobbler(name)
			if ok && s != nil {
				p.pluginScrobblers[name] = newBufferedScrobbler(p.ds, s, name)
			}
		}
	}

	type stoppableScrobbler interface {
		Scrobbler
		Stop()
	}

	// Process removals - remove plugins that no longer exist
	for name, scrobbler := range p.pluginScrobblers {
		if _, exists := current[name]; !exists {
			// If the scrobbler implements stoppableScrobbler, call Stop() before removing it
			if stoppable, ok := scrobbler.(stoppableScrobbler); ok {
				log.Debug("Stopping scrobbler", "name", name)
				stoppable.Stop()
			}
			delete(p.pluginScrobblers, name)
		}
	}
}

// getActiveScrobblers refreshes plugin scrobblers, acquires a read lock,
// combines builtin and plugin scrobblers into a new map, releases the lock,
// and returns the combined map.
//
// getActiveScrobblers 返回内置与插件 scrobbler 的合集快照。
// 返回副本使调用方可在锁外从容遍历（推送耗时较长）。
func (p *playTracker) getActiveScrobblers() map[string]Scrobbler {
	p.refreshPluginScrobblers()
	p.mu.RLock()
	defer p.mu.RUnlock()
	combined := maps.Clone(p.builtinScrobblers)
	maps.Copy(combined, p.pluginScrobblers)
	return combined
}

// NowPlaying 记录某播放器当前播放的曲目。
//
// TTL 取剩余时长再加 5 秒缓冲：客户端停播后无需显式通知，记录会自动过期；
// 缓冲量确保曲目播完前记录不会提前消失。
// 播放位置若超出时长（数据异常）则按 0 处理，避免负数 TTL。
func (p *playTracker) NowPlaying(ctx context.Context, playerId string, playerName string, trackId string, position int) error {
	mf, err := p.ds.MediaFile(ctx).GetWithParticipants(trackId)
	if err != nil {
		log.Error(ctx, "Error retrieving mediaFile", "id", trackId, err)
		return err
	}

	user, _ := request.UserFrom(ctx)
	info := NowPlayingInfo{
		MediaFile:  *mf,
		Start:      time.Now(),
		Position:   position,
		Username:   user.UserName,
		PlayerId:   playerId,
		PlayerName: playerName,
	}

	// Calculate TTL based on remaining track duration. If position exceeds track duration,
	// remaining is set to 0 to avoid negative TTL.
	remaining := int(mf.Duration) - position
	if remaining < 0 {
		remaining = 0
	}
	// Add 5 seconds buffer to ensure the NowPlaying info is available slightly longer than the track duration.
	ttl := time.Duration(remaining+5) * time.Second
	_ = p.playMap.AddWithTTL(playerId, info, ttl)
	if conf.Server.EnableNowPlaying {
		p.broker.SendBroadcastMessage(ctx, &events.NowPlayingCount{Count: p.playMap.Len()})
	}
	player, _ := request.PlayerFrom(ctx)
	if player.ScrobbleEnabled {
		p.enqueueNowPlaying(ctx, playerId, user.ID, mf, position)
	}
	return nil
}

// enqueueNowPlaying 把待推送项按播放器 ID 入队（同一播放器只保留最新一条，天然去重）。
// 上下文剥离取消信号：HTTP 请求结束后后台推送仍需继续。
func (p *playTracker) enqueueNowPlaying(ctx context.Context, playerId string, userId string, track *model.MediaFile, position int) {
	p.npMu.Lock()
	defer p.npMu.Unlock()
	ctx = context.WithoutCancel(ctx) // Prevent cancellation from affecting background processing
	p.npQueue[playerId] = nowPlayingEntry{
		ctx:      ctx,
		userId:   userId,
		track:    track,
		position: position,
	}
	p.sendNowPlayingSignal()
}

// sendNowPlayingSignal 唤醒后台协程。信号通道容量为 1，
// 已有待处理信号时直接丢弃，绝不阻塞调用方。
func (p *playTracker) sendNowPlayingSignal() {
	// Don't block if the previous signal was not read yet
	select {
	case p.npSignal <- struct{}{}:
	default:
	}
}

// nowPlayingWorker 后台推送协程。
//
// 由信号或 1 秒定时轮询驱动（定时兜底，防止信号丢失导致队列滞留）。
// 处理时先整体换出队列再解锁，使外部推送在锁外进行，
// 期间新到的通知可继续入队，互不阻塞。
func (p *playTracker) nowPlayingWorker() {
	defer close(p.workerDone)
	for {
		select {
		case <-p.shutdown:
			return
		case <-time.After(time.Second):
		case <-p.npSignal:
		}

		p.npMu.Lock()
		if len(p.npQueue) == 0 {
			p.npMu.Unlock()
			continue
		}

		// Keep a copy of the entries to process and clear the queue
		entries := p.npQueue
		p.npQueue = make(map[string]nowPlayingEntry)
		p.npMu.Unlock()

		// Process entries without holding lock
		for _, entry := range entries {
			p.dispatchNowPlaying(entry.ctx, entry.userId, entry.track, entry.position)
		}
	}
}

// dispatchNowPlaying 向所有已授权的 scrobbler 推送「正在播放」。
// 未知艺术家的曲目不外传：外部服务无法匹配，只会产生垃圾数据。
// 单个 scrobbler 失败不影响其余。
func (p *playTracker) dispatchNowPlaying(ctx context.Context, userId string, t *model.MediaFile, position int) {
	if t.Artist == consts.UnknownArtist {
		log.Debug(ctx, "Ignoring external NowPlaying update for track with unknown artist", "track", t.Title, "artist", t.Artist)
		return
	}
	allScrobblers := p.getActiveScrobblers()
	for name, s := range allScrobblers {
		if !s.IsAuthorized(ctx, userId) {
			continue
		}
		log.Debug(ctx, "Sending NowPlaying update", "scrobbler", name, "track", t.Title, "artist", t.Artist, "position", position)
		err := s.NowPlaying(ctx, userId, t, position)
		if err != nil {
			log.Error(ctx, "Error sending NowPlayingInfo", "scrobbler", name, "track", t.Title, "artist", t.Artist, err)
			continue
		}
	}
}

// GetNowPlaying 返回当前所有「正在播放」记录，按开始时间倒序。
func (p *playTracker) GetNowPlaying(_ context.Context) ([]NowPlayingInfo, error) {
	res := p.playMap.Values()
	sort.Slice(res, func(i, j int) bool {
		return res[i].Start.After(res[j].Start)
	})
	return res, nil
}

// Submit 处理播放上报：累加本地播放次数，并按需转发给外部 scrobbler。
//
// 逐条处理，单条失败只记日志——客户端可能一次提交离线期间攒下的多条记录，
// 不应因其中一条无效而整批丢弃。
// 至少成功一条才发送刷新事件，让 UI 更新播放次数。
func (p *playTracker) Submit(ctx context.Context, submissions []Submission) error {
	username, _ := request.UsernameFrom(ctx)
	player, _ := request.PlayerFrom(ctx)
	if !player.ScrobbleEnabled {
		log.Debug(ctx, "External scrobbling disabled for this player", "player", player.Name, "ip", player.IP, "user", username)
	}
	event := &events.RefreshResource{}
	success := 0

	for _, s := range submissions {
		mf, err := p.ds.MediaFile(ctx).GetWithParticipants(s.TrackID)
		if err != nil {
			log.Error(ctx, "Cannot find track for scrobbling", "id", s.TrackID, "user", username, err)
			continue
		}
		err = p.incPlay(ctx, mf, s.Timestamp)
		if err != nil {
			log.Error(ctx, "Error updating play counts", "id", mf.ID, "track", mf.Title, "user", username, err)
		} else {
			success++
			event.With("song", mf.ID).With("album", mf.AlbumID).With("artist", mf.AlbumArtistID)
			log.Info(ctx, "Scrobbled", "title", mf.Title, "artist", mf.Artist, "user", username, "timestamp", s.Timestamp)
			if player.ScrobbleEnabled {
				p.dispatchScrobble(ctx, mf, s.Timestamp)
			}
		}
	}

	if success > 0 {
		p.broker.SendMessage(ctx, event)
	}
	return nil
}

// incPlay 在单个事务中累加曲目、专辑及各参与艺术家的播放次数，
// 并按配置记录播放历史，保证多级计数的一致性。
func (p *playTracker) incPlay(ctx context.Context, track *model.MediaFile, timestamp time.Time) error {
	return p.ds.WithTx(func(tx model.DataStore) error {
		err := tx.MediaFile(ctx).IncPlayCount(track.ID, timestamp)
		if err != nil {
			return err
		}
		err = tx.Album(ctx).IncPlayCount(track.AlbumID, timestamp)
		if err != nil {
			return err
		}
		for _, artist := range track.Participants[model.RoleArtist] {
			err = tx.Artist(ctx).IncPlayCount(artist.ID, timestamp)
			if err != nil {
				return err
			}
		}
		if conf.Server.EnableScrobbleHistory {
			return tx.Scrobble(ctx).RecordScrobble(track.ID, timestamp)
		}
		return nil
	})
}

// dispatchScrobble 把播放记录投递给各 scrobbler 的缓冲区，由其异步提交到外部服务。
func (p *playTracker) dispatchScrobble(ctx context.Context, t *model.MediaFile, playTime time.Time) {
	if t.Artist == consts.UnknownArtist {
		log.Debug(ctx, "Ignoring external Scrobble for track with unknown artist", "track", t.Title, "artist", t.Artist)
		return
	}

	allScrobblers := p.getActiveScrobblers()
	u, _ := request.UserFrom(ctx)
	scrobble := Scrobble{MediaFile: *t, TimeStamp: playTime}
	for name, s := range allScrobblers {
		if !s.IsAuthorized(ctx, u.ID) {
			continue
		}
		log.Debug(ctx, "Buffering Scrobble", "scrobbler", name, "track", t.Title, "artist", t.Artist)
		err := s.Scrobble(ctx, u.ID, scrobble)
		if err != nil {
			log.Error(ctx, "Error sending Scrobble", "scrobbler", name, "track", t.Title, "artist", t.Artist, err)
			continue
		}
	}
}

// constructors 是内置 scrobbler 的注册表。
var constructors map[string]Constructor

// Register 注册内置 scrobbler，通常在各实现包的 init 中调用。
func Register(name string, init Constructor) {
	if constructors == nil {
		constructors = make(map[string]Constructor)
	}
	constructors[name] = init
}
