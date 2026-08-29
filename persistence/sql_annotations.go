package persistence

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/log"
)

// 本文件实现用户标注（收藏、评分、播放次数）的读写。
//
// 标注独立存于 annotation 表，按 (user_id, item_type, item_id) 唯一，
// 从而让同一首曲目对不同用户有各自的收藏与评分。
// 这些方法由 sqlRepository 提供，各仓储内嵌后即获得标注能力。

const annotationTable = "annotation"

// withAnnotation 给查询左连接当前用户的标注并追加相关列。
//
// 用 LEFT JOIN 而非 INNER JOIN：从未被标注过的条目也必须出现在结果中。
// 因此各列用 coalesce 兜默认值，避免上层拿到 NULL。
//
// 未登录时直接跳过——此时无标注可言，且能省掉一次 JOIN。
func (r sqlRepository) withAnnotation(query SelectBuilder, idField string) SelectBuilder {
	userID := loggedUser(r.ctx).ID
	if userID == invalidUserId {
		return query
	}
	query = query.
		LeftJoin("annotation on ("+
			"annotation.item_id = "+idField+
			" AND annotation.user_id = '"+userID+"')").
		Columns(
			"coalesce(starred, 0) as starred",
			"coalesce(rating, 0) as rating",
			"starred_at",
			"play_date",
			"rated_at",
		)
	// 归一化模式下，专辑播放次数取「曲目播放总次数 / 曲目数」的均值，
	// 使听完整张专辑记为 1 次，而非累加成曲目数那么多次
	if conf.Server.AlbumPlayCountMode == consts.AlbumPlayCountModeNormalized && r.tableName == "album" {
		query = query.Columns(
			fmt.Sprintf("round(coalesce(round(cast(play_count as float) / coalesce(%[1]s.song_count, 1), 1), 0)) as play_count", r.tableName),
		)
	} else {
		query = query.Columns("coalesce(play_count, 0) as play_count")
	}

	return query
}

// annId 构造定位标注记录的条件：当前用户 + 当前实体类型 + 指定 ID。
func (r sqlRepository) annId(itemID ...string) And {
	userID := loggedUser(r.ctx).ID
	return And{
		Eq{annotationTable + ".user_id": userID},
		Eq{annotationTable + ".item_type": r.tableName},
		Eq{annotationTable + ".item_id": itemID},
	}
}

// annUpsert 写入标注：先批量 UPDATE，未命中任何行再逐条 INSERT。
//
// 采用「先更新后插入」而非 INSERT OR REPLACE，
// 是为了保留同一行中未被本次修改的其他字段
// （例如只改评分时不应清掉收藏状态与播放次数）。
func (r sqlRepository) annUpsert(values map[string]interface{}, itemIDs ...string) error {
	upd := Update(annotationTable).Where(r.annId(itemIDs...))
	for f, v := range values {
		upd = upd.Set(f, v)
	}
	c, err := r.executeSQL(upd)
	if c == 0 || errors.Is(err, sql.ErrNoRows) {
		userID := loggedUser(r.ctx).ID
		for _, itemID := range itemIDs {
			values["user_id"] = userID
			values["item_type"] = r.tableName
			values["item_id"] = itemID
			ins := Insert(annotationTable).SetMap(values)
			_, err = r.executeSQL(ins)
			if err != nil {
				return err
			}
		}
	}
	return err
}

// SetStar 设置或取消收藏，可批量操作。
func (r sqlRepository) SetStar(starred bool, ids ...string) error {
	starredAt := time.Now()
	return r.annUpsert(map[string]interface{}{"starred": starred, "starred_at": starredAt}, ids...)
}

// SetRating 设置评分。
func (r sqlRepository) SetRating(rating int, itemID string) error {
	ratedAt := time.Now()
	return r.annUpsert(map[string]interface{}{"rating": rating, "rated_at": ratedAt}, itemID)
}

// IncPlayCount 递增播放次数并更新最近播放时间。
//
// 计数用 SQL 表达式自增而非「读取后写回」，以避免并发下丢失更新。
// 播放时间取 max：客户端可能延迟上报或补报历史记录，
// 不能让旧时间戳覆盖更新的时间。ifnull 处理首次为 NULL 的情况。
func (r sqlRepository) IncPlayCount(itemID string, ts time.Time) error {
	upd := Update(annotationTable).Where(r.annId(itemID)).
		Set("play_count", Expr("play_count+1")).
		Set("play_date", Expr("max(ifnull(play_date,''),?)", ts))
	c, err := r.executeSQL(upd)

	if c == 0 || errors.Is(err, sql.ErrNoRows) {
		userID := loggedUser(r.ctx).ID
		values := map[string]interface{}{}
		values["user_id"] = userID
		values["item_type"] = r.tableName
		values["item_id"] = itemID
		values["play_count"] = 1
		values["play_date"] = ts
		ins := Insert(annotationTable).SetMap(values)
		_, err = r.executeSQL(ins)
		if err != nil {
			return err
		}
	}
	return err
}

// ReassignAnnotation 把标注从旧 ID 迁移到新 ID。
//
// 当实体的持久化 ID 因标签变更而改变时，用它把用户数据带过去，
// 避免收藏与播放记录丢失。注意此处不限定用户：所有用户的标注一起迁移。
func (r sqlRepository) ReassignAnnotation(prevID string, newID string) error {
	if prevID == newID || prevID == "" || newID == "" {
		return nil
	}
	upd := Update(annotationTable).Where(And{
		Eq{annotationTable + ".item_type": r.tableName},
		Eq{annotationTable + ".item_id": prevID},
	}).Set("item_id", newID)
	_, err := r.executeSQL(upd)
	return err
}

// cleanAnnotations 删除指向已不存在实体的孤立标注，由 GC 调用。
func (r sqlRepository) cleanAnnotations() error {
	del := Delete(annotationTable).Where(Eq{"item_type": r.tableName}).Where("item_id not in (select id from " + r.tableName + ")")
	c, err := r.executeSQL(del)
	if err != nil {
		return fmt.Errorf("error cleaning up %s annotations: %w", r.tableName, err)
	}
	if c > 0 {
		log.Debug(r.ctx, "Clean-up annotations", "table", r.tableName, "totalDeleted", c)
	}
	return nil
}
