package persistence

import (
	"context"
	"errors"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// transcodingRepository 是转码配置仓储。
// 每条记录定义一种目标格式及其转码命令行。
// 读取对所有用户开放（播放时需要），但增删改限管理员——
// 转码配置含可执行命令，属高危配置。
type transcodingRepository struct {
	sqlRepository
}

// NewTranscodingRepository 创建转码配置仓储。
func NewTranscodingRepository(ctx context.Context, db dbx.Builder) model.TranscodingRepository {
	r := &transcodingRepository{}
	r.ctx = ctx
	r.db = db
	r.registerModel(&model.Transcoding{}, nil)
	return r
}

// Get 按 ID 读取转码配置。
func (r *transcodingRepository) Get(id string) (*model.Transcoding, error) {
	sel := r.newSelect().Columns("*").Where(Eq{"id": id})
	var res model.Transcoding
	err := r.queryOne(sel, &res)
	return &res, err
}

// CountAll 统计转码配置数量。
func (r *transcodingRepository) CountAll(qo ...model.QueryOptions) (int64, error) {
	return r.count(Select(), qo...)
}

// FindByFormat 按目标格式查找转码配置，播放时据此选择转码方式。
func (r *transcodingRepository) FindByFormat(format string) (*model.Transcoding, error) {
	sel := r.newSelect().Columns("*").Where(Eq{"target_format": format})
	var res model.Transcoding
	err := r.queryOne(sel, &res)
	return &res, err
}

// Put 写入转码配置，仅管理员可调用。
func (r *transcodingRepository) Put(t *model.Transcoding) error {
	if !loggedUser(r.ctx).IsAdmin {
		return rest.ErrPermissionDenied
	}
	_, err := r.put(t.ID, t)
	return err
}

// 以下实现 rest 接口，写操作一律限管理员。

func (r *transcodingRepository) Count(options ...rest.QueryOptions) (int64, error) {
	return r.count(Select(), r.parseRestOptions(r.ctx, options...))
}

func (r *transcodingRepository) Read(id string) (interface{}, error) {
	return r.Get(id)
}

func (r *transcodingRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	sel := r.newSelect(r.parseRestOptions(r.ctx, options...)).Columns("*")
	res := model.Transcodings{}
	err := r.queryAll(sel, &res)
	return res, err
}

func (r *transcodingRepository) EntityName() string {
	return "transcoding"
}

func (r *transcodingRepository) NewInstance() interface{} {
	return &model.Transcoding{}
}

func (r *transcodingRepository) Save(entity interface{}) (string, error) {
	if !loggedUser(r.ctx).IsAdmin {
		return "", rest.ErrPermissionDenied
	}
	t := entity.(*model.Transcoding)
	id, err := r.put(t.ID, t)
	if errors.Is(err, model.ErrNotFound) {
		return "", rest.ErrNotFound
	}
	return id, err
}

func (r *transcodingRepository) Update(id string, entity interface{}, cols ...string) error {
	if !loggedUser(r.ctx).IsAdmin {
		return rest.ErrPermissionDenied
	}
	t := entity.(*model.Transcoding)
	t.ID = id
	_, err := r.put(id, t)
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

func (r *transcodingRepository) Delete(id string) error {
	if !loggedUser(r.ctx).IsAdmin {
		return rest.ErrPermissionDenied
	}
	err := r.delete(Eq{"id": id})
	if errors.Is(err, model.ErrNotFound) {
		return rest.ErrNotFound
	}
	return err
}

var _ model.TranscodingRepository = (*transcodingRepository)(nil)
var _ rest.Repository = (*transcodingRepository)(nil)
var _ rest.Persistable = (*transcodingRepository)(nil)
