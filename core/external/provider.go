package external

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/agents"
	_ "github.com/navidrome/navidrome/core/agents/deezer"
	_ "github.com/navidrome/navidrome/core/agents/lastfm"
	_ "github.com/navidrome/navidrome/core/agents/listenbrainz"
	_ "github.com/navidrome/navidrome/core/agents/spotify"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils"
	. "github.com/navidrome/navidrome/utils/gg"
	"github.com/navidrome/navidrome/utils/random"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/navidrome/navidrome/utils/str"
	"golang.org/x/sync/errgroup"
)

const (
	// maxSimilarArtists 是入库保存的相似艺术家上限
	maxSimilarArtists = 100
	// refreshDelay 是后台刷新两次请求之间的间隔，用于给外部服务限速
	refreshDelay = 5 * time.Second
	// refreshTimeout 是单次后台刷新的超时时间
	refreshTimeout = 15 * time.Second
	// refreshQueueLength 是后台刷新队列容量，满了就丢弃新请求
	refreshQueueLength = 2000
)

// Provider 是外部元数据的统一入口，负责调用代理、缓存结果并与本地库匹配。
type Provider interface {
	UpdateAlbumInfo(ctx context.Context, id string) (*model.Album, error)
	UpdateArtistInfo(ctx context.Context, id string, count int, includeNotPresent bool) (*model.Artist, error)
	ArtistRadio(ctx context.Context, id string, count int) (model.MediaFiles, error)
	TopSongs(ctx context.Context, artist string, count int) (model.MediaFiles, error)
	ArtistImage(ctx context.Context, id string) (*url.URL, error)
	AlbumImage(ctx context.Context, id string) (*url.URL, error)
}

// provider 是 Provider 的实现，内含两个后台刷新队列用于异步更新过期信息。
type provider struct {
	ds          model.DataStore
	ag          Agents
	artistQueue refreshQueue[auxArtist]
	albumQueue  refreshQueue[auxAlbum]
}

// auxAlbum 包装 model.Album，覆写 Name 以适配外部 API 调用。
type auxAlbum struct {
	model.Album
}

// Name returns the appropriate album name for external API calls
// based on the DevPreserveUnicodeInExternalCalls configuration option
//
// Name 返回用于外部查询的专辑名。
// 默认清洗掉特殊 Unicode 字符——外部服务往往无法匹配带特殊符号的名称。
func (a *auxAlbum) Name() string {
	if conf.Server.DevPreserveUnicodeInExternalCalls {
		return a.Album.Name
	}
	return str.Clear(a.Album.Name)
}

// auxArtist 包装 model.Artist，覆写 Name 以适配外部 API 调用。
type auxArtist struct {
	model.Artist
}

// Name returns the appropriate artist name for external API calls
// based on the DevPreserveUnicodeInExternalCalls configuration option
//
// Name 返回用于外部查询的艺术家名，规则同 auxAlbum.Name。
func (a *auxArtist) Name() string {
	if conf.Server.DevPreserveUnicodeInExternalCalls {
		return a.Artist.Name
	}
	return str.Clear(a.Artist.Name)
}

// Agents 汇总 provider 所需的全部代理能力。
type Agents interface {
	agents.AlbumInfoRetriever
	agents.AlbumImageRetriever
	agents.ArtistBiographyRetriever
	agents.ArtistMBIDRetriever
	agents.ArtistImageRetriever
	agents.ArtistSimilarRetriever
	agents.ArtistTopSongsRetriever
	agents.ArtistURLRetriever
}

// NewProvider 创建外部元数据服务，并启动两个后台刷新队列。
func NewProvider(ds model.DataStore, agents Agents) Provider {
	e := &provider{ds: ds, ag: agents}
	e.artistQueue = newRefreshQueue(context.TODO(), e.populateArtistInfo)
	e.albumQueue = newRefreshQueue(context.TODO(), e.populateAlbumInfo)
	return e
}

// getAlbum 按 ID 取专辑，传入曲目 ID 时自动上溯到其所属专辑。
func (e *provider) getAlbum(ctx context.Context, id string) (auxAlbum, error) {
	var entity interface{}
	entity, err := model.GetEntityByID(ctx, e.ds, id)
	if err != nil {
		return auxAlbum{}, err
	}

	var album auxAlbum
	switch v := entity.(type) {
	case *model.Album:
		album.Album = *v
	case *model.MediaFile:
		return e.getAlbum(ctx, v.AlbumID)
	default:
		return auxAlbum{}, model.ErrNotFound
	}

	return album, nil
}

// UpdateAlbumInfo 返回专辑的外部信息。
//
// 首次访问同步拉取（否则用户会看到空白）；
// 已有但过期时先返回旧数据，再入队后台刷新——
// 陈旧数据也远好过让用户干等外部服务响应。
func (e *provider) UpdateAlbumInfo(ctx context.Context, id string) (*model.Album, error) {
	album, err := e.getAlbum(ctx, id)
	if err != nil {
		log.Info(ctx, "Not found", "id", id)
		return nil, err
	}

	updatedAt := V(album.ExternalInfoUpdatedAt)
	albumName := album.Name()
	if updatedAt.IsZero() {
		log.Debug(ctx, "AlbumInfo not cached. Retrieving it now", "updatedAt", updatedAt, "id", id, "name", albumName)
		album, err = e.populateAlbumInfo(ctx, album)
		if err != nil {
			return nil, err
		}
	}

	// If info is expired, trigger a populateAlbumInfo in the background
	if time.Since(updatedAt) > conf.Server.DevAlbumInfoTimeToLive {
		log.Debug("Found expired cached AlbumInfo, refreshing in the background", "updatedAt", album.ExternalInfoUpdatedAt, "name", albumName)
		e.albumQueue.enqueue(&album)
	}

	return &album.Album, nil
}

// populateAlbumInfo 拉取专辑外部信息并落库。
// 图片按尺寸降序取前三张，分别作为大/中/小图。
// 代理返回「未找到」不算错误：说明外部确实没有这张专辑。
func (e *provider) populateAlbumInfo(ctx context.Context, album auxAlbum) (auxAlbum, error) {
	start := time.Now()
	albumName := album.Name()
	info, err := e.ag.GetAlbumInfo(ctx, albumName, album.AlbumArtist, album.MbzAlbumID)
	if errors.Is(err, agents.ErrNotFound) {
		return album, nil
	}
	if err != nil {
		log.Error("Error refreshing AlbumInfo", "id", album.ID, "name", albumName, "artist", album.AlbumArtist,
			"elapsed", time.Since(start), err)
		return album, err
	}

	album.ExternalInfoUpdatedAt = P(time.Now())
	album.ExternalUrl = info.URL

	if info.Description != "" {
		album.Description = info.Description
	}

	images, err := e.ag.GetAlbumImages(ctx, albumName, album.AlbumArtist, album.MbzAlbumID)
	if err == nil && len(images) > 0 {
		sort.Slice(images, func(i, j int) bool {
			return images[i].Size > images[j].Size
		})

		album.LargeImageUrl = images[0].URL

		if len(images) >= 2 {
			album.MediumImageUrl = images[1].URL
		}

		if len(images) >= 3 {
			album.SmallImageUrl = images[2].URL
		}
	}

	err = e.ds.Album(ctx).UpdateExternalInfo(&album.Album)
	if err != nil {
		log.Error(ctx, "Error trying to update album external information", "id", album.ID, "name", albumName,
			"elapsed", time.Since(start), err)
	} else {
		log.Trace(ctx, "AlbumInfo collected", "album", album, "elapsed", time.Since(start))
	}

	return album, nil
}

// getArtist 按 ID 取艺术家，传入曲目或专辑 ID 时自动上溯。
func (e *provider) getArtist(ctx context.Context, id string) (auxArtist, error) {
	var entity interface{}
	entity, err := model.GetEntityByID(ctx, e.ds, id)
	if err != nil {
		return auxArtist{}, err
	}

	var artist auxArtist
	switch v := entity.(type) {
	case *model.Artist:
		artist.Artist = *v
	case *model.MediaFile:
		return e.getArtist(ctx, v.ArtistID)
	case *model.Album:
		return e.getArtist(ctx, v.AlbumArtistID)
	default:
		return auxArtist{}, model.ErrNotFound
	}
	return artist, nil
}

// UpdateArtistInfo 返回艺术家外部信息，并按需加载相似艺术家。
func (e *provider) UpdateArtistInfo(ctx context.Context, id string, similarCount int, includeNotPresent bool) (*model.Artist, error) {
	artist, err := e.refreshArtistInfo(ctx, id)
	if err != nil {
		return nil, err
	}

	err = e.loadSimilar(ctx, &artist, similarCount, includeNotPresent)
	return &artist.Artist, err
}

// refreshArtistInfo 取艺术家信息，缓存策略同 UpdateAlbumInfo：
// 无数据同步拉取，过期则后台异步刷新。
func (e *provider) refreshArtistInfo(ctx context.Context, id string) (auxArtist, error) {
	artist, err := e.getArtist(ctx, id)
	if err != nil {
		return auxArtist{}, err
	}

	// If we don't have any info, retrieves it now
	updatedAt := V(artist.ExternalInfoUpdatedAt)
	artistName := artist.Name()
	if updatedAt.IsZero() {
		log.Debug(ctx, "ArtistInfo not cached. Retrieving it now", "updatedAt", updatedAt, "id", id, "name", artistName)
		artist, err = e.populateArtistInfo(ctx, artist)
		if err != nil {
			return auxArtist{}, err
		}
	}

	// If info is expired, trigger a populateArtistInfo in the background
	if time.Since(updatedAt) > conf.Server.DevArtistInfoTimeToLive {
		log.Debug("Found expired cached ArtistInfo, refreshing in the background", "updatedAt", updatedAt, "name", artistName)
		e.artistQueue.enqueue(&artist)
	}
	return artist, nil
}

// populateArtistInfo 并发拉取艺术家的图片、简介、链接与相似艺术家。
//
// 先补 MBID：后续各项查询有了 MBID 才能精确匹配，避免同名艺术家混淆。
// 四项查询并发但限流 2 路，防止对同一外部服务并发过高触发限流。
// 各子任务一律返回 nil：单项失败不应影响其余项落库。
func (e *provider) populateArtistInfo(ctx context.Context, artist auxArtist) (auxArtist, error) {
	start := time.Now()
	// Get MBID first, if it is not yet available
	artistName := artist.Name()
	if artist.MbzArtistID == "" {
		mbid, err := e.ag.GetArtistMBID(ctx, artist.ID, artistName)
		if mbid != "" && err == nil {
			artist.MbzArtistID = mbid
		}
	}

	// Call all registered agents and collect information
	g := errgroup.Group{}
	g.SetLimit(2)
	g.Go(func() error { e.callGetImage(ctx, e.ag, &artist); return nil })
	g.Go(func() error { e.callGetBiography(ctx, e.ag, &artist); return nil })
	g.Go(func() error { e.callGetURL(ctx, e.ag, &artist); return nil })
	g.Go(func() error { e.callGetSimilar(ctx, e.ag, &artist, maxSimilarArtists, true); return nil })
	_ = g.Wait()

	if utils.IsCtxDone(ctx) {
		log.Warn(ctx, "ArtistInfo update canceled", "id", artist.ID, "name", artistName, "elapsed", time.Since(start), ctx.Err())
		return artist, ctx.Err()
	}

	artist.ExternalInfoUpdatedAt = P(time.Now())
	err := e.ds.Artist(ctx).UpdateExternalInfo(&artist.Artist)
	if err != nil {
		log.Error(ctx, "Error trying to update artist external information", "id", artist.ID, "name", artistName,
			"elapsed", time.Since(start), err)
	} else {
		log.Trace(ctx, "ArtistInfo collected", "artist", artist, "elapsed", time.Since(start))
	}
	return artist, nil
}

// ArtistRadio 生成「艺术家电台」：以该艺术家及其相似艺术家的热门曲目组成随机歌单。
//
// 用加权随机而非纯随机，使结果既有倾向性又不完全固定：
// 本尊权重高于相似艺术家（+10），同一艺术家内按热度递减（每首减 4）。
// 因是带权重的有放回抽取，可能重复抽到同一首。
func (e *provider) ArtistRadio(ctx context.Context, id string, count int) (model.MediaFiles, error) {
	artist, err := e.getArtist(ctx, id)
	if err != nil {
		return nil, err
	}

	e.callGetSimilar(ctx, e.ag, &artist, 15, false)
	if utils.IsCtxDone(ctx) {
		log.Warn(ctx, "ArtistRadio call canceled", ctx.Err())
		return nil, ctx.Err()
	}

	weightedSongs := random.NewWeightedChooser[model.MediaFile]()
	addArtist := func(a model.Artist, weightedSongs *random.WeightedChooser[model.MediaFile], count, artistWeight int) error {
		if utils.IsCtxDone(ctx) {
			log.Warn(ctx, "ArtistRadio call canceled", ctx.Err())
			return ctx.Err()
		}

		topCount := max(count, 20)
		topSongs, err := e.getMatchingTopSongs(ctx, e.ag, &auxArtist{Artist: a}, topCount)
		if err != nil {
			log.Warn(ctx, "Error getting artist's top songs", "artist", a.Name, err)
			return nil
		}

		weight := topCount * (4 + artistWeight)
		for _, mf := range topSongs {
			weightedSongs.Add(mf, weight)
			weight -= 4
		}
		return nil
	}

	err = addArtist(artist.Artist, weightedSongs, count, 10)
	if err != nil {
		return nil, err
	}
	for _, a := range artist.SimilarArtists {
		err := addArtist(a, weightedSongs, count, 0)
		if err != nil {
			return nil, err
		}
	}

	var similarSongs model.MediaFiles
	for len(similarSongs) < count && weightedSongs.Size() > 0 {
		s, err := weightedSongs.Pick()
		if err != nil {
			log.Warn(ctx, "Error getting weighted song", err)
			continue
		}
		similarSongs = append(similarSongs, s)
	}

	return similarSongs, nil
}

// ArtistImage 返回艺术家图片链接（优先大图）。
func (e *provider) ArtistImage(ctx context.Context, id string) (*url.URL, error) {
	artist, err := e.getArtist(ctx, id)
	if err != nil {
		return nil, err
	}

	e.callGetImage(ctx, e.ag, &artist)
	if utils.IsCtxDone(ctx) {
		log.Warn(ctx, "ArtistImage call canceled", ctx.Err())
		return nil, ctx.Err()
	}

	imageUrl := artist.ArtistImageUrl()
	if imageUrl == "" {
		return nil, model.ErrNotFound
	}
	return url.Parse(imageUrl)
}

// AlbumImage 返回专辑封面链接，取尺寸最大的一张。
// 错误按类型分级记录：未找到属正常，取消不告警，其余才是真异常。
func (e *provider) AlbumImage(ctx context.Context, id string) (*url.URL, error) {
	album, err := e.getAlbum(ctx, id)
	if err != nil {
		return nil, err
	}

	albumName := album.Name()
	images, err := e.ag.GetAlbumImages(ctx, albumName, album.AlbumArtist, album.MbzAlbumID)
	if err != nil {
		switch {
		case errors.Is(err, agents.ErrNotFound):
			log.Trace(ctx, "Album not found in agent", "albumID", id, "name", albumName, "artist", album.AlbumArtist)
			return nil, model.ErrNotFound
		case errors.Is(err, context.Canceled):
			log.Debug(ctx, "GetAlbumImages call canceled", err)
		default:
			log.Warn(ctx, "Error getting album images from agent", "albumID", id, "name", albumName, "artist", album.AlbumArtist, err)
		}
		return nil, err
	}

	if len(images) == 0 {
		log.Warn(ctx, "Agent returned no images without error", "albumID", id, "name", albumName, "artist", album.AlbumArtist)
		return nil, model.ErrNotFound
	}

	// Return the biggest image
	var img agents.ExternalImage
	for _, i := range images {
		if img.Size <= i.Size {
			img = i
		}
	}
	if img.URL == "" {
		return nil, model.ErrNotFound
	}
	return url.Parse(img.URL)
}

// TopSongs 按艺术家名查询其热门单曲（仅返回本地库中存在的）。
// 找不到艺术家时返回空而非错误——这属于正常的「无结果」。
func (e *provider) TopSongs(ctx context.Context, artistName string, count int) (model.MediaFiles, error) {
	artist, err := e.findArtistByName(ctx, artistName)
	if err != nil {
		log.Error(ctx, "Artist not found", "name", artistName, err)
		return nil, nil
	}

	songs, err := e.getMatchingTopSongs(ctx, e.ag, artist, count)
	if err != nil {
		switch {
		case errors.Is(err, agents.ErrNotFound):
			log.Trace(ctx, "TopSongs not found", "name", artistName)
			return nil, model.ErrNotFound
		case errors.Is(err, context.Canceled):
			log.Debug(ctx, "TopSongs call canceled", err)
		default:
			log.Warn(ctx, "Error getting top songs from agent", "artist", artistName, err)
		}

		return nil, err
	}
	return songs, nil
}

// getMatchingTopSongs 把外部返回的热门单曲匹配到本地库中的曲目。
//
// 两级匹配：MBID 精确匹配优先（可靠），标题匹配兜底（外部常无 MBID）。
// 两次批量查询而非逐首查，避免 N 次数据库往返。
func (e *provider) getMatchingTopSongs(ctx context.Context, agent agents.ArtistTopSongsRetriever, artist *auxArtist, count int) (model.MediaFiles, error) {
	artistName := artist.Name()
	songs, err := agent.GetArtistTopSongs(ctx, artist.ID, artistName, artist.MbzArtistID, count)
	if err != nil {
		return nil, fmt.Errorf("failed to get top songs for artist %s: %w", artistName, err)
	}

	mbidMatches, err := e.loadTracksByMBID(ctx, songs)
	if err != nil {
		return nil, fmt.Errorf("failed to load tracks by MBID: %w", err)
	}
	titleMatches, err := e.loadTracksByTitle(ctx, songs, artist, mbidMatches)
	if err != nil {
		return nil, fmt.Errorf("failed to load tracks by title: %w", err)
	}

	log.Trace(ctx, "Top Songs loaded", "name", artistName, "numSongs", len(songs), "numMBIDMatches", len(mbidMatches), "numTitleMatches", len(titleMatches))
	mfs := e.selectTopSongs(songs, mbidMatches, titleMatches, count)

	if len(mfs) == 0 {
		log.Debug(ctx, "No matching top songs found", "name", artistName)
	} else {
		log.Debug(ctx, "Found matching top songs", "name", artistName, "numSongs", len(mfs))
	}

	return mfs, nil
}

// loadTracksByMBID 按 MBID 批量匹配本地曲目，返回 MBID 到曲目的映射。
// 同一 MBID 有多条时只留第一条（同一录音的不同副本）。
func (e *provider) loadTracksByMBID(ctx context.Context, songs []agents.Song) (map[string]model.MediaFile, error) {
	var mbids []string
	for _, s := range songs {
		if s.MBID != "" {
			mbids = append(mbids, s.MBID)
		}
	}
	matches := map[string]model.MediaFile{}
	if len(mbids) == 0 {
		return matches, nil
	}
	res, err := e.ds.MediaFile(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.And{
			squirrel.Eq{"mbz_recording_id": mbids},
			squirrel.Eq{"missing": false},
		},
	})
	if err != nil {
		return matches, err
	}
	for _, mf := range res {
		if id := mf.MbzRecordingID; id != "" {
			if _, ok := matches[id]; !ok {
				matches[id] = mf
			}
		}
	}
	return matches, nil
}

// loadTracksByTitle 按标题匹配本地曲目，跳过已由 MBID 命中的。
//
// 标题经排序归一化后比对，消除大小写、冠词、标点的差异。
// 结果按收藏、评分、年份、非合辑排序，使同名曲目优先选中更「正统」的版本。
func (e *provider) loadTracksByTitle(ctx context.Context, songs []agents.Song, artist *auxArtist, mbidMatches map[string]model.MediaFile) (map[string]model.MediaFile, error) {
	titleMap := map[string]string{}
	for _, s := range songs {
		if s.MBID != "" && mbidMatches[s.MBID].ID != "" {
			continue
		}
		sanitized := str.SanitizeFieldForSorting(s.Name)
		titleMap[sanitized] = s.Name
	}
	matches := map[string]model.MediaFile{}
	if len(titleMap) == 0 {
		return matches, nil
	}
	titleFilters := squirrel.Or{}
	for sanitized := range titleMap {
		titleFilters = append(titleFilters, squirrel.Like{"order_title": sanitized})
	}

	res, err := e.ds.MediaFile(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.And{
			squirrel.Or{
				squirrel.Eq{"artist_id": artist.ID},
				squirrel.Eq{"album_artist_id": artist.ID},
			},
			titleFilters,
			squirrel.Eq{"missing": false},
		},
		Sort: "starred desc, rating desc, year asc, compilation asc ",
	})
	if err != nil {
		return matches, err
	}
	for _, mf := range res {
		sanitized := str.SanitizeFieldForSorting(mf.Title)
		if _, ok := matches[sanitized]; !ok {
			matches[sanitized] = mf
		}
	}
	return matches, nil
}

// selectTopSongs 按外部返回的热度顺序回填本地曲目，优先用 MBID 匹配结果。
func (e *provider) selectTopSongs(songs []agents.Song, byMBID, byTitle map[string]model.MediaFile, count int) model.MediaFiles {
	var mfs model.MediaFiles
	for _, t := range songs {
		if len(mfs) == count {
			break
		}
		if t.MBID != "" {
			if mf, ok := byMBID[t.MBID]; ok {
				mfs = append(mfs, mf)
				continue
			}
		}
		if mf, ok := byTitle[str.SanitizeFieldForSorting(t.Name)]; ok {
			mfs = append(mfs, mf)
		}
	}
	return mfs
}

// callGetURL 取艺术家外部主页链接，失败静默跳过。
func (e *provider) callGetURL(ctx context.Context, agent agents.ArtistURLRetriever, artist *auxArtist) {
	artisURL, err := agent.GetArtistURL(ctx, artist.ID, artist.Name(), artist.MbzArtistID)
	if err != nil {
		return
	}
	artist.ExternalUrl = artisURL
}

// callGetBiography 取艺术家简介并清洗：
// 过滤危险 HTML、去掉换行（前端单段展示），
// 并给链接补上 target='_blank'，避免点击后跳离应用。
func (e *provider) callGetBiography(ctx context.Context, agent agents.ArtistBiographyRetriever, artist *auxArtist) {
	bio, err := agent.GetArtistBiography(ctx, artist.ID, artist.Name(), artist.MbzArtistID)
	if err != nil {
		return
	}
	bio = str.SanitizeText(bio)
	bio = strings.ReplaceAll(bio, "\n", " ")
	artist.Biography = strings.ReplaceAll(bio, "<a ", "<a target='_blank' ")
}

// callGetImage 取艺术家图片，按尺寸降序分配为大/中/小图。
func (e *provider) callGetImage(ctx context.Context, agent agents.ArtistImageRetriever, artist *auxArtist) {
	images, err := agent.GetArtistImages(ctx, artist.ID, artist.Name(), artist.MbzArtistID)
	if err != nil {
		return
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Size > images[j].Size })

	if len(images) >= 1 {
		artist.LargeImageUrl = images[0].URL
	}
	if len(images) >= 2 {
		artist.MediumImageUrl = images[1].URL
	}
	if len(images) >= 3 {
		artist.SmallImageUrl = images[2].URL
	}
}

// callGetSimilar 取相似艺术家并映射到本地库。
func (e *provider) callGetSimilar(ctx context.Context, agent agents.ArtistSimilarRetriever, artist *auxArtist,
	limit int, includeNotPresent bool) {
	artistName := artist.Name()
	similar, err := agent.GetSimilarArtists(ctx, artist.ID, artistName, artist.MbzArtistID, limit)
	if len(similar) == 0 || err != nil {
		return
	}
	start := time.Now()
	sa, err := e.mapSimilarArtists(ctx, similar, limit, includeNotPresent)
	log.Debug(ctx, "Mapped Similar Artists", "artist", artistName, "numSimilar", len(sa), "elapsed", time.Since(start))
	if err != nil {
		return
	}
	artist.SimilarArtists = sa
}

// mapSimilarArtists 把外部返回的艺术家名匹配到本地库。
//
// 一次批量查询后在内存中比对，避免逐个查库。
// 本地存在的优先填充；配额未满且允许时，再补上仅有名字的「不在库中」条目
// （前端可展示但不可点击）。
func (e *provider) mapSimilarArtists(ctx context.Context, similar []agents.Artist, limit int, includeNotPresent bool) (model.Artists, error) {
	var result model.Artists
	var notPresent []string

	artistNames := slice.Map(similar, func(artist agents.Artist) string { return artist.Name })

	// Query all artists at once
	clauses := slice.Map(artistNames, func(name string) squirrel.Sqlizer {
		return squirrel.Like{"artist.name": name}
	})
	artists, err := e.ds.Artist(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.Or(clauses),
	})
	if err != nil {
		return nil, err
	}

	// Create a map for quick lookup
	artistMap := make(map[string]model.Artist)
	for _, artist := range artists {
		artistMap[artist.Name] = artist
	}

	count := 0

	// Process the similar artists
	for _, s := range similar {
		if artist, found := artistMap[s.Name]; found {
			result = append(result, artist)
			count++

			if count >= limit {
				break
			}
		} else {
			notPresent = append(notPresent, s.Name)
		}
	}

	// Then fill up with non-present artists
	if includeNotPresent && count < limit {
		for _, s := range notPresent {
			// Let the ID empty to indicate that the artist is not present in the DB
			sa := model.Artist{Name: s}
			result = append(result, sa)

			count++
			if count >= limit {
				break
			}
		}
	}

	return result, nil
}

// findArtistByName 按名称模糊查询本地艺术家，只取第一条。
func (e *provider) findArtistByName(ctx context.Context, artistName string) (*auxArtist, error) {
	artists, err := e.ds.Artist(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.Like{"artist.name": artistName},
		Max:     1,
	})
	if err != nil {
		return nil, err
	}
	if len(artists) == 0 {
		return nil, model.ErrNotFound
	}
	return &auxArtist{Artist: artists[0]}, nil
}

// loadSimilar 从数据库补全相似艺术家的完整信息（图片、统计等）。
// 借助 map 查表但按原数组顺序遍历，以保留外部返回的相似度排序。
func (e *provider) loadSimilar(ctx context.Context, artist *auxArtist, count int, includeNotPresent bool) error {
	var ids []string
	for _, sa := range artist.SimilarArtists {
		if sa.ID == "" {
			continue
		}
		ids = append(ids, sa.ID)
	}

	similar, err := e.ds.Artist(ctx).GetAll(model.QueryOptions{
		Filters: squirrel.Eq{"artist.id": ids},
	})
	if err != nil {
		log.Error("Error loading similar artists", "id", artist.ID, "name", artist.Name(), err)
		return err
	}

	// Use a map and iterate through original array, to keep the same order
	artistMap := make(map[string]model.Artist)
	for _, sa := range similar {
		artistMap[sa.ID] = sa
	}

	var loaded model.Artists
	for _, sa := range artist.SimilarArtists {
		if len(loaded) >= count {
			break
		}
		la, ok := artistMap[sa.ID]
		if !ok {
			if !includeNotPresent {
				continue
			}
			la = sa
			la.ID = ""
		}
		loaded = append(loaded, la)
	}
	artist.SimilarArtists = loaded
	return nil
}

// refreshQueue 是后台刷新队列，只暴露写入端。
type refreshQueue[T any] chan<- *T

// newRefreshQueue 创建刷新队列并启动消费协程。
//
// 每处理一项前先等 refreshDelay，起到全局限速作用——
// 大批量刷新时不至于把外部服务打挂。
// 单项限时 refreshTimeout，防止慢请求阻塞整个队列。
func newRefreshQueue[T any](ctx context.Context, processFn func(context.Context, T) (T, error)) refreshQueue[T] {
	queue := make(chan *T, refreshQueueLength)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(refreshDelay):
				ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
				select {
				case item := <-queue:
					_, _ = processFn(ctx, *item)
					cancel()
				case <-ctx.Done():
					cancel()
				}
			}
		}
	}()
	return queue
}

// enqueue 入队刷新请求。队列已满时直接丢弃：
// 刷新只是尽力而为的优化，漏掉一次无伤大雅，绝不能阻塞调用方。
func (q *refreshQueue[T]) enqueue(item *T) {
	select {
	case *q <- item:
	default: // It is ok to miss a refresh request
	}
}
