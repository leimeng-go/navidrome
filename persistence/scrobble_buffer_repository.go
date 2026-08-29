package persistence

import (
	"context"
	"errors"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/id"
	"github.com/pocketbase/dbx"
)

// scrobbleBufferRepository 是听歌记录（scrobble）的发送缓冲队列仓储。
//
// 上报到 Last.fm、ListenBrainz 等外部服务可能因网络或服务故障失败，
// 故先落库缓冲，再由后台任务逐条重试发送，避免丢失播放记录。
type scrobbleBufferRepository struct {
	sqlRepository
}

// dbScrobbleBuffer 嵌入 dbMediaFile 以复用曲目字段解析。
type dbScrobbleBuffer struct {
	dbMediaFile
	*model.ScrobbleEntry `structs:",flatten"`
}

// PostScan 解析曲目后回填 ID。
// 查询同时选出曲目与缓冲记录两张表的列，ID 会互相覆盖，故需显式修正。
func (t *dbScrobbleBuffer) PostScan() error {
	if err := t.dbMediaFile.PostScan(); err != nil {
		return err
	}
	t.ScrobbleEntry.MediaFile = *t.dbMediaFile.MediaFile
	t.ScrobbleEntry.MediaFile.ID = t.MediaFileID
	return nil
}

// NewScrobbleBufferRepository 创建 scrobble 缓冲仓储。
func NewScrobbleBufferRepository(ctx context.Context, db dbx.Builder) model.ScrobbleBufferRepository {
	r := &scrobbleBufferRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "scrobble_buffer"
	return r
}

// UserIDs 返回该服务下有待发送记录的用户 ID。
// 按积压条数升序，让积压少的用户先被处理，
// 避免某个大量积压的用户长时间独占发送队列。
func (r *scrobbleBufferRepository) UserIDs(service string) ([]string, error) {
	sql := Select().Columns("user_id").
		From(r.tableName).
		Where(And{
			Eq{"service": service},
		}).
		GroupBy("user_id").
		OrderBy("count(*)")
	var userIds []string
	err := r.queryAllSlice(sql, &userIds)
	return userIds, err
}

// Enqueue 把一条播放记录加入待发送队列。
func (r *scrobbleBufferRepository) Enqueue(service, userId, mediaFileId string, playTime time.Time) error {
	ins := Insert(r.tableName).SetMap(map[string]interface{}{
		"id":            id.NewRandom(),
		"user_id":       userId,
		"service":       service,
		"media_file_id": mediaFileId,
		"play_time":     playTime,
		"enqueue_time":  time.Now(),
	})
	_, err := r.executeSQL(ins)
	return err
}

// Next 取出下一条待发送记录（按播放时间先后），队列为空时返回 (nil, nil)。
// 同时加载参与者信息——外部服务需要艺人名等元数据。
func (r *scrobbleBufferRepository) Next(service string, userId string) (*model.ScrobbleEntry, error) {
	// Put `s.*` last or else m.id overrides s.id
	// s.* 必须放在 m.* 之后：列名重复时后者生效，
	// 这样 id 取的是缓冲记录的 ID（Dequeue 需要它）
	sql := Select().Columns("m.*, s.*").
		From(r.tableName+" s").
		LeftJoin("media_file m on m.id = s.media_file_id").
		Where(And{
			Eq{"service": service},
			Eq{"user_id": userId},
		}).
		OrderBy("play_time", "s.rowid").Limit(1)

	var res dbScrobbleBuffer
	err := r.queryOne(sql, &res)
	if errors.Is(err, model.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	res.ScrobbleEntry.Participants, err = r.getParticipants(&res.ScrobbleEntry.MediaFile)
	if err != nil {
		return nil, err
	}
	return res.ScrobbleEntry, nil
}

// Dequeue 在成功发送后移除该条记录。
func (r *scrobbleBufferRepository) Dequeue(entry *model.ScrobbleEntry) error {
	return r.delete(Eq{"id": entry.ID})
}

// Length 返回队列中待发送的记录总数。
func (r *scrobbleBufferRepository) Length() (int64, error) {
	return r.count(Select())
}

var _ model.ScrobbleBufferRepository = (*scrobbleBufferRepository)(nil)
