package nativeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/slice"
)

// updateQueuePayload 是队列更新请求体。
// 字段均为指针：需区分「未提供」与「显式置零」，以支持部分更新。
type updateQueuePayload struct {
	Ids      *[]string `json:"ids,omitempty"`
	Current  *int      `json:"current,omitempty"`
	Position *int64    `json:"position,omitempty"`
}

// validateCurrentIndex validates that the current index is within bounds of the items array.
// Returns false if validation fails (and sends error response), true if validation passes.
func validateCurrentIndex(w http.ResponseWriter, current int, itemsLength int) bool {
	if current < 0 || current >= itemsLength {
		http.Error(w, "current index out of bounds", http.StatusBadRequest)
		return false
	}
	return true
}

// retrieveExistingQueue retrieves an existing play queue for a user with proper error handling.
// Returns the queue (nil if not found) and false if an error occurred and response was sent.
func retrieveExistingQueue(ctx context.Context, w http.ResponseWriter, ds model.DataStore, userID string) (*model.PlayQueue, bool) {
	existing, err := ds.PlayQueue(ctx).Retrieve(userID)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		log.Error(ctx, "Error retrieving queue", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return existing, true
}

// decodeUpdatePayload decodes the JSON payload from the request body.
// Returns false if decoding fails (and sends error response), true if successful.
func decodeUpdatePayload(w http.ResponseWriter, r *http.Request) (*updateQueuePayload, bool) {
	var payload updateQueuePayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return nil, false
	}
	return &payload, true
}

// createMediaFileItems converts a slice of IDs to MediaFile items.
// createMediaFileItems 把 ID 列表转成只含 ID 的媒体文件，仅用于持久化引用关系。
func createMediaFileItems(ids []string) []model.MediaFile {
	return slice.Map(ids, func(id string) model.MediaFile {
		return model.MediaFile{ID: id}
	})
}

// extractUserAndClient extracts user and client from the request context.
func extractUserAndClient(ctx context.Context) (model.User, string) {
	user, _ := request.UserFrom(ctx)
	client, _ := request.ClientFrom(ctx)
	return user, client
}

// getQueue 返回当前用户的播放队列。队列不存在时返回空对象而非 404。
func getQueue(ds model.DataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		user, _ := request.UserFrom(ctx)
		repo := ds.PlayQueue(ctx)
		pq, err := repo.RetrieveWithMediaFiles(user.ID)
		if err != nil && !errors.Is(err, model.ErrNotFound) {
			log.Error(ctx, "Error retrieving queue", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if pq == nil {
			pq = &model.PlayQueue{}
		}
		resp, err := json.Marshal(pq)
		if err != nil {
			log.Error(ctx, "Error marshalling queue", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(resp)
	}
}

// saveQueue 整体覆盖播放队列。
func saveQueue(ds model.DataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		payload, ok := decodeUpdatePayload(w, r)
		if !ok {
			return
		}
		user, client := extractUserAndClient(ctx)
		ids := V(payload.Ids)
		items := createMediaFileItems(ids)
		current := V(payload.Current)
		if len(ids) > 0 && !validateCurrentIndex(w, current, len(ids)) {
			return
		}
		pq := &model.PlayQueue{
			UserID:    user.ID,
			Current:   current,
			Position:  max(V(payload.Position), 0),
			ChangedBy: client,
			Items:     items,
		}
		if err := ds.PlayQueue(ctx).Store(pq); err != nil {
			log.Error(ctx, "Error saving queue", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// updateQueue 部分更新播放队列，只写入请求中出现的字段。
//
// 当前曲目下标必须与曲目列表匹配，故需分情况校验：
// 同时更新两者时用新列表校验；只更新其一时，另一半从数据库取现值校验。
// 这样可避免出现指向不存在曲目的下标。
func updateQueue(ds model.DataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Decode and validate the JSON payload
		payload, ok := decodeUpdatePayload(w, r)
		if !ok {
			return
		}

		// Extract user and client information from request context
		user, client := extractUserAndClient(ctx)

		// Initialize play queue with user ID and client info
		pq := &model.PlayQueue{UserID: user.ID, ChangedBy: client}
		var cols []string // Track which columns to update in the database

		// Handle queue items update
		if payload.Ids != nil {
			pq.Items = createMediaFileItems(*payload.Ids)
			cols = append(cols, "items")

			// If current index is not being updated, validate existing current index
			// against the new items list to ensure it remains valid
			if payload.Current == nil {
				existing, ok := retrieveExistingQueue(ctx, w, ds, user.ID)
				if !ok {
					return
				}
				if existing != nil && !validateCurrentIndex(w, existing.Current, len(*payload.Ids)) {
					return
				}
			}
		}

		// Handle current track index update
		if payload.Current != nil {
			pq.Current = *payload.Current
			cols = append(cols, "current")

			if payload.Ids != nil {
				// If items are also being updated, validate current index against new items
				if !validateCurrentIndex(w, *payload.Current, len(*payload.Ids)) {
					return
				}
			} else {
				// If only current index is being updated, validate against existing items
				existing, ok := retrieveExistingQueue(ctx, w, ds, user.ID)
				if !ok {
					return
				}
				if existing != nil && !validateCurrentIndex(w, *payload.Current, len(existing.Items)) {
					return
				}
			}
		}

		// Handle playback position update
		if payload.Position != nil {
			pq.Position = max(*payload.Position, 0) // Ensure position is non-negative
			cols = append(cols, "position")
		}

		// If no fields were specified for update, return success without doing anything
		if len(cols) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Perform partial update of the specified columns only
		if err := ds.PlayQueue(ctx).Store(pq, cols...); err != nil {
			log.Error(ctx, "Error updating queue", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// clearQueue 清空当前用户的播放队列。
func clearQueue(ds model.DataStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		user, _ := request.UserFrom(ctx)
		if err := ds.PlayQueue(ctx).Clear(user.ID); err != nil {
			log.Error(ctx, "Error clearing queue", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
