package model

import (
	"context"
)

// TODO: Should the type be encoded in the ID?
// GetEntityByID 在不知道实体类型的情况下按 ID 查找对象。
// Subsonic API 的部分接口（如 star/unstar）只给 ID 不给类型，只能依次试探：
// 艺人 → 专辑 → 播放列表 → 曲目，命中即返回。
//
// TODO 若把类型信息编码进 ID，即可一次定位，省去多轮查询。
func GetEntityByID(ctx context.Context, ds DataStore, id string) (interface{}, error) {
	ar, err := ds.Artist(ctx).Get(id)
	if err == nil {
		return ar, nil
	}
	al, err := ds.Album(ctx).Get(id)
	if err == nil {
		return al, nil
	}
	pls, err := ds.Playlist(ctx).Get(id)
	if err == nil {
		return pls, nil
	}
	mf, err := ds.MediaFile(ctx).Get(id)
	if err == nil {
		return mf, nil
	}
	return nil, err
}
