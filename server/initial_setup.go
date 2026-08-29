package server

import (
	"context"
	"fmt"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/core/ffmpeg"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
)

// initialSetup 执行首次启动初始化。
// 整个过程放在一个事务里，避免中途失败留下半初始化状态。
// 通过属性表中的标志位保证只执行一次；音乐库目录则每次启动都同步。
func initialSetup(ds model.DataStore) {
	ctx := context.TODO()
	_ = ds.WithTx(func(tx model.DataStore) error {
		if err := tx.Library(ctx).StoreMusicFolder(); err != nil {
			return err
		}

		properties := tx.Property(ctx)
		_, err := properties.Get(consts.InitialSetupFlagKey)
		if err == nil {
			return nil
		}
		log.Info("Running initial setup")
		if conf.Server.DevAutoCreateAdminPassword != "" {
			if err = createInitialAdminUser(tx, conf.Server.DevAutoCreateAdminPassword); err != nil {
				return err
			}
		}

		err = properties.Put(consts.InitialSetupFlagKey, time.Now().String())
		return err
	}, "initial setup")
}

// If the Dev Admin user is not present, create it
// createInitialAdminUser 创建开发用管理员账号。
// 仅在配置了 DevAutoCreateAdminPassword 时触发，密码会明文写进日志，
// 因此只可用于开发环境。
func createInitialAdminUser(ds model.DataStore, initialPassword string) error {
	users := ds.User(context.TODO())
	c, err := users.CountAll(model.QueryOptions{Filters: squirrel.Eq{"user_name": consts.DevInitialUserName}})
	if err != nil {
		panic(fmt.Sprintf("Could not access User table: %s", err))
	}
	if c == 0 {
		newID := id.NewRandom()
		log.Warn("Creating initial admin user. This should only be used for development purposes!!",
			"user", consts.DevInitialUserName, "password", initialPassword, "id", newID)
		initialUser := model.User{
			ID:          newID,
			UserName:    consts.DevInitialUserName,
			Name:        consts.DevInitialName,
			Email:       "",
			NewPassword: initialPassword,
			IsAdmin:     true,
		}
		err := users.Put(&initialUser)
		if err != nil {
			log.Error("Could not create initial admin user", "user", initialUser, err)
		}
	}
	return err
}

// checkFFmpegInstallation 检查 ffmpeg 是否可用。
// 缺失时不阻断启动（转码功能才需要），但若元数据提取器配的是 ffmpeg，
// 则必须回退到 taglib，否则扫描将完全失效。
func checkFFmpegInstallation() {
	f := ffmpeg.New()
	_, err := f.CmdPath()
	if err == nil {
		return
	}
	log.Warn("Unable to find ffmpeg. Transcoding will fail if used", err)
	if conf.Server.Scanner.Extractor == "ffmpeg" {
		log.Warn("ffmpeg cannot be used for metadata extraction. Falling back to taglib")
		conf.Server.Scanner.Extractor = "taglib"
	}
}

// checkExternalCredentials 在启动时输出各外部服务的启用状态，便于排查配置遗漏。
func checkExternalCredentials() {
	if conf.Server.EnableExternalServices {
		if !conf.Server.LastFM.Enabled {
			log.Info("Last.fm integration is DISABLED")
		} else {
			log.Debug("Last.fm integration is ENABLED")
		}

		if !conf.Server.ListenBrainz.Enabled {
			log.Info("ListenBrainz integration is DISABLED")
		} else {
			log.Debug("ListenBrainz integration is ENABLED", "ListenBrainz.BaseURL", conf.Server.ListenBrainz.BaseURL)
		}

		if conf.Server.Spotify.ID == "" || conf.Server.Spotify.Secret == "" {
			log.Info("Spotify integration is not enabled: missing ID/Secret")
		} else {
			log.Debug("Spotify integration is ENABLED")
		}
	}
}
