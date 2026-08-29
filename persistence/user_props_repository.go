package persistence

import (
	"context"
	"errors"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// userPropsRepository 是用户属性仓储，
// 按「用户 + 键」存放用户级偏好与外部服务凭据（如 Last.fm 会话密钥）。
type userPropsRepository struct {
	sqlRepository
}

// NewUserPropsRepository 创建用户属性仓储。
func NewUserPropsRepository(ctx context.Context, db dbx.Builder) model.UserPropsRepository {
	r := &userPropsRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "user_props"
	return r
}

// Put 写入用户属性：先尝试更新，无匹配行再插入。
func (r userPropsRepository) Put(userId, key string, value string) error {
	update := Update(r.tableName).Set("value", value).Where(And{Eq{"user_id": userId}, Eq{"key": key}})
	count, err := r.executeSQL(update)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	insert := Insert(r.tableName).Columns("user_id", "key", "value").Values(userId, key, value)
	_, err = r.executeSQL(insert)
	return err
}

// Get 读取用户属性，不存在时返回 model.ErrNotFound。
func (r userPropsRepository) Get(userId, key string) (string, error) {
	sel := Select("value").From(r.tableName).Where(And{Eq{"user_id": userId}, Eq{"key": key}})
	resp := struct {
		Value string
	}{}
	err := r.queryOne(sel, &resp)
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}

// DefaultGet 读取用户属性，不存在时返回默认值。
func (r userPropsRepository) DefaultGet(userId, key string, defaultValue string) (string, error) {
	value, err := r.Get(userId, key)
	if errors.Is(err, model.ErrNotFound) {
		return defaultValue, nil
	}
	if err != nil {
		return defaultValue, err
	}
	return value, nil
}

// Delete 删除用户属性。
func (r userPropsRepository) Delete(userId, key string) error {
	return r.delete(And{Eq{"user_id": userId}, Eq{"key": key}})
}
