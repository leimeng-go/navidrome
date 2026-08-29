package model

import (
	"cmp"
	"crypto/md5"
	"fmt"
	"slices"
	"strings"

	"github.com/navidrome/navidrome/utils/slice"
)

// 预定义的全部角色常量。Role 采用「私有字段包装」的设计，
// 外部无法凭空构造非法角色，只能通过 RoleFromString 转换，从而保证取值合法。
var (
	RoleInvalid     = Role{"invalid"} // 非法角色，作为转换失败的返回值
	RoleArtist      = Role{"artist"}
	RoleAlbumArtist = Role{"albumartist"}
	RoleComposer    = Role{"composer"}
	RoleConductor   = Role{"conductor"}
	RoleLyricist    = Role{"lyricist"}
	RoleArranger    = Role{"arranger"}
	RoleProducer    = Role{"producer"}
	RoleDirector    = Role{"director"}
	RoleEngineer    = Role{"engineer"}
	RoleMixer       = Role{"mixer"}
	RoleRemixer     = Role{"remixer"}
	RoleDJMixer     = Role{"djmixer"}
	RolePerformer   = Role{"performer"}
	// RoleMainCredit is a credit where the artist is an album artist or artist
	// RoleMainCredit 是「主要署名」的合成角色，等价于艺人或专辑艺人，
	// 用于按"主要参与者"筛选而无需分别匹配两个角色
	RoleMainCredit = Role{"maincredit"}
)

// AllRoles 是角色字符串到 Role 的映射表，注意其中不含 RoleInvalid，
// 因此查表失败即表示输入非法。
var AllRoles = map[string]Role{
	RoleArtist.role:      RoleArtist,
	RoleAlbumArtist.role: RoleAlbumArtist,
	RoleComposer.role:    RoleComposer,
	RoleConductor.role:   RoleConductor,
	RoleLyricist.role:    RoleLyricist,
	RoleArranger.role:    RoleArranger,
	RoleProducer.role:    RoleProducer,
	RoleDirector.role:    RoleDirector,
	RoleEngineer.role:    RoleEngineer,
	RoleMixer.role:       RoleMixer,
	RoleRemixer.role:     RoleRemixer,
	RoleDJMixer.role:     RoleDJMixer,
	RolePerformer.role:   RolePerformer,
	RoleMainCredit.role:  RoleMainCredit,
}

// Role represents the role of an artist in a track or album.
// Role 表示艺人在某曲目或专辑中承担的角色。
// 字段私有使其成为受控枚举：外部无法构造出未定义的角色值。
type Role struct {
	role string
}

func (r Role) String() string {
	return r.role
}

// MarshalText 实现 encoding.TextMarshaler，使 Role 可作为 JSON 的 map 键序列化。
func (r Role) MarshalText() (text []byte, err error) {
	return []byte(r.role), nil
}

// UnmarshalText 实现 encoding.TextUnmarshaler，反序列化时校验角色合法性，
// 遇到未知角色直接报错而非静默接受。
func (r *Role) UnmarshalText(text []byte) error {
	role := RoleFromString(string(text))
	if role == RoleInvalid {
		return fmt.Errorf("invalid role: %s", text)
	}
	*r = role
	return nil
}

// RoleFromString 把字符串转为 Role，未知取值返回 RoleInvalid。
func RoleFromString(role string) Role {
	if r, ok := AllRoles[role]; ok {
		return r
	}
	return RoleInvalid
}

// Participant 是一位参与者：艺人本体加上可选的子角色。
// 子角色用于细化描述，例如 performer 角色下的具体乐器（"guitar"、"piano"）。
type Participant struct {
	Artist
	SubRole string `json:"subRole,omitempty"`
}

type ParticipantList []Participant

// Join 拼接参与者名称，带子角色的会以「名字 (子角色)」形式呈现。
func (p ParticipantList) Join(sep string) string {
	return strings.Join(slice.Map(p, func(p Participant) string {
		if p.SubRole != "" {
			return p.Name + " (" + p.SubRole + ")"
		}
		return p.Name
	}), sep)
}

// Participants 按角色组织曲目/专辑的全部参与艺人，
// 是取代早期单一 ArtistID 字段的多角色模型。
type Participants map[Role]ParticipantList

// Add adds the artists to the role, ignoring duplicates.
// Add 把艺人加入指定角色，自动忽略重复项。
func (p Participants) Add(role Role, artists ...Artist) {
	participants := slice.Map(artists, func(artist Artist) Participant {
		return Participant{Artist: artist}
	})
	p.add(role, participants...)
}

// AddWithSubRole adds the artists to the role, ignoring duplicates.
// AddWithSubRole 带子角色地加入艺人（如 performer + "guitar"），自动忽略重复项。
func (p Participants) AddWithSubRole(role Role, subRole string, artists ...Artist) {
	participants := slice.Map(artists, func(artist Artist) Participant {
		return Participant{Artist: artist, SubRole: subRole}
	})
	p.add(role, participants...)
}

// Sort 对每个角色下的参与者按名字排序，使输出顺序稳定。
func (p Participants) Sort() {
	for _, artists := range p {
		slices.SortFunc(artists, func(a1, a2 Participant) int {
			return cmp.Compare(a1.Name, a2.Name)
		})
	}
}

// First returns the first artist for the role, or an empty artist if the role is not present.
// First 返回指定角色下的第一位艺人；角色不存在时返回零值 Artist 而非报错。
func (p Participants) First(role Role) Artist {
	if artists, ok := p[role]; ok && len(artists) > 0 {
		return artists[0].Artist
	}
	return Artist{}
}

// Merge merges the other Participants into this one.
// Merge 把另一组参与者并入当前集合，重复项会被忽略。
// 专辑聚合时用它汇总所有曲目的参与者。
func (p Participants) Merge(other Participants) {
	for role, artists := range other {
		p.add(role, artists...)
	}
}

// add 是去重追加的内部实现。
// 去重键为「艺人 ID + 子角色」，因为同一位艺人可以承担同一角色下的不同子角色
// （例如既弹吉他又弹贝斯），这两条记录都应保留。
func (p Participants) add(role Role, participants ...Participant) {
	seen := make(map[string]struct{}, len(p[role]))
	for _, artist := range p[role] {
		seen[artist.ID+artist.SubRole] = struct{}{}
	}
	for _, participant := range participants {
		key := participant.ID + participant.SubRole
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			p[role] = append(p[role], participant)
		}
	}
}

// AllArtists returns all artists found in the Participants.
// AllArtists 返回全部参与艺人（跨角色去重）。
// 先预统计总数一次性分配切片，再排序 + CompactFunc 按 ID 去重。
func (p Participants) AllArtists() []Artist {
	// First count the total number of artists to avoid reallocations.
	totalArtists := 0
	for _, roleArtists := range p {
		totalArtists += len(roleArtists)
	}
	artists := make(Artists, 0, totalArtists)
	for _, roleArtists := range p {
		artists = append(artists, slice.Map(roleArtists, func(p Participant) Artist { return p.Artist })...)
	}
	slices.SortStableFunc(artists, func(a1, a2 Artist) int {
		return cmp.Compare(a1.ID, a2.ID)
	})
	return slices.CompactFunc(artists, func(a1, a2 Artist) bool {
		return a1.ID == a2.ID
	})
}

// AllIDs returns all artist IDs found in the Participants.
// AllIDs 返回全部参与艺人的 ID（已去重）。
func (p Participants) AllIDs() []string {
	artists := p.AllArtists()
	return slice.Map(artists, func(a Artist) string { return a.ID })
}

// AllNames returns all artist names found in the Participants, including SortArtistNames.
// AllNames 返回全部参与艺人的名字，并一并收入排序名。
// 两者都纳入是为了让搜索既能匹配显示名，也能匹配排序名
// （例如搜 "Beatles" 可命中 "The Beatles"）。
func (p Participants) AllNames() []string {
	names := make([]string, 0, len(p))
	for _, artists := range p {
		for _, artist := range artists {
			names = append(names, artist.Name)
			if artist.SortArtistName != "" {
				names = append(names, artist.SortArtistName)
			}
		}
	}
	return slice.Unique(names)
}

// Hash 计算参与者集合的指纹，供 MediaFile.Hash 叠加使用。
// 内部对 ID 列表与角色行分别排序后再拼接，
// 确保 map 遍历顺序的随机性不会影响结果，同一组参与者始终得到相同哈希。
func (p Participants) Hash() []byte {
	flattened := make([]string, 0, len(p))
	for role, artists := range p {
		ids := slice.Map(artists, func(participant Participant) string { return participant.SubRole + ":" + participant.ID })
		slices.Sort(ids)
		flattened = append(flattened, role.String()+":"+strings.Join(ids, "/"))
	}
	slices.Sort(flattened)
	sum := md5.New()
	sum.Write([]byte(strings.Join(flattened, "|")))
	return sum.Sum(nil)
}
