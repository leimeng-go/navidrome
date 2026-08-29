package persistence

import (
	"context"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
	"github.com/pocketbase/dbx"
)

// scrobbleRepository 是本地播放历史仓储，只追加不修改。
// 与 scrobble_buffer 不同，这里记录的是已发生的播放事件本身，
// 而非待上报到外部服务的队列。
type scrobbleRepository struct {
	sqlRepository
}

// NewScrobbleRepository 创建播放历史仓储。
func NewScrobbleRepository(ctx context.Context, db dbx.Builder) model.ScrobbleRepository {
	r := &scrobbleRepository{}
	r.ctx = ctx
	r.db = db
	r.tableName = "scrobbles"
	return r
}

// RecordScrobble 记录一次播放。时间以 Unix 秒存储，便于按区间聚合统计。
func (r *scrobbleRepository) RecordScrobble(mediaFileID string, submissionTime time.Time) error {
	userID := loggedUser(r.ctx).ID
	values := map[string]interface{}{
		"media_file_id":   mediaFileID,
		"user_id":         userID,
		"submission_time": submissionTime.Unix(),
	}
	insert := Insert(r.tableName).SetMap(values)
	_, err := r.executeSQL(insert)
	return err
}
