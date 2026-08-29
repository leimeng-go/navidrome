package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/RaveNoX/go-jsoncommentstrip"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/criteria"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/utils/ioutils"
	"github.com/navidrome/navidrome/utils/slice"
	"golang.org/x/text/unicode/norm"
)

// Playlists 负责播放列表的导入与更新。
// 支持两种格式：.m3u/.m3u8 普通列表，以及 .nsp 智能列表（JSON 规则）。
type Playlists interface {
	ImportFile(ctx context.Context, folder *model.Folder, filename string) (*model.Playlist, error)
	Update(ctx context.Context, playlistID string, name *string, comment *string, public *bool, idsToAdd []string, idxToRemove []int) error
	ImportM3U(ctx context.Context, reader io.Reader) (*model.Playlist, error)
}

type playlists struct {
	ds model.DataStore
}

// NewPlaylists 创建播放列表服务。
func NewPlaylists(ds model.DataStore) Playlists {
	return &playlists{ds: ds}
}

// InPlaylistsPath 判断目录是否位于允许导入播放列表的路径内。
// PlaylistsPath 为空表示不限制；否则按 doublestar 通配符逐条匹配库内相对路径。
func InPlaylistsPath(folder model.Folder) bool {
	if conf.Server.PlaylistsPath == "" {
		return true
	}
	rel, _ := filepath.Rel(folder.LibraryPath, folder.AbsolutePath())
	for _, path := range strings.Split(conf.Server.PlaylistsPath, string(filepath.ListSeparator)) {
		if match, _ := doublestar.Match(path, rel); match {
			return true
		}
	}
	return false
}

// ImportFile 从音乐库中的播放列表文件导入，导入后保持与文件同步（Sync=true）。
func (s *playlists) ImportFile(ctx context.Context, folder *model.Folder, filename string) (*model.Playlist, error) {
	pls, err := s.parsePlaylist(ctx, filename, folder)
	if err != nil {
		log.Error(ctx, "Error parsing playlist", "path", filepath.Join(folder.AbsolutePath(), filename), err)
		return nil, err
	}
	log.Debug("Found playlist", "name", pls.Name, "lastUpdated", pls.UpdatedAt, "path", pls.Path, "numTracks", len(pls.Tracks))
	err = s.updatePlaylist(ctx, pls)
	if err != nil {
		log.Error(ctx, "Error updating playlist", "path", filepath.Join(folder.AbsolutePath(), filename), err)
	}
	return pls, err
}

// ImportM3U 从用户上传的流中导入 M3U 播放列表。
// 与 ImportFile 不同，此处 Sync=false——列表无对应文件，不参与后续同步。
func (s *playlists) ImportM3U(ctx context.Context, reader io.Reader) (*model.Playlist, error) {
	owner, _ := request.UserFrom(ctx)
	pls := &model.Playlist{
		OwnerID: owner.ID,
		Public:  false,
		Sync:    false,
	}
	err := s.parseM3U(ctx, pls, nil, reader)
	if err != nil {
		log.Error(ctx, "Error parsing playlist", err)
		return nil, err
	}
	err = s.ds.Playlist(ctx).Put(pls)
	if err != nil {
		log.Error(ctx, "Error saving playlist", err)
		return nil, err
	}
	return pls, nil
}

// parsePlaylist 按扩展名分派到对应解析器，并统一转码为 UTF-8。
func (s *playlists) parsePlaylist(ctx context.Context, playlistFile string, folder *model.Folder) (*model.Playlist, error) {
	pls, err := s.newSyncedPlaylist(folder.AbsolutePath(), playlistFile)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(pls.Path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	reader := ioutils.UTF8Reader(file)
	extension := strings.ToLower(filepath.Ext(playlistFile))
	switch extension {
	case ".nsp":
		err = s.parseNSP(ctx, pls, reader)
	default:
		err = s.parseM3U(ctx, pls, folder, reader)
	}
	return pls, err
}

// newSyncedPlaylist 构造一个与文件关联的同步型播放列表，
// 名称取自文件名（去扩展名），UpdatedAt 取文件修改时间以便判断是否需要重新导入。
func (s *playlists) newSyncedPlaylist(baseDir string, playlistFile string) (*model.Playlist, error) {
	playlistPath := filepath.Join(baseDir, playlistFile)
	info, err := os.Stat(playlistPath)
	if err != nil {
		return nil, err
	}

	var extension = filepath.Ext(playlistFile)
	var name = playlistFile[0 : len(playlistFile)-len(extension)]

	pls := &model.Playlist{
		Name:      name,
		Comment:   fmt.Sprintf("Auto-imported from '%s'", playlistFile),
		Public:    false,
		Path:      playlistPath,
		Sync:      true,
		UpdatedAt: info.ModTime(),
	}
	return pls, nil
}

// getPositionFromOffset 把字节偏移换算为行列号，用于生成可读的 JSON 语法错误提示。
func getPositionFromOffset(data []byte, offset int64) (line, column int) {
	line = 1
	for _, b := range data[:offset] {
		if b == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return
}

// parseNSP 解析 .nsp 智能播放列表（JSON 规则）。
// 限制 100KB 以防超大文件耗尽内存；解析前剥离注释，
// 因为 .nsp 允许写注释而标准 JSON 不允许。
func (s *playlists) parseNSP(_ context.Context, pls *model.Playlist, reader io.Reader) error {
	nsp := &nspFile{}
	reader = io.LimitReader(reader, 100*1024) // Limit to 100KB
	reader = jsoncommentstrip.NewReader(reader)
	input, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("reading SmartPlaylist: %w", err)
	}
	err = json.Unmarshal(input, nsp)
	if err != nil {
		var syntaxErr *json.SyntaxError
		if errors.As(err, &syntaxErr) {
			line, col := getPositionFromOffset(input, syntaxErr.Offset)
			return fmt.Errorf("JSON syntax error in SmartPlaylist at line %d, column %d: %w", line, col, err)
		}
		return fmt.Errorf("JSON parsing error in SmartPlaylist: %w", err)
	}
	pls.Rules = &nsp.Criteria
	if nsp.Name != "" {
		pls.Name = nsp.Name
	}
	if nsp.Comment != "" {
		pls.Comment = nsp.Comment
	}
	return nil
}

// parseM3U 解析 M3U 播放列表并把每行路径映射为库中的曲目。
//
// 按 400 行分批处理，使超大列表的内存占用保持恒定。
// 每批先过滤（提取 #PLAYLIST: 名称、跳过注释与非音频行、还原 file:// URL 转义），
// 再解析为「库ID:相对路径」并批量查库。
//
// 路径统一转为 NFD 小写后比对：macOS 文件系统使用 NFD 编码，
// 数据库亦按 NFD 存储，不归一化会导致含重音字符的路径匹配失败（issue #4663）。
//
// 最后按原始行顺序回填曲目，保持播放列表的顺序语义。
func (s *playlists) parseM3U(ctx context.Context, pls *model.Playlist, folder *model.Folder, reader io.Reader) error {
	mediaFileRepository := s.ds.MediaFile(ctx)
	var mfs model.MediaFiles
	for lines := range slice.CollectChunks(slice.LinesFrom(reader), 400) {
		filteredLines := make([]string, 0, len(lines))
		for _, line := range lines {
			line := strings.TrimSpace(line)
			if strings.HasPrefix(line, "#PLAYLIST:") {
				pls.Name = line[len("#PLAYLIST:"):]
				continue
			}
			// Skip empty lines and extended info
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if strings.HasPrefix(line, "file://") {
				line = strings.TrimPrefix(line, "file://")
				line, _ = url.QueryUnescape(line)
			}
			if !model.IsAudioFile(line) {
				continue
			}
			filteredLines = append(filteredLines, line)
		}
		resolvedPaths, err := s.resolvePaths(ctx, folder, filteredLines)
		if err != nil {
			log.Warn(ctx, "Error resolving paths in playlist", "playlist", pls.Name, err)
			continue
		}

		// Normalize to NFD for filesystem compatibility (macOS). Database stores paths in NFD.
		// See https://github.com/navidrome/navidrome/issues/4663
		resolvedPaths = slice.Map(resolvedPaths, func(path string) string {
			return strings.ToLower(norm.NFD.String(path))
		})

		found, err := mediaFileRepository.FindByPaths(resolvedPaths)
		if err != nil {
			log.Warn(ctx, "Error reading files from DB", "playlist", pls.Name, err)
			continue
		}
		// Build lookup map with library-qualified keys, normalized for comparison
		existing := make(map[string]int, len(found))
		for idx := range found {
			// Normalize to lowercase for case-insensitive comparison
			// Key format: "libraryID:path"
			key := fmt.Sprintf("%d:%s", found[idx].LibraryID, strings.ToLower(found[idx].Path))
			existing[key] = idx
		}

		// Find media files in the order of the resolved paths, to keep playlist order
		for _, path := range resolvedPaths {
			idx, ok := existing[path]
			if ok {
				mfs = append(mfs, found[idx])
			} else {
				log.Warn(ctx, "Path in playlist not found", "playlist", pls.Name, "path", path)
			}
		}
	}
	if pls.Name == "" {
		pls.Name = time.Now().Format(time.RFC3339)
	}
	pls.Tracks = nil
	pls.AddMediaFiles(mfs)

	return nil
}

// pathResolution holds the result of resolving a playlist path to a library-relative path.
// pathResolution 保存一条播放列表路径的解析结果。
type pathResolution struct {
	absolutePath string
	libraryPath  string
	libraryID    int
	valid        bool
}

// ToQualifiedString converts the path resolution to a library-qualified string with forward slashes.
// Format: "libraryID:relativePath" with forward slashes for path separators.
//
// ToQualifiedString 生成「库ID:相对路径」形式的限定键。
// 带库 ID 是因为不同库下可能存在同名相对路径；
// 分隔符统一为正斜杠以保证跨平台一致。
func (r pathResolution) ToQualifiedString() (string, error) {
	if !r.valid {
		return "", fmt.Errorf("invalid path resolution")
	}
	relativePath, err := filepath.Rel(r.libraryPath, r.absolutePath)
	if err != nil {
		return "", err
	}
	// Convert path separators to forward slashes
	return fmt.Sprintf("%d:%s", r.libraryID, filepath.ToSlash(relativePath)), nil
}

// libraryMatcher holds sorted libraries with cleaned paths for efficient path matching.
// libraryMatcher 持有按路径长度倒序排列的库列表，用于快速定位路径归属。
type libraryMatcher struct {
	libraries    model.Libraries
	cleanedPaths []string
}

// findLibraryForPath finds which library contains the given absolute path.
// Returns library ID and path, or 0 and empty string if not found.
//
// findLibraryForPath 查找包含给定绝对路径的音乐库。
// 除前缀匹配外还须校验路径边界，
// 否则 /music-extra 会被误判为属于 /music。
func (lm *libraryMatcher) findLibraryForPath(absolutePath string) (int, string) {
	// Check sorted libraries (longest path first) to find the best match
	for i, cleanLibPath := range lm.cleanedPaths {
		// Check if absolutePath is under this library path
		if strings.HasPrefix(absolutePath, cleanLibPath) {
			// Ensure it's a proper path boundary (not just a prefix)
			if len(absolutePath) == len(cleanLibPath) || absolutePath[len(cleanLibPath)] == filepath.Separator {
				return lm.libraries[i].ID, cleanLibPath
			}
		}
	}
	return 0, ""
}

// newLibraryMatcher creates a libraryMatcher with libraries sorted by path length (longest first).
// This ensures correct matching when library paths are prefixes of each other.
// Example: /music-classical must be checked before /music
// Otherwise, /music-classical/track.mp3 would match /music instead of /music-classical
//
// newLibraryMatcher 构造匹配器：按路径长度降序排列，
// 使嵌套或前缀相同的库路径能匹配到最具体的那个；
// 路径预先 Clean 一次，避免每次匹配重复计算。
func newLibraryMatcher(libs model.Libraries) *libraryMatcher {
	// Sort libraries by path length (descending) to ensure longest paths match first.
	slices.SortFunc(libs, func(i, j model.Library) int {
		return cmp.Compare(len(j.Path), len(i.Path)) // Reverse order for descending
	})

	// Pre-clean all library paths once for efficient matching
	cleanedPaths := make([]string, len(libs))
	for i, lib := range libs {
		cleanedPaths[i] = filepath.Clean(lib.Path)
	}
	return &libraryMatcher{
		libraries:    libs,
		cleanedPaths: cleanedPaths,
	}
}

// pathResolver handles path resolution logic for playlist imports.
// pathResolver 承担播放列表导入时的路径解析。
type pathResolver struct {
	matcher *libraryMatcher
}

// newPathResolver creates a pathResolver with libraries loaded from the datastore.
// newPathResolver 载入全部音乐库并构造解析器。
func newPathResolver(ctx context.Context, ds model.DataStore) (*pathResolver, error) {
	libs, err := ds.Library(ctx).GetAll()
	if err != nil {
		return nil, err
	}
	matcher := newLibraryMatcher(libs)
	return &pathResolver{matcher: matcher}, nil
}

// resolvePath determines the absolute path and library path for a playlist entry.
// For absolute paths, it uses them directly.
// For relative paths, it resolves them relative to the playlist's folder location.
// Example: playlist at /music/playlists/test.m3u with line "../songs/abc.mp3"
//
//	resolves to /music/songs/abc.mp3
//
// resolvePath 解析单行路径：相对路径以播放列表所在目录为基准展开，
// 绝对路径直接使用。folder 为 nil（上传导入）时只接受绝对路径。
func (r *pathResolver) resolvePath(line string, folder *model.Folder) pathResolution {
	var absolutePath string
	if folder != nil && !filepath.IsAbs(line) {
		// Resolve relative path to absolute path based on playlist location
		absolutePath = filepath.Clean(filepath.Join(folder.AbsolutePath(), line))
	} else {
		// Use absolute path directly after cleaning
		absolutePath = filepath.Clean(line)
	}

	return r.findInLibraries(absolutePath)
}

// findInLibraries matches an absolute path against all known libraries and returns
// a pathResolution with the library information. Returns an invalid resolution if
// the path is not found in any library.
//
// findInLibraries 把绝对路径归属到某个音乐库，不属于任何库则返回无效结果。
func (r *pathResolver) findInLibraries(absolutePath string) pathResolution {
	libID, libPath := r.matcher.findLibraryForPath(absolutePath)
	if libID == 0 {
		return pathResolution{valid: false}
	}
	return pathResolution{
		absolutePath: absolutePath,
		libraryPath:  libPath,
		libraryID:    libID,
		valid:        true,
	}
}

// resolvePaths converts playlist file paths to library-qualified paths (format: "libraryID:relativePath").
// For relative paths, it resolves them to absolute paths first, then determines which
// library they belong to. This allows playlists to reference files across library boundaries.
//
// resolvePaths 批量解析路径。无法归属到任何库的行只记录告警并跳过，
// 单行异常不应导致整个播放列表导入失败。
func (s *playlists) resolvePaths(ctx context.Context, folder *model.Folder, lines []string) ([]string, error) {
	resolver, err := newPathResolver(ctx, s.ds)
	if err != nil {
		return nil, err
	}

	results := make([]string, 0, len(lines))
	for idx, line := range lines {
		resolution := resolver.resolvePath(line, folder)

		if !resolution.valid {
			log.Warn(ctx, "Path in playlist not found in any library", "path", line, "line", idx)
			continue
		}

		qualifiedPath, err := resolution.ToQualifiedString()
		if err != nil {
			log.Debug(ctx, "Error getting library-qualified path", "path", line,
				"libPath", resolution.libraryPath, "filePath", resolution.absolutePath, err)
			continue
		}

		results = append(results, qualifiedPath)
	}

	return results, nil
}

// updatePlaylist 落库播放列表：不存在则新建，已存在则更新。
//
// 已存在但 Sync=false 表示用户曾手工接管该列表，此时不再覆盖。
// 同步更新时保留数据库中的名称、备注、归属与可见性——
// 这些属于用户设置，只有曲目内容才来自文件。
func (s *playlists) updatePlaylist(ctx context.Context, newPls *model.Playlist) error {
	owner, _ := request.UserFrom(ctx)

	pls, err := s.ds.Playlist(ctx).FindByPath(newPls.Path)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return err
	}
	if err == nil && !pls.Sync {
		log.Debug(ctx, "Playlist already imported and not synced", "playlist", pls.Name, "path", pls.Path)
		return nil
	}

	if err == nil {
		log.Info(ctx, "Updating synced playlist", "playlist", pls.Name, "path", newPls.Path)
		newPls.ID = pls.ID
		newPls.Name = pls.Name
		newPls.Comment = pls.Comment
		newPls.OwnerID = pls.OwnerID
		newPls.Public = pls.Public
		newPls.EvaluatedAt = &time.Time{}
	} else {
		log.Info(ctx, "Adding synced playlist", "playlist", newPls.Name, "path", newPls.Path, "owner", owner.UserName)
		newPls.OwnerID = owner.ID
		newPls.Public = conf.Server.DefaultPlaylistPublicVisibility
	}
	return s.ds.Playlist(ctx).Put(newPls)
}

// Update 更新播放列表的元信息与曲目，整体在一个立即写事务中完成。
//
// 指针参数为 nil 表示该字段不修改，从而区分「不改」与「改为空值」。
//
// 分两条路径：需要删除曲目时必须载入完整曲目列表，在内存中按下标增删后整体写回；
// 仅追加时可直接走增量添加，省去加载全部曲目的开销。
// 若删除后列表为空，需显式清空关联表。
func (s *playlists) Update(ctx context.Context, playlistID string,
	name *string, comment *string, public *bool,
	idsToAdd []string, idxToRemove []int) error {
	needsInfoUpdate := name != nil || comment != nil || public != nil
	needsTrackRefresh := len(idxToRemove) > 0

	return s.ds.WithTxImmediate(func(tx model.DataStore) error {
		var pls *model.Playlist
		var err error
		repo := tx.Playlist(ctx)
		tracks := repo.Tracks(playlistID, true)
		if tracks == nil {
			return fmt.Errorf("%w: playlist '%s'", model.ErrNotFound, playlistID)
		}
		if needsTrackRefresh {
			pls, err = repo.GetWithTracks(playlistID, true, false)
			pls.RemoveTracks(idxToRemove)
			pls.AddMediaFilesByID(idsToAdd)
		} else {
			if len(idsToAdd) > 0 {
				_, err = tracks.Add(idsToAdd)
				if err != nil {
					return err
				}
			}
			if needsInfoUpdate {
				pls, err = repo.Get(playlistID)
			}
		}
		if err != nil {
			return err
		}
		if !needsTrackRefresh && !needsInfoUpdate {
			return nil
		}

		if name != nil {
			pls.Name = *name
		}
		if comment != nil {
			pls.Comment = *comment
		}
		if public != nil {
			pls.Public = *public
		}
		// Special case: The playlist is now empty
		if len(idxToRemove) > 0 && len(pls.Tracks) == 0 {
			if err = tracks.DeleteAll(); err != nil {
				return err
			}
		}
		return repo.Put(pls)
	})
}

// nspFile 是 .nsp 智能播放列表文件的结构：筛选规则 + 名称与备注。
type nspFile struct {
	criteria.Criteria
	Name    string `json:"name"`
	Comment string `json:"comment"`
}

// UnmarshalJSON 自定义反序列化。
// 因 Criteria 为匿名嵌入且自带 UnmarshalJSON，直接解码会吞掉 name/comment 字段，
// 故先单独取出这两项，再把整体交给 Criteria 解析。
func (i *nspFile) UnmarshalJSON(data []byte) error {
	m := map[string]interface{}{}
	err := json.Unmarshal(data, &m)
	if err != nil {
		return err
	}
	i.Name, _ = m["name"].(string)
	i.Comment, _ = m["comment"].(string)
	return json.Unmarshal(data, &i.Criteria)
}
