package core

import (
	"context"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	gonanoid "github.com/matoous/go-nanoid/v2"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/navidrome/navidrome/utils/str"
)

// Share 管理公开分享链接。
type Share interface {
	Load(ctx context.Context, id string) (*model.Share, error)
	NewRepository(ctx context.Context) rest.Repository
}

// NewShare 创建分享服务。
func NewShare(ds model.DataStore) Share {
	return &shareService{
		ds: ds,
	}
}

type shareService struct {
	ds model.DataStore
}

// Load 读取分享并累加访问计数。
// 已过期返回 ErrExpired；计数更新失败只告警，不影响本次访问。
func (s *shareService) Load(ctx context.Context, id string) (*model.Share, error) {
	repo := s.ds.Share(ctx)
	share, err := repo.Get(id)
	if err != nil {
		return nil, err
	}
	expiresAt := V(share.ExpiresAt)
	if !expiresAt.IsZero() && expiresAt.Before(time.Now()) {
		return nil, model.ErrExpired
	}
	share.LastVisitedAt = P(time.Now())
	share.VisitCount++

	err = repo.(rest.Persistable).Update(id, share, "last_visited_at", "visit_count")
	if err != nil {
		log.Warn(ctx, "Could not increment visit count for share", "share", share.ID)
	}
	return share, nil
}

// NewRepository 返回带业务逻辑的分享仓储：负责生成短 ID、推断资源类型与内容描述。
func (s *shareService) NewRepository(ctx context.Context) rest.Repository {
	repo := s.ds.Share(ctx)
	wrapper := &shareRepositoryWrapper{
		ctx:             ctx,
		ShareRepository: repo,
		Repository:      repo.(rest.Repository),
		Persistable:     repo.(rest.Persistable),
		ds:              s.ds,
	}
	return wrapper
}

type shareRepositoryWrapper struct {
	model.ShareRepository
	rest.Repository
	rest.Persistable
	ctx context.Context
	ds  model.DataStore
}

// newId 生成 10 位 nanoid 作为分享短链接 ID，循环直到不与现有 ID 冲突。
func (r *shareRepositoryWrapper) newId() (string, error) {
	for {
		id, err := gonanoid.Generate("0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", 10)
		if err != nil {
			return "", err
		}
		exists, err := r.Exists(id)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
}

// Save 创建分享。
//
// 资源类型由第一个 ID 反查实体推断得出（前端只传 ID 列表），
// 据此生成用于展示的内容描述并截断至 30 字符。
// 未指定过期时间时套用配置的默认时长。
func (r *shareRepositoryWrapper) Save(entity interface{}) (string, error) {
	s := entity.(*model.Share)
	id, err := r.newId()
	if err != nil {
		return "", err
	}
	s.ID = id
	if V(s.ExpiresAt).IsZero() {
		s.ExpiresAt = P(time.Now().Add(conf.Server.DefaultShareExpiration))
	}

	firstId := strings.SplitN(s.ResourceIDs, ",", 2)[0]
	v, err := model.GetEntityByID(r.ctx, r.ds, firstId)
	if err != nil {
		return "", err
	}
	switch v.(type) {
	case *model.Artist:
		s.ResourceType = "artist"
		s.Contents = r.contentsLabelFromArtist(s.ID, s.ResourceIDs)
	case *model.Album:
		s.ResourceType = "album"
		s.Contents = r.contentsLabelFromAlbums(s.ID, s.ResourceIDs)
	case *model.Playlist:
		s.ResourceType = "playlist"
		s.Contents = r.contentsLabelFromPlaylist(s.ID, s.ResourceIDs)
	case *model.MediaFile:
		s.ResourceType = "media_file"
		s.Contents = r.contentsLabelFromMediaFiles(s.ID, s.ResourceIDs)
	default:
		log.Error(r.ctx, "Invalid Resource ID", "id", firstId)
		return "", model.ErrNotFound
	}

	s.Contents = str.TruncateRunes(s.Contents, 30, "...")

	id, err = r.Persistable.Save(s)
	return id, err
}

// Update 只允许修改描述、是否可下载与过期时间，
// 分享内容本身不可变更（否则链接接收方看到的东西会被偷换）。
func (r *shareRepositoryWrapper) Update(id string, entity interface{}, _ ...string) error {
	cols := []string{"description", "downloadable"}

	// TODO Better handling of Share expiration
	if !V(entity.(*model.Share).ExpiresAt).IsZero() {
		cols = append(cols, "expires_at")
	}
	return r.Persistable.Update(id, entity, cols...)
}

// contentsLabelFromArtist 取艺术家名作为分享描述。
func (r *shareRepositoryWrapper) contentsLabelFromArtist(shareID string, ids string) string {
	idList := strings.SplitN(ids, ",", 2)
	a, err := r.ds.Artist(r.ctx).Get(idList[0])
	if err != nil {
		log.Error(r.ctx, "Error retrieving artist name for share", "share", shareID, err)
		return ""
	}
	return a.Name
}

// contentsLabelFromAlbums 用逗号连接所有专辑名作为分享描述。
func (r *shareRepositoryWrapper) contentsLabelFromAlbums(shareID string, ids string) string {
	idList := strings.Split(ids, ",")
	all, err := r.ds.Album(r.ctx).GetAll(model.QueryOptions{Filters: squirrel.Eq{"album.id": idList}})
	if err != nil {
		log.Error(r.ctx, "Error retrieving album names for share", "share", shareID, err)
		return ""
	}
	names := slice.Map(all, func(a model.Album) string { return a.Name })
	return strings.Join(names, ", ")
}

// contentsLabelFromPlaylist 取播放列表名作为分享描述。
func (r *shareRepositoryWrapper) contentsLabelFromPlaylist(shareID string, id string) string {
	pls, err := r.ds.Playlist(r.ctx).Get(id)
	if err != nil {
		log.Error(r.ctx, "Error retrieving album names for share", "share", shareID, err)
		return ""
	}
	return pls.Name
}

// contentsLabelFromMediaFiles 为曲目分享挑选最贴切的描述：
// 单曲用标题；同属一张专辑用专辑名；同属一位艺术家用艺术家名；
// 都不满足则退回首曲标题。
func (r *shareRepositoryWrapper) contentsLabelFromMediaFiles(shareID string, ids string) string {
	idList := strings.Split(ids, ",")
	mfs, err := r.ds.MediaFile(r.ctx).GetAll(model.QueryOptions{Filters: squirrel.And{
		squirrel.Eq{"media_file.id": idList},
		squirrel.Eq{"missing": false},
	}})
	if err != nil {
		log.Error(r.ctx, "Error retrieving media files for share", "share", shareID, err)
		return ""
	}

	if len(mfs) == 1 {
		return mfs[0].Title
	}

	albums := slice.Group(mfs, func(mf model.MediaFile) string {
		return mf.Album
	})
	if len(albums) == 1 {
		for name := range albums {
			return name
		}
	}
	artists := slice.Group(mfs, func(mf model.MediaFile) string {
		return mf.AlbumArtist
	})
	if len(artists) == 1 {
		for name := range artists {
			return name
		}
	}

	return mfs[0].Title
}
