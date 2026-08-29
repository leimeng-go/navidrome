package core

import (
	"context"
	"fmt"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils"
)

// Players 管理播放器注册与查询。
// 每个「用户 + 客户端 + UserAgent」组合对应一个播放器记录，
// 用于保存各客户端独立的转码与音量偏好。
type Players interface {
	Get(ctx context.Context, playerId string) (*model.Player, error)
	Register(ctx context.Context, id, client, userAgent, ip string) (*model.Player, *model.Transcoding, error)
}

// NewPlayers 创建播放器服务。
// limiter 用于限制写库频率——每次请求都会带上播放器信息，
// 若逐次落库将产生大量无谓写入。
func NewPlayers(ds model.DataStore) Players {
	return &players{
		ds:      ds,
		limiter: utils.Limiter{Interval: consts.UpdatePlayerFrequency},
	}
}

type players struct {
	ds      model.DataStore
	limiter utils.Limiter
}

// Register 注册或更新播放器，并返回其绑定的转码配置。
//
// 查找顺序：先按 ID 命中（ID 与 client 不符则视为无效，防止 ID 被冒用），
// 再按「用户+客户端+UserAgent」匹配已有记录，最后才新建。
//
// 写库经限流器节流，且单独设置 1 秒超时——
// 播放器信息更新属于次要操作，不应拖慢主请求。
func (p *players) Register(ctx context.Context, playerID, client, userAgent, ip string) (*model.Player, *model.Transcoding, error) {
	var plr *model.Player
	var trc *model.Transcoding
	var err error
	user, _ := request.UserFrom(ctx)
	if playerID != "" {
		plr, err = p.ds.Player(ctx).Get(playerID)
		if err == nil && plr.Client != client {
			playerID = ""
		}
	}
	username := userName(ctx)
	if err != nil || playerID == "" {
		plr, err = p.ds.Player(ctx).FindMatch(user.ID, client, userAgent)
		if err == nil {
			log.Debug(ctx, "Found matching player", "id", plr.ID, "client", client, "username", username, "type", userAgent)
		} else {
			plr = &model.Player{
				ID:              id.NewRandom(),
				UserId:          user.ID,
				Client:          client,
				ScrobbleEnabled: true,
				ReportRealPath:  conf.Server.Subsonic.DefaultReportRealPath,
			}
			log.Info(ctx, "Registering new player", "id", plr.ID, "client", client, "username", username, "type", userAgent)
		}
	}
	plr.Name = fmt.Sprintf("%s [%s]", client, userAgent)
	plr.UserAgent = userAgent
	plr.IP = ip
	plr.LastSeen = time.Now()
	p.limiter.Do(plr.ID, func() {
		ctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()

		err = p.ds.Player(ctx).Put(plr)
		if err != nil {
			log.Warn(ctx, "Could not save player", "id", plr.ID, "client", client, "username", username, "type", userAgent, err)
		}
	})
	if plr.TranscodingId != "" {
		trc, err = p.ds.Transcoding(ctx).Get(plr.TranscodingId)
	}
	return plr, trc, err
}

// Get 按 ID 查询播放器。
func (p *players) Get(ctx context.Context, playerId string) (*model.Player, error) {
	return p.ds.Player(ctx).Get(playerId)
}
