package metadata

import (
	"cmp"
	"strings"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/consts"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/utils/str"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

// roleTags 描述某个角色对应的三个标签：名字、排序名、MusicBrainz ID。
// sort 与 mbid 可为空，表示该角色在标签规范中没有对应项。
type roleTags struct {
	name model.TagName
	sort model.TagName
	mbid model.TagName
}

// roleMappings 是「固定角色 → 标签」的映射表。
// 这里只含每种角色对应独立标签的情形；
// 演奏者（Performer）因带子角色（乐器）而走键值对标签，单独处理。
var roleMappings = map[model.Role]roleTags{
	model.RoleComposer:  {name: model.TagComposer, sort: model.TagComposerSort, mbid: model.TagMusicBrainzComposerID},
	model.RoleLyricist:  {name: model.TagLyricist, sort: model.TagLyricistSort, mbid: model.TagMusicBrainzLyricistID},
	model.RoleConductor: {name: model.TagConductor, mbid: model.TagMusicBrainzConductorID},
	model.RoleArranger:  {name: model.TagArranger, mbid: model.TagMusicBrainzArrangerID},
	model.RoleDirector:  {name: model.TagDirector, mbid: model.TagMusicBrainzDirectorID},
	model.RoleProducer:  {name: model.TagProducer, mbid: model.TagMusicBrainzProducerID},
	model.RoleEngineer:  {name: model.TagEngineer, mbid: model.TagMusicBrainzEngineerID},
	model.RoleMixer:     {name: model.TagMixer, mbid: model.TagMusicBrainzMixerID},
	model.RoleRemixer:   {name: model.TagRemixer, mbid: model.TagMusicBrainzRemixerID},
	model.RoleDJMixer:   {name: model.TagDJMixer, mbid: model.TagMusicBrainzDJMixerID},
}

// mapParticipants 解析曲目的全部参与者，按角色归类。
//
// 顺序为：曲目艺人 → 专辑艺人 → 其他固定角色 → 演奏者（带子角色），
// 最后统一补全缺失的 MusicBrainz ID。
// 曲目艺人必须先解析，因为专辑艺人缺失时要回退到它。
func (md Metadata) mapParticipants() model.Participants {
	participants := make(model.Participants)

	// Parse track artists
	artists := md.parseArtists(
		model.TagTrackArtist, model.TagTrackArtists,
		model.TagTrackArtistSort, model.TagTrackArtistsSort,
		model.TagMusicBrainzArtistID,
	)
	participants.Add(model.RoleArtist, artists...)

	// Parse album artists
	albumArtists := md.parseArtists(
		model.TagAlbumArtist, model.TagAlbumArtists,
		model.TagAlbumArtistSort, model.TagAlbumArtistsSort,
		model.TagMusicBrainzAlbumArtistID,
	)
	// 专辑艺人标签缺失时（parseArtists 会填入 UnknownArtist）需要回退：
	// 合辑归到「群星」，否则沿用曲目艺人。
	// 否则同一张专辑的曲目会因专辑艺人为「未知」而被错误地聚到一起
	if len(albumArtists) == 1 && albumArtists[0].Name == consts.UnknownArtist {
		if md.Bool(model.TagCompilation) {
			albumArtists = md.buildArtists([]string{consts.VariousArtists}, nil, []string{consts.VariousArtistsMbzId})
		} else {
			albumArtists = artists
		}
	}
	participants.Add(model.RoleAlbumArtist, albumArtists...)

	// Parse all other roles
	// 其余固定角色：作曲、作词、指挥、制作人等
	for role, info := range roleMappings {
		names := md.getRoleValues(info.name)
		if len(names) > 0 {
			sorts := md.Strings(info.sort)
			mbids := md.Strings(info.mbid)
			artists := md.buildArtists(names, sorts, mbids)
			participants.Add(role, artists...)
		}
	}

	rolesMbzIdMap := md.buildRoleMbidMaps()
	md.processPerformers(participants, rolesMbzIdMap)
	md.syncMissingMbzIDs(participants)

	return participants
}

// buildRoleMbidMaps creates a map of roles to MBZ IDs
// buildRoleMbidMaps 建立「子角色 → MusicBrainz ID 列表」的映射。
// 演奏者的 MBID 与名字分散在两个键值对标签中，需按子角色配对，
// 因此先把 MBID 按子角色聚合，键统一转为首字母大写以消除大小写差异。
func (md Metadata) buildRoleMbidMaps() map[string][]string {
	titleCaser := cases.Title(language.Und)
	rolesMbzIdMap := make(map[string][]string)
	for _, mbid := range md.Pairs(model.TagMusicBrainzPerformerID) {
		role := titleCaser.String(mbid.Key())
		rolesMbzIdMap[role] = append(rolesMbzIdMap[role], mbid.Value())
	}

	return rolesMbzIdMap
}

// processPerformers 解析演奏者标签，子角色即所演奏的乐器。
// 演奏者与其 MBID 分列于两个标签，只能按「同一子角色内出现顺序」配对，
// 故用 roleIdx 为每个子角色维护游标逐个取用。
func (md Metadata) processPerformers(participants model.Participants, rolesMbzIdMap map[string][]string) {
	// roleIdx keeps track of the index of the MBZ ID for each role
	// roleIdx 记录每个子角色已消费到第几个 MBID
	roleIdx := make(map[string]int)
	for role := range rolesMbzIdMap {
		roleIdx[role] = 0
	}

	titleCaser := cases.Title(language.Und)
	for _, performer := range md.Pairs(model.TagPerformer) {
		name := performer.Value()
		subRole := titleCaser.String(performer.Key())

		artist := model.Artist{
			ID:              md.artistID(name),
			Name:            name,
			OrderArtistName: str.SanitizeFieldForSortingNoArticle(name),
			MbzArtistID:     md.getPerformerMbid(subRole, rolesMbzIdMap, roleIdx),
		}
		participants.AddWithSubRole(model.RolePerformer, subRole, artist)
	}
}

// getPerformerMbid returns the MBZ ID for a performer, based on the subrole
// getPerformerMbid 取该子角色下一个待用的 MBID，取完即前移游标。
// 用 defer 递增可保证先返回当前索引的值再前移。
// MBID 数量少于演奏者数量时返回空串，不做错位配对。
func (md Metadata) getPerformerMbid(subRole string, rolesMbzIdMap map[string][]string, roleIdx map[string]int) string {
	if mbids, exists := rolesMbzIdMap[subRole]; exists && roleIdx[subRole] < len(mbids) {
		defer func() { roleIdx[subRole]++ }()
		return mbids[roleIdx[subRole]]
	}
	return ""
}

// syncMissingMbzIDs fills in missing MBZ IDs for artists that have been previously parsed
// syncMissingMbzIDs 用已知的「艺人名 → MBID」补全其他角色中缺失的 MBID。
//
// 现实中的标签往往只为曲目/专辑艺人写了 MBID，
// 而同一人以制作人、演奏者等身份出现时没有。
// 按名字回填可让同一人在各角色下归并为同一艺人实体，避免重复条目。
func (md Metadata) syncMissingMbzIDs(participants model.Participants) {
	artistMbzIDMap := make(map[string]string)
	for _, artist := range append(participants[model.RoleArtist], participants[model.RoleAlbumArtist]...) {
		if artist.MbzArtistID != "" {
			artistMbzIDMap[artist.Name] = artist.MbzArtistID
		}
	}

	for role, list := range participants {
		for i, artist := range list {
			if artist.MbzArtistID == "" {
				if mbzID, exists := artistMbzIDMap[artist.Name]; exists {
					participants[role][i].MbzArtistID = mbzID
				}
			}
		}
	}
}

// parseArtists 解析艺人列表，兼容单值与多值两套标签。
// 名字为空时兜底为「未知艺人」，保证每首曲目都有艺人归属，
// 否则该曲目在按艺人浏览时会消失。
func (md Metadata) parseArtists(
	name model.TagName, names model.TagName, sort model.TagName,
	sorts model.TagName, mbid model.TagName,
) []model.Artist {
	nameValues := md.getArtistValues(name, names)
	sortValues := md.getArtistValues(sort, sorts)
	mbids := md.Strings(mbid)
	if len(nameValues) == 0 {
		nameValues = []string{consts.UnknownArtist}
	}
	return md.buildArtists(nameValues, sortValues, mbids)
}

// buildArtists 按下标把名字、排序名、MBID 三个平行列表组装成艺人对象。
// 三者长度常不一致（排序名与 MBID 经常缺失或不全），故逐个判界，
// 缺失部分留空而不报错。
func (md Metadata) buildArtists(names, sorts, mbids []string) []model.Artist {
	var artists []model.Artist
	for i, name := range names {
		id := md.artistID(name)
		artist := model.Artist{
			ID:              id,
			Name:            name,
			OrderArtistName: str.SanitizeFieldForSortingNoArticle(name),
		}
		if i < len(sorts) {
			artist.SortArtistName = sorts[i]
		}
		if i < len(mbids) {
			artist.MbzArtistID = mbids[i]
		}
		artists = append(artists, artist)
	}
	return artists
}

// getRoleValues returns the values of a role tag, splitting them if necessary
// getRoleValues 取角色标签的值，必要时按分隔符拆分为多人。
// 拆分规则优先用该标签自身的配置，未配置则回退到角色的通用配置，
// 使用户既能全局设定分隔符，也能对个别标签特殊处理。
func (md Metadata) getRoleValues(role model.TagName) []string {
	values := md.Strings(role)
	if len(values) == 0 {
		return nil
	}
	conf := model.TagMainMappings()[role]
	if conf.Split == nil {
		conf = model.TagRolesConf()
	}
	if len(conf.Split) > 0 {
		values = conf.SplitTagValue(values)
		return filterDuplicatedOrEmptyValues(values)
	}
	return values
}

// getArtistValues returns the values of a single or multi artist tag, splitting them if necessary
// getArtistValues 取艺人名列表，多值标签优先于单值标签。
//
// 多值标签（如 ARTISTS）已明确区分了每位艺人，可直接使用；
// 单值标签则可能把多位艺人写在一个字符串里，需按分隔符拆分。
// 仅当单值标签恰好只有一个值时才拆分：若本就有多个值，
// 说明写入方已做了分隔，再拆会破坏含分隔符的艺人名。
func (md Metadata) getArtistValues(single, multi model.TagName) []string {
	vMulti := md.Strings(multi)
	if len(vMulti) > 0 {
		return vMulti
	}
	vSingle := md.Strings(single)
	if len(vSingle) != 1 {
		return vSingle
	}
	conf := model.TagMainMappings()[single]
	if conf.Split == nil {
		conf = model.TagArtistsConf()
	}
	if len(conf.Split) > 0 {
		vSingle = conf.SplitTagValue(vSingle)
		return filterDuplicatedOrEmptyValues(vSingle)
	}
	return vSingle
}

// mapDisplayName 拼接用于展示的艺人名，多位艺人用配置的连接符相连。
// 展示名与参与者列表是两套数据：前者保留标签原貌供界面显示，
// 后者是拆分后的结构化数据供检索与聚合。
func (md Metadata) mapDisplayName(singularTagName, pluralTagName model.TagName) string {
	return cmp.Or(
		strings.Join(md.tags[singularTagName], conf.Server.Scanner.ArtistJoiner),
		strings.Join(md.tags[pluralTagName], conf.Server.Scanner.ArtistJoiner),
	)
}

// mapDisplayArtist 返回曲目艺人的展示名，缺失时为「未知艺人」。
func (md Metadata) mapDisplayArtist() string {
	return cmp.Or(
		md.mapDisplayName(model.TagTrackArtist, model.TagTrackArtists),
		consts.UnknownArtist,
	)
}

// mapDisplayAlbumArtist 返回专辑艺人的展示名。
// 依次回退：专辑艺人标签 → 已解析出的首位专辑艺人 → 兜底名
// （合辑兜底为「群星」，否则为「未知艺人」）。
func (md Metadata) mapDisplayAlbumArtist(mf model.MediaFile) string {
	fallbackName := consts.UnknownArtist
	if md.Bool(model.TagCompilation) {
		fallbackName = consts.VariousArtists
	}
	return cmp.Or(
		md.mapDisplayName(model.TagAlbumArtist, model.TagAlbumArtists),
		mf.Participants.First(model.RoleAlbumArtist).Name,
		fallbackName,
	)
}
