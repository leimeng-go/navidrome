package public

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/navidrome/navidrome/core/artwork"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/req"
)

// handleImages 返回公开封面图。
//
// 请求已取消时直接返回，省去无谓的图片生成开销。
// 单次处理限时 10 秒，防止慢速的封面提取拖住连接。
// 图片按内容寻址不会变更，故设置超长缓存。
func (pub *Router) handleImages(w http.ResponseWriter, r *http.Request) {
	// If context is already canceled, discard request without further processing
	if r.Context().Err() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	p := req.Params(r)
	id, _ := p.String(":id")
	if id == "" {
		log.Warn(r, "No id provided")
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	artId, err := decodeArtworkID(id)
	if err != nil {
		log.Error(r, "Error decoding artwork id", "id", id, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	size := p.IntOr("size", 0)
	square := p.BoolOr("square", false)

	imgReader, lastUpdate, err := pub.artwork.Get(ctx, artId, size, square)
	switch {
	case errors.Is(err, context.Canceled):
		return
	case errors.Is(err, model.ErrNotFound):
		log.Warn(r, "Couldn't find coverArt", "id", id, err)
		http.Error(w, "Artwork not found", http.StatusNotFound)
		return
	case errors.Is(err, artwork.ErrUnavailable):
		log.Debug(r, "Item does not have artwork", "id", id, err)
		http.Error(w, "Artwork not found", http.StatusNotFound)
		return
	case err != nil:
		log.Error(r, "Error retrieving coverArt", "id", id, err)
		http.Error(w, "Error retrieving coverArt", http.StatusInternalServerError)
		return
	}

	defer imgReader.Close()
	w.Header().Set("Cache-Control", "public, max-age=315360000")
	w.Header().Set("Last-Modified", lastUpdate.Format(time.RFC1123))
	cnt, err := io.Copy(w, imgReader)
	if err != nil {
		log.Warn(ctx, "Error sending image", "count", cnt, err)
	}
}
