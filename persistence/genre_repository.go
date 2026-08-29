package persistence

import (
	"context"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// genreRepository 是流派仓储。
// 流派本质上就是名为 genre 的标签，故直接复用标签基类并固定过滤条件；
// 存在独立类型是为了兼容 Subsonic API 中「流派」这一独立概念。
type genreRepository struct {
	*baseTagRepository
}

// NewGenreRepository 创建流派仓储。
func NewGenreRepository(ctx context.Context, db dbx.Builder) model.GenreRepository {
	genreFilter := model.TagGenre
	return &genreRepository{
		baseTagRepository: newBaseTagRepository(ctx, db, &genreFilter),
	}
}

// selectGenre 把标签取值映射为 Genre 的 name 字段。
func (r *genreRepository) selectGenre(opt ...model.QueryOptions) SelectBuilder {
	return r.newSelect(opt...).Columns("tag.tag_value as name")
}

// GetAll 返回全部流派。
func (r *genreRepository) GetAll(opt ...model.QueryOptions) (model.Genres, error) {
	sq := r.selectGenre(opt...)
	res := model.Genres{}
	err := r.queryAll(sq, &res)
	return res, err
}

// Override ResourceRepository methods to return Genre objects instead of Tag objects
// 覆写基类方法，使 REST 层返回 Genre 而非 Tag。

func (r *genreRepository) Read(id string) (interface{}, error) {
	sel := r.selectGenre().Where(Eq{"tag.id": id})
	var res model.Genre
	err := r.queryOne(sel, &res)
	return &res, err
}

func (r *genreRepository) ReadAll(options ...rest.QueryOptions) (interface{}, error) {
	return r.GetAll(r.parseRestOptions(r.ctx, options...))
}

func (r *genreRepository) NewInstance() interface{} {
	return &model.Genre{}
}

var _ model.GenreRepository = (*genreRepository)(nil)
var _ model.ResourceRepository = (*genreRepository)(nil)
