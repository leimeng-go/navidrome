// Package filter 汇集 Subsonic 各接口用到的查询条件构造函数，
// 把协议参数翻译成仓库层的 QueryOptions。
package filter

import (
	"time"

	. "github.com/Masterminds/squirrel"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/persistence"
)

// Options 是仓库查询选项的别名，简化本包的书写。
type Options = model.QueryOptions

// defaultFilters 默认排除标记为丢失的条目：文件已不在磁盘上，返回给客户端只会导致播放失败。
var defaultFilters = Eq{"missing": false}

// addDefaultFilters 把默认过滤条件与调用方条件做 AND 合并。
func addDefaultFilters(options Options) Options {
	if options.Filters == nil {
		options.Filters = defaultFilters
	} else {
		options.Filters = And{defaultFilters, options.Filters}
	}
	return options
}

// AlbumsByNewest 按入库时间倒序。
func AlbumsByNewest() Options {
	return addDefaultFilters(addDefaultFilters(Options{Sort: "recently_added", Order: "desc"}))
}

// AlbumsByRecent 按最近播放倒序，仅含播放过的专辑。
func AlbumsByRecent() Options {
	return addDefaultFilters(Options{Sort: "playDate", Order: "desc", Filters: Gt{"play_date": time.Time{}}})
}

// AlbumsByFrequent 按播放次数倒序，仅含播放过的专辑。
func AlbumsByFrequent() Options {
	return addDefaultFilters(Options{Sort: "playCount", Order: "desc", Filters: Gt{"play_count": 0}})
}

// AlbumsByRandom 随机排序。
func AlbumsByRandom() Options {
	return addDefaultFilters(Options{Sort: "random"})
}

// AlbumsByName 按专辑名排序。
func AlbumsByName() Options {
	return addDefaultFilters(Options{Sort: "name"})
}

// AlbumsByArtist 按艺人名排序。
func AlbumsByArtist() Options {
	return addDefaultFilters(Options{Sort: "artist"})
}

// AlbumsByArtistID 查询某艺人的专辑。
// 默认只算专辑艺人；开启 ArtistParticipations 后连参演专辑一并列出。
func AlbumsByArtistID(artistId string) Options {
	filters := []Sqlizer{
		persistence.Exists("json_tree(participants, '$.albumartist')", Eq{"value": artistId}),
	}
	if conf.Server.Subsonic.ArtistParticipations {
		filters = append(filters,
			persistence.Exists("json_tree(participants, '$.artist')", Eq{"value": artistId}),
		)
	}
	return addDefaultFilters(Options{
		Sort:    "max_year",
		Filters: Or(filters),
	})
}

// AlbumsByYear 查询年份区间内的专辑。
// 起止年份倒置时视为要求倒序，这是 Subsonic 客户端约定的表达方式。
// 专辑跨年份时只要区间与 [min_year, max_year] 有交集即命中。
func AlbumsByYear(fromYear, toYear int) Options {
	orderOption := ""
	if fromYear > toYear {
		fromYear, toYear = toYear, fromYear
		orderOption = "desc"
	}
	return addDefaultFilters(Options{
		Sort:  "max_year",
		Order: orderOption,
		Filters: Or{
			And{
				GtOrEq{"min_year": fromYear},
				LtOrEq{"min_year": toYear},
			},
			And{
				GtOrEq{"max_year": fromYear},
				LtOrEq{"max_year": toYear},
			},
		},
	})
}

// SongsByAlbum 查询专辑内曲目，按专辑内既定顺序（碟号、音轨号）排列。
func SongsByAlbum(albumId string) Options {
	return addDefaultFilters(Options{
		Filters: Eq{"album_id": albumId},
		Sort:    "album",
	})
}

// SongsByRandom 随机取曲目，可叠加风格与年份区间过滤。
func SongsByRandom(genre string, fromYear, toYear int) Options {
	options := Options{
		Sort: "random",
	}
	ff := And{}
	if genre != "" {
		ff = append(ff, filterByGenre(genre))
	}
	if fromYear != 0 {
		ff = append(ff, GtOrEq{"year": fromYear})
	}
	if toYear != 0 {
		ff = append(ff, LtOrEq{"year": toYear})
	}
	options.Filters = ff
	return addDefaultFilters(options)
}

// SongsByArtistTitleWithLyricsFirst 按艺人与标题查曲目。
// 同名曲目可能有多个版本，优先返回带歌词且更新更晚的那条。
func SongsByArtistTitleWithLyricsFirst(artist, title string) Options {
	return addDefaultFilters(Options{
		Sort:  "lyrics, updated_at",
		Order: "desc",
		Max:   1,
		Filters: And{
			Eq{"title": title},
			Or{
				persistence.Exists("json_tree(participants, '$.albumartist')", Eq{"value": artist}),
				persistence.Exists("json_tree(participants, '$.artist')", Eq{"value": artist}),
			},
		},
	})
}

// ApplyLibraryFilter 追加音乐库过滤条件。未指定库时不加限制。
func ApplyLibraryFilter(opts Options, musicFolderIds []int) Options {
	if len(musicFolderIds) == 0 {
		return opts
	}

	libraryFilter := Eq{"library_id": musicFolderIds}
	if opts.Filters == nil {
		opts.Filters = libraryFilter
	} else {
		opts.Filters = And{opts.Filters, libraryFilter}
	}

	return opts
}

// ApplyArtistLibraryFilter applies a filter to the given Options to ensure that only artists
// that are associated with the specified music folders are included in the results.
//
// ApplyArtistLibraryFilter 追加艺人的音乐库过滤。
// 艺人与库是多对多关系，需通过 library_artist 关联表过滤，不能直接用 library_id。
func ApplyArtistLibraryFilter(opts Options, musicFolderIds []int) Options {
	if len(musicFolderIds) == 0 {
		return opts
	}

	artistLibraryFilter := Eq{"library_artist.library_id": musicFolderIds}
	if opts.Filters == nil {
		opts.Filters = artistLibraryFilter
	} else {
		opts.Filters = And{opts.Filters, artistLibraryFilter}
	}

	return opts
}

// ByGenre 按风格过滤。
func ByGenre(genre string) Options {
	return addDefaultFilters(Options{
		Sort:    "name",
		Filters: filterByGenre(genre),
	})
}

// filterByGenre 在 tags 的 JSON 结构中匹配风格。
// 需排除 atom 为空的节点，否则会命中数组容器本身而非具体值。
func filterByGenre(genre string) Sqlizer {
	return persistence.Exists(`json_tree(tags, "$.genre")`, And{
		Like{"value": genre},
		NotEq{"atom": nil},
	})
}

// ByRating 按评分倒序，仅含已评分条目。
func ByRating() Options {
	return addDefaultFilters(Options{Sort: "rating", Order: "desc", Filters: Gt{"rating": 0}})
}

// ByStarred 按收藏时间倒序。
func ByStarred() Options {
	return addDefaultFilters(Options{Sort: "starred_at", Order: "desc", Filters: Eq{"starred": true}})
}

// ArtistsByStarred 收藏的艺人。
// 艺人表没有 missing 列，故不套用默认过滤条件。
func ArtistsByStarred() Options {
	return Options{Sort: "starred_at", Order: "desc", Filters: Eq{"starred": true}}
}
