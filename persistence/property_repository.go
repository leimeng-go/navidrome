package persistence

import (
	"context"
	"errors"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// propertyRepository 是系统属性仓储，
// 一张简单的键值表，用于存放数据库版本、密码加密指纹等内部状态。
type propertyRepository struct {
	sqlRepository
}

// NewPropertyRepository 创建属性仓储。
func NewPropertyRepository(ctx context.Context, db dbx.Builder) model.PropertyRepository {
	r := &propertyRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "property"
	return r
}

// Put 写入属性：先尝试更新，无匹配行再插入。
func (r propertyRepository) Put(id string, value string) error {
	update := Update(r.tableName).Set("value", value).Where(Eq{"id": id})
	count, err := r.executeSQL(update)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	insert := Insert(r.tableName).Columns("id", "value").Values(id, value)
	_, err = r.executeSQL(insert)
	return err
}

// Get 读取属性值，不存在时返回 model.ErrNotFound。
func (r propertyRepository) Get(id string) (string, error) {
	sel := Select("value").From(r.tableName).Where(Eq{"id": id})
	resp := struct {
		Value string
	}{}
	err := r.queryOne(sel, &resp)
	if err != nil {
		return "", err
	}
	return resp.Value, nil
}

// DefaultGet 读取属性值，不存在时返回默认值。
func (r propertyRepository) DefaultGet(id string, defaultValue string) (string, error) {
	value, err := r.Get(id)
	if errors.Is(err, model.ErrNotFound) {
		return defaultValue, nil
	}
	if err != nil {
		return defaultValue, err
	}
	return value, nil
}

// Delete 删除属性。
func (r propertyRepository) Delete(id string) error {
	return r.delete(Eq{"id": id})
}
