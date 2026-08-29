package persistence

import (
	"encoding/json"
	"fmt"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/slice"
)

// 参与者数据以两种形式存储：
//   - JSON 列（media_file.participants / album.participants）：
//     便于随实体一次读出用于展示，无需 JOIN
//   - 关联表（media_file_artists / album_artists）：
//     便于按艺人反查作品、做聚合统计
//
// 本文件负责这两种形式的序列化与同步。

// participant 是参与者在 JSON 列中的存储形式。
// 冗余存 Name 以便展示时免于回查 artist 表。
type participant struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	SubRole string `json:"subRole,omitempty"`
}

// flatParticipant represents a flattened participant structure for SQL processing
// flatParticipant 是写入关联表时使用的扁平结构：
// 把「角色 → 艺人列表」的嵌套形式摊平为每行一条记录。
type flatParticipant struct {
	ArtistID string `json:"artist_id"`
	Role     string `json:"role"`
	SubRole  string `json:"sub_role,omitempty"`
}

// marshalParticipants 把参与者序列化为 JSON 列的内容。
func marshalParticipants(participants model.Participants) string {
	dbParticipants := make(map[model.Role][]participant)
	for role, artists := range participants {
		for _, artist := range artists {
			dbParticipants[role] = append(dbParticipants[role], participant{ID: artist.ID, SubRole: artist.SubRole, Name: artist.Name})
		}
	}
	res, _ := json.Marshal(dbParticipants)
	return string(res)
}

// unmarshalParticipants 从 JSON 列解析参与者。
// 只还原 ID 与名字，其余艺人属性需要时再由 getParticipants 补齐。
func unmarshalParticipants(data string) (model.Participants, error) {
	var dbParticipants map[model.Role][]participant
	err := json.Unmarshal([]byte(data), &dbParticipants)
	if err != nil {
		return nil, fmt.Errorf("parsing participants: %w", err)
	}

	participants := make(model.Participants, len(dbParticipants))
	for role, participantList := range dbParticipants {
		artists := slice.Map(participantList, func(p participant) model.Participant {
			return model.Participant{Artist: model.Artist{ID: p.ID, Name: p.Name}, SubRole: p.SubRole}
		})
		participants[role] = artists
	}
	return participants, nil
}

// updateParticipants 同步实体与艺人的关联表。
//
// 采用「先全删再重建」而非增量比对：参与者数量少，
// 全量替换更简单且不会遗留过期关联。
//
// 插入用单条 SQL 完成（json_each 展开 + JOIN artist），
// 而非在 Go 中循环逐条插入，原因有二：
//   - 一次往返即可写入全部，减少 SQLite 的语句开销
//   - INNER JOIN artist 天然过滤掉不存在的艺人 ID，
//     避免外键约束失败使整批写入回滚
//
// ON CONFLICT DO NOTHING 保证重复调用是幂等的。
func (r sqlRepository) updateParticipants(itemID string, participants model.Participants) error {
	// Delete all existing participant entries for this item.
	// This ensures stale role associations are removed when an artist's role changes
	// (e.g., an artist was both albumartist and composer, but is now only composer).
	sqd := Delete(r.tableName + "_artists").Where(Eq{r.tableName + "_id": itemID})
	_, err := r.executeSQL(sqd)
	if err != nil {
		return err
	}
	if len(participants) == 0 {
		return nil
	}

	var flatParticipants []flatParticipant
	for role, artists := range participants {
		for _, artist := range artists {
			flatParticipants = append(flatParticipants, flatParticipant{
				ArtistID: artist.ID,
				Role:     role.String(),
				SubRole:  artist.SubRole,
			})
		}
	}

	participantsJSON, err := json.Marshal(flatParticipants)
	if err != nil {
		return fmt.Errorf("marshaling participants: %w", err)
	}

	// Build the INSERT query using json_each and INNER JOIN to artist table
	// to automatically filter out non-existent artist IDs
	query := fmt.Sprintf(`
		INSERT INTO %[1]s_artists (%[1]s_id, artist_id, role, sub_role)
		SELECT ?, 
		       json_extract(value, '$.artist_id') as artist_id,
		       json_extract(value, '$.role') as role,
		       COALESCE(json_extract(value, '$.sub_role'), '') as sub_role
		-- Parse the flat JSON array: [{"artist_id": "id", "role": "role", "sub_role": "subRole"}]
		FROM json_each(?)                                        -- Iterate through each array element
		-- CRITICAL: Only insert records for artists that actually exist in the database
		JOIN artist ON artist.id = json_extract(value, '$.artist_id')  -- Filter out non-existent artist IDs via INNER JOIN
		-- Handle duplicate insertions gracefully (e.g., if called multiple times)
		ON CONFLICT (artist_id, %[1]s_id, role, sub_role) DO NOTHING   -- Ignore duplicates
	`, r.tableName)

	_, err = r.executeSQL(Expr(query, itemID, string(participantsJSON)))
	return err
}

// getParticipants 用完整的艺人信息填充参与者列表。
//
// JSON 列中只存了 ID 与名字，展示时若需要头像、MBID、排序名等，
// 需回查 artist 表。这里一次性批量查出再按 ID 建索引回填，
// 避免每个参与者单独查询造成 N+1。
func (r *sqlRepository) getParticipants(m *model.MediaFile) (model.Participants, error) {
	ar := NewArtistRepository(r.ctx, r.db)
	ids := m.Participants.AllIDs()
	artists, err := ar.GetAll(model.QueryOptions{Filters: Eq{"artist.id": ids}})
	if err != nil {
		return nil, fmt.Errorf("getting participants: %w", err)
	}
	artistMap := slice.ToMap(artists, func(a model.Artist) (string, model.Artist) {
		return a.ID, a
	})
	p := m.Participants
	for role, artistList := range p {
		for idx, artist := range artistList {
			if a, ok := artistMap[artist.ID]; ok {
				p[role][idx].Artist = a
			}
		}
	}
	return p, nil
}
