package agents

import (
	"context"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
)

// LocalAgentName 是本地代理的名称，始终启用，作为外部代理的兜底。
const LocalAgentName = "local"

// localAgent 是仅依赖本地数据库的代理，不访问外部网络。
type localAgent struct {
	ds model.DataStore
}

func localsConstructor(ds model.DataStore) Interface {
	return &localAgent{ds}
}

func (p *localAgent) AgentName() string {
	return LocalAgentName
}

// GetArtistTopSongs 用本地数据推断热门单曲：
// 取该艺术家已收藏或满星的曲目，按播放次数降序。
// 没有外部数据时，用户自己的偏好就是最好的「热门」依据。
func (p *localAgent) GetArtistTopSongs(ctx context.Context, id, artistName, mbid string, count int) ([]Song, error) {
	top, err := p.ds.MediaFile(ctx).GetAll(model.QueryOptions{
		Sort:  "playCount",
		Order: "desc",
		Max:   count,
		Filters: squirrel.And{
			squirrel.Eq{"artist_id": id},
			squirrel.Or{
				squirrel.Eq{"starred": true},
				squirrel.Eq{"rating": 5},
			},
		},
	})
	if err != nil {
		return nil, err
	}
	var result []Song
	for _, s := range top {
		result = append(result, Song{
			Name: s.Title,
			MBID: s.MbzReleaseTrackID,
		})
	}
	return result, nil
}

func init() {
	Register(LocalAgentName, localsConstructor)
}
