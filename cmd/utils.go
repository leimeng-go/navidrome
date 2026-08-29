package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/navidrome/navidrome/core/auth"
	"github.com/navidrome/navidrome/db"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/persistence"
)

// getAdminContext 为 CLI 命令构造管理员上下文。
// 命令行操作没有登录态，故借库中已有的管理员身份执行；
// 一个管理员都没有说明数据库尚未初始化，直接终止。
func getAdminContext(ctx context.Context) (model.DataStore, context.Context) {
	sqlDB := db.Db()
	ds := persistence.New(sqlDB)
	ctx = auth.WithAdminUser(ctx, ds)
	u, _ := request.UserFrom(ctx)
	if !u.IsAdmin {
		log.Fatal(ctx, "There must be at least one admin user to run this command.")
	}
	return ds, ctx
}

// getUser 按用户名或 ID 查找用户，先按名称再按 ID，方便命令行两种写法都能用。
func getUser(ctx context.Context, id string, ds model.DataStore) (*model.User, error) {
	user, err := ds.User(ctx).FindByUsername(id)

	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, fmt.Errorf("finding user by name: %w", err)
	}

	if errors.Is(err, model.ErrNotFound) {
		user, err = ds.User(ctx).Get(id)
		if err != nil {
			return nil, fmt.Errorf("finding user by id: %w", err)
		}
	}

	return user, nil
}
