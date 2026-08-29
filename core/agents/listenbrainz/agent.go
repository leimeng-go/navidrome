package listenbrainz

import (
	"context"
	"errors"
	"net/http"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/core/scrobbler"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/cache"
	"github.com/navidrome/navidrome/utils/slice"
)

const (
	listenBrainzAgentName = "listenbrainz"
	sessionKeyProperty    = "ListenBrainzSessionKey"
)

// listenBrainzAgent 是 ListenBrainz 的 scrobbler 实现。
// ListenBrainz 不提供元数据查询，故只实现上报能力。
type listenBrainzAgent struct {
	ds          model.DataStore
	sessionKeys *agents.SessionKeys
	baseURL     string
	client      *client
}

// listenBrainzConstructor 构造代理。BaseURL 可配置，以支持自建实例。
func listenBrainzConstructor(ds model.DataStore) *listenBrainzAgent {
	l := &listenBrainzAgent{
		ds:          ds,
		sessionKeys: &agents.SessionKeys{DataStore: ds, KeyName: sessionKeyProperty},
		baseURL:     conf.Server.ListenBrainz.BaseURL,
	}
	hc := &http.Client{
		Timeout: consts.DefaultHttpClientTimeOut,
	}
	chc := cache.NewHTTPClient(hc, consts.DefaultHttpClientTimeOut)
	l.client = newClient(l.baseURL, chc)
	return l
}

// AgentName 返回代理名。
func (l *listenBrainzAgent) AgentName() string {
	return listenBrainzAgentName
}

// formatListen 把媒体文件转换为 ListenBrainz 的上报结构。
// 除合并署名外还带上逐个艺人的名称与 MBID，让服务端能精确关联。
func (l *listenBrainzAgent) formatListen(track *model.MediaFile) listenInfo {
	artistMBIDs := slice.Map(track.Participants[model.RoleArtist], func(p model.Participant) string {
		return p.MbzArtistID
	})
	artistNames := slice.Map(track.Participants[model.RoleArtist], func(p model.Participant) string {
		return p.Name
	})
	li := listenInfo{
		TrackMetadata: trackMetadata{
			ArtistName:  track.Artist,
			TrackName:   track.Title,
			ReleaseName: track.Album,
			AdditionalInfo: additionalInfo{
				SubmissionClient:        consts.AppName,
				SubmissionClientVersion: consts.Version,
				TrackNumber:             track.TrackNumber,
				ArtistNames:             artistNames,
				ArtistMBIDs:             artistMBIDs,
				RecordingMBID:           track.MbzRecordingID,
				ReleaseMBID:             track.MbzAlbumID,
				ReleaseGroupMBID:        track.MbzReleaseGroupID,
				DurationMs:              int(track.Duration * 1000),
			},
		},
	}
	return li
}

// NowPlaying 上报「正在播放」，失败视为不可恢复（该状态无重试价值）。
func (l *listenBrainzAgent) NowPlaying(ctx context.Context, userId string, track *model.MediaFile, position int) error {
	sk, err := l.sessionKeys.Get(ctx, userId)
	if err != nil || sk == "" {
		return errors.Join(err, scrobbler.ErrNotAuthorized)
	}

	li := l.formatListen(track)
	err = l.client.updateNowPlaying(ctx, sk, li)
	if err != nil {
		log.Warn(ctx, "ListenBrainz updateNowPlaying returned error", "track", track.Title, err)
		return errors.Join(err, scrobbler.ErrUnrecoverable)
	}
	return nil
}

// Scrobble 上报播放记录。
// 网络错误与服务端 500/503 标记为可重试，其余（如认证失败）不再重试。
func (l *listenBrainzAgent) Scrobble(ctx context.Context, userId string, s scrobbler.Scrobble) error {
	sk, err := l.sessionKeys.Get(ctx, userId)
	if err != nil || sk == "" {
		return errors.Join(err, scrobbler.ErrNotAuthorized)
	}

	li := l.formatListen(&s.MediaFile)
	li.ListenedAt = int(s.TimeStamp.Unix())
	err = l.client.scrobble(ctx, sk, li)

	if err == nil {
		return nil
	}
	var lbErr *listenBrainzError
	isListenBrainzError := errors.As(err, &lbErr)
	if !isListenBrainzError {
		log.Warn(ctx, "ListenBrainz Scrobble returned HTTP error", "track", s.Title, err)
		return errors.Join(err, scrobbler.ErrRetryLater)
	}
	if lbErr.Code == 500 || lbErr.Code == 503 {
		return errors.Join(err, scrobbler.ErrRetryLater)
	}
	return errors.Join(err, scrobbler.ErrUnrecoverable)
}

// IsAuthorized 判断用户是否已配置有效的 token。
func (l *listenBrainzAgent) IsAuthorized(ctx context.Context, userId string) bool {
	sk, err := l.sessionKeys.Get(ctx, userId)
	return err == nil && sk != ""
}

// init 在配置加载后按开关注册 scrobbler。
func init() {
	conf.AddHook(func() {
		if conf.Server.ListenBrainz.Enabled {
			scrobbler.Register(listenBrainzAgentName, func(ds model.DataStore) scrobbler.Scrobbler {
				return listenBrainzConstructor(ds)
			})
		}
	})
}
