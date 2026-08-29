package subsonic

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/deluan/sanitize"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/server/public"
	"github.com/navidrome/navidrome/server/subsonic/responses"
	"github.com/navidrome/navidrome/utils/req"
	"github.com/navidrome/navidrome/utils/slice"
	"golang.org/x/sync/errgroup"
)

// searchParams 汇总三类实体各自的分页参数。
type searchParams struct {
	query        string
	artistCount  int
	artistOffset int
	albumCount   int
	albumOffset  int
	songCount    int
	songOffset   int
}

// getSearchParams 解析搜索参数。
// query 缺省为一对引号，表示「匹配全部」，这是 Subsonic 客户端惯用的取全量写法。
func (api *Router) getSearchParams(r *http.Request) (*searchParams, error) {
	p := req.Params(r)
	sp := &searchParams{}
	sp.query = p.StringOr("query", `""`)
	sp.artistCount = p.IntOr("artistCount", 20)
	sp.artistOffset = p.IntOr("artistOffset", 0)
	sp.albumCount = p.IntOr("albumCount", 20)
	sp.albumOffset = p.IntOr("albumOffset", 0)
	sp.songCount = p.IntOr("songCount", 20)
	sp.songOffset = p.IntOr("songOffset", 0)
	return sp, nil
}

// searchFunc 抽象各仓库的搜索方法签名，便于泛型复用。
type searchFunc[T any] func(q string, offset int, size int, options ...model.QueryOptions) (T, error)

// callSearch 把一次搜索包装成可并行执行的任务。
// 单类搜索失败不向上返回错误，避免一类结果出错就让整个搜索失败。
func callSearch[T any](ctx context.Context, s searchFunc[T], q string, offset, size int, result *T, options ...model.QueryOptions) func() error {
	return func() error {
		if size == 0 {
			return nil
		}
		typ := strings.TrimPrefix(reflect.TypeOf(*result).String(), "model.")
		var err error
		start := time.Now()
		*result, err = s(q, offset, size, options...)
		if err != nil {
			log.Error(ctx, "Error searching "+typ, "query", q, "elapsed", time.Since(start), err)
		} else {
			log.Trace(ctx, "Search for "+typ+" completed", "query", q, "elapsed", time.Since(start))
		}
		return nil
	}
}

// searchAll 并行搜索曲目、专辑与艺人。
//
// 查询串先去掉尾部通配符再转小写并去重音，与索引侧的规范化方式保持一致。
// 艺人的库过滤需走 library_artist 关联表，与专辑/曲目的直接 library_id 过滤不同。
func (api *Router) searchAll(ctx context.Context, sp *searchParams, musicFolderIds []int) (mediaFiles model.MediaFiles, albums model.Albums, artists model.Artists) {
	start := time.Now()
	q := sanitize.Accents(strings.ToLower(strings.TrimSuffix(sp.query, "*")))

	// Create query options for library filtering
	var options []model.QueryOptions
	var artistOptions []model.QueryOptions
	if len(musicFolderIds) > 0 {
		// For MediaFiles and Albums, use direct library_id filter
		options = append(options, model.QueryOptions{
			Filters: Eq{"library_id": musicFolderIds},
		})
		// For Artists, use the repository's built-in library filtering mechanism
		// which properly handles the library_artist table joins
		// TODO Revisit library filtering in sql_base_repository.go
		artistOptions = append(artistOptions, model.QueryOptions{
			Filters: Eq{"library_artist.library_id": musicFolderIds},
		})
	}

	// Run searches in parallel
	g, ctx := errgroup.WithContext(ctx)
	g.Go(callSearch(ctx, api.ds.MediaFile(ctx).Search, q, sp.songOffset, sp.songCount, &mediaFiles, options...))
	g.Go(callSearch(ctx, api.ds.Album(ctx).Search, q, sp.albumOffset, sp.albumCount, &albums, options...))
	g.Go(callSearch(ctx, api.ds.Artist(ctx).Search, q, sp.artistOffset, sp.artistCount, &artists, artistOptions...))
	err := g.Wait()
	if err == nil {
		log.Debug(ctx, fmt.Sprintf("Search resulted in %d songs, %d albums and %d artists",
			len(mediaFiles), len(albums), len(artists)), "query", sp.query, "elapsedTime", time.Since(start))
	} else {
		log.Warn(ctx, "Search was interrupted", "query", sp.query, "elapsedTime", time.Since(start), err)
	}
	return mediaFiles, albums, artists
}

// Search2 搜索（旧版结构）。
func (api *Router) Search2(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	sp, err := api.getSearchParams(r)
	if err != nil {
		return nil, err
	}

	// Get optional library IDs from musicFolderId parameter
	musicFolderIds, err := selectedMusicFolderIds(r, false)
	if err != nil {
		return nil, err
	}
	mfs, als, as := api.searchAll(ctx, sp, musicFolderIds)

	response := newResponse()
	searchResult2 := &responses.SearchResult2{}
	searchResult2.Artist = slice.Map(as, func(artist model.Artist) responses.Artist {
		a := responses.Artist{
			Id:             artist.ID,
			Name:           artist.Name,
			UserRating:     int32(artist.Rating),
			CoverArt:       artist.CoverArtID().String(),
			ArtistImageUrl: public.ImageURL(r, artist.CoverArtID(), 600),
		}
		if artist.Starred {
			a.Starred = artist.StarredAt
		}
		return a
	})
	searchResult2.Album = slice.MapWithArg(als, ctx, childFromAlbum)
	searchResult2.Song = slice.MapWithArg(mfs, ctx, childFromMediaFile)
	response.SearchResult2 = searchResult2
	return response, nil
}

// Search3 搜索（ID3 结构）。
func (api *Router) Search3(r *http.Request) (*responses.Subsonic, error) {
	ctx := r.Context()
	sp, err := api.getSearchParams(r)
	if err != nil {
		return nil, err
	}

	// Get optional library IDs from musicFolderId parameter
	musicFolderIds, err := selectedMusicFolderIds(r, false)
	if err != nil {
		return nil, err
	}
	mfs, als, as := api.searchAll(ctx, sp, musicFolderIds)

	response := newResponse()
	searchResult3 := &responses.SearchResult3{}
	searchResult3.Artist = slice.MapWithArg(as, r, toArtistID3)
	searchResult3.Album = slice.MapWithArg(als, ctx, buildAlbumID3)
	searchResult3.Song = slice.MapWithArg(mfs, ctx, childFromMediaFile)
	response.SearchResult3 = searchResult3
	return response, nil
}
