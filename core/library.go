package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/deluan/rest"
	"github.com/navidrome/navidrome/core/storage"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/model/request"
	"github.com/navidrome/navidrome/server/events"
	"github.com/navidrome/navidrome/utils/slice"
)

// Watcher interface for managing file system watchers
// Watcher 是文件监听器的最小接口。
// 此处重新声明而非直接引用 scanner 包，是为了避免 core 与 scanner 之间的循环依赖。
type Watcher interface {
	Watch(ctx context.Context, lib *model.Library) error
	StopWatching(ctx context.Context, libraryID int) error
}

// Library provides business logic for library management and user-library associations
// Library 提供音乐库管理与「用户—库」授权关系的业务逻辑。
type Library interface {
	GetUserLibraries(ctx context.Context, userID string) (model.Libraries, error)
	SetUserLibraries(ctx context.Context, userID string, libraryIDs []int) error
	ValidateLibraryAccess(ctx context.Context, userID string, libraryID int) error

	NewRepository(ctx context.Context) rest.Repository
}

// libraryService 是 Library 的实现。
type libraryService struct {
	ds      model.DataStore
	scanner model.Scanner
	watcher Watcher
	broker  events.Broker
}

// NewLibrary creates a new Library service
// NewLibrary 创建音乐库服务。
func NewLibrary(ds model.DataStore, scanner model.Scanner, watcher Watcher, broker events.Broker) Library {
	return &libraryService{
		ds:      ds,
		scanner: scanner,
		watcher: watcher,
		broker:  broker,
	}
}

// User-library association operations
// 「用户—库」授权关系相关操作

// GetUserLibraries 返回用户可访问的音乐库列表。
func (s *libraryService) GetUserLibraries(ctx context.Context, userID string) (model.Libraries, error) {
	// Verify user exists
	if _, err := s.ds.User(ctx).Get(userID); err != nil {
		return nil, err
	}

	return s.ds.User(ctx).GetUserLibraries(userID)
}

// SetUserLibraries 设置用户可访问的音乐库。
//
// 两条业务约束：管理员自动拥有全部库，禁止手工指定（否则会与自动授权冲突）；
// 普通用户至少要有一个库，否则登录后将看不到任何内容。
// 变更后广播刷新事件，让在线客户端立即感知权限变化。
func (s *libraryService) SetUserLibraries(ctx context.Context, userID string, libraryIDs []int) error {
	// Verify user exists
	user, err := s.ds.User(ctx).Get(userID)
	if err != nil {
		return err
	}

	// Admin users get all libraries automatically - don't allow manual assignment
	if user.IsAdmin {
		return fmt.Errorf("%w: cannot manually assign libraries to admin users", model.ErrValidation)
	}

	// Regular users must have at least one library
	if len(libraryIDs) == 0 {
		return fmt.Errorf("%w: at least one library must be assigned to non-admin users", model.ErrValidation)
	}

	// Validate all library IDs exist
	if len(libraryIDs) > 0 {
		if err := s.validateLibraryIDs(ctx, libraryIDs); err != nil {
			return err
		}
	}

	// Set user libraries
	err = s.ds.User(ctx).SetUserLibraries(userID, libraryIDs)
	if err != nil {
		return fmt.Errorf("error setting user libraries: %w", err)
	}

	// Send refresh event to all clients
	event := &events.RefreshResource{}
	libIDs := slice.Map(libraryIDs, func(id int) string { return strconv.Itoa(id) })
	event = event.With("user", userID).With("library", libIDs...)
	s.broker.SendBroadcastMessage(ctx, event)
	return nil
}

// ValidateLibraryAccess 校验用户是否有权访问指定音乐库。
// 注意：管理员判定取自 context 中的当前登录用户，而授权列表按传入的 userID 查询。
func (s *libraryService) ValidateLibraryAccess(ctx context.Context, userID string, libraryID int) error {
	user, ok := request.UserFrom(ctx)
	if !ok {
		return fmt.Errorf("user not found in context")
	}

	// Admin users have access to all libraries
	if user.IsAdmin {
		return nil
	}

	// Check if user has explicit access to this library
	libraries, err := s.ds.User(ctx).GetUserLibraries(userID)
	if err != nil {
		log.Error(ctx, "Error checking library access", "userID", userID, "libraryID", libraryID, err)
		return fmt.Errorf("error checking library access: %w", err)
	}

	for _, lib := range libraries {
		if lib.ID == libraryID {
			return nil
		}
	}

	return fmt.Errorf("%w: user does not have access to library %d", model.ErrNotAuthorized, libraryID)
}

// REST repository wrapper
// REST 仓储包装层

// NewRepository 返回带业务副作用的 REST 仓储：
// 在纯数据操作之外，附加校验、监听器启停、扫描触发与事件广播。
func (s *libraryService) NewRepository(ctx context.Context) rest.Repository {
	repo := s.ds.Library(ctx)
	wrapper := &libraryRepositoryWrapper{
		ctx:               ctx,
		LibraryRepository: repo,
		Repository:        repo.(rest.Repository),
		ds:                s.ds,
		scanner:           s.scanner,
		watcher:           s.watcher,
		broker:            s.broker,
	}
	return wrapper
}

// libraryRepositoryWrapper 同时嵌入 REST 与领域仓储接口：
// 未被覆写的方法直接透传，Save/Update/Delete 则加上业务副作用。
type libraryRepositoryWrapper struct {
	rest.Repository
	model.LibraryRepository
	ctx     context.Context
	ds      model.DataStore
	scanner model.Scanner
	watcher Watcher
	broker  events.Broker
}

// Save 新建音乐库，随后启动监听、异步触发扫描并广播刷新事件。
// 扫描放在独立协程中，避免阻塞创建请求的响应。
func (r *libraryRepositoryWrapper) Save(entity interface{}) (string, error) {
	lib := entity.(*model.Library)
	if err := r.validateLibrary(lib); err != nil {
		return "", err
	}

	err := r.LibraryRepository.Put(lib)
	if err != nil {
		return "", r.mapError(err)
	}

	// Start watcher and trigger scan after successful library creation
	if r.watcher != nil {
		if err := r.watcher.Watch(r.ctx, lib); err != nil {
			log.Warn(r.ctx, "Failed to start watcher for new library", "libraryID", lib.ID, "name", lib.Name, "path", lib.Path, err)
		}
	}

	if r.scanner != nil {
		go r.triggerScan(lib, "new")
	}

	// Send library refresh event to all clients
	if r.broker != nil {
		event := &events.RefreshResource{}
		r.broker.SendBroadcastMessage(r.ctx, event.With("library", strconv.Itoa(lib.ID)))
		log.Debug(r.ctx, "Library created - sent refresh event", "libraryID", lib.ID, "name", lib.Name)
	}

	return strconv.Itoa(lib.ID), nil
}

// Update 更新音乐库。
// 仅当路径发生变化时才重启监听并重新扫描——
// 改名等操作不影响文件系统，无需付出扫描代价。
func (r *libraryRepositoryWrapper) Update(id string, entity interface{}, _ ...string) error {
	lib := entity.(*model.Library)
	libID, err := strconv.Atoi(id)
	if err != nil {
		return fmt.Errorf("invalid library ID: %s", id)
	}

	lib.ID = libID
	if err := r.validateLibrary(lib); err != nil {
		return err
	}

	// Get the original library to check if path changed
	originalLib, err := r.Get(libID)
	if err != nil {
		return r.mapError(err)
	}

	pathChanged := originalLib.Path != lib.Path

	err = r.LibraryRepository.Put(lib)
	if err != nil {
		return r.mapError(err)
	}

	// Restart watcher and trigger scan if path was updated
	if pathChanged {
		if r.watcher != nil {
			if err := r.watcher.Watch(r.ctx, lib); err != nil {
				log.Warn(r.ctx, "Failed to restart watcher for updated library", "libraryID", lib.ID, "name", lib.Name, "path", lib.Path, err)
			}
		}

		if r.scanner != nil {
			go r.triggerScan(lib, "updated")
		}
	}

	// Send library refresh event to all clients
	if r.broker != nil {
		event := &events.RefreshResource{}
		r.broker.SendBroadcastMessage(r.ctx, event.With("library", id))
		log.Debug(r.ctx, "Library updated - sent refresh event", "libraryID", libID, "name", lib.Name)
	}

	return nil
}

// Delete 删除音乐库，停止监听并触发扫描以清理残留的孤儿数据。
func (r *libraryRepositoryWrapper) Delete(id string) error {
	libID, err := strconv.Atoi(id)
	if err != nil {
		return &rest.ValidationError{Errors: map[string]string{
			"id": "invalid library ID format",
		}}
	}

	// Get library info before deletion for logging
	lib, err := r.Get(libID)
	if err != nil {
		return r.mapError(err)
	}

	err = r.LibraryRepository.Delete(libID)
	if err != nil {
		return r.mapError(err)
	}

	// Stop watcher and trigger scan after successful library deletion to clean up orphaned data
	if r.watcher != nil {
		if err := r.watcher.StopWatching(r.ctx, libID); err != nil {
			log.Warn(r.ctx, "Failed to stop watcher for deleted library", "libraryID", libID, "name", lib.Name, "path", lib.Path, err)
		}
	}

	if r.scanner != nil {
		go r.triggerScan(lib, "deleted")
	}

	// Send library refresh event to all clients
	if r.broker != nil {
		event := &events.RefreshResource{}
		r.broker.SendBroadcastMessage(r.ctx, event.With("library", id))
		log.Debug(r.ctx, "Library deleted - sent refresh event", "libraryID", libID, "name", lib.Name)
	}

	return nil
}

// Helper methods
// 辅助方法

// mapError 把底层错误翻译为 REST 层错误。
// 唯一约束冲突通过匹配错误文本识别（SQLite 未提供结构化错误码），
// 返回的是 react-admin 的翻译键而非可读文案。
func (r *libraryRepositoryWrapper) mapError(err error) error {
	if err == nil {
		return nil
	}

	errStr := err.Error()

	// Handle database constraint violations.
	// TODO: Being tied to react-admin translations is not ideal, but this will probably go away with the new UI/API
	if strings.Contains(errStr, "UNIQUE constraint failed") {
		if strings.Contains(errStr, "library.name") {
			return &rest.ValidationError{Errors: map[string]string{"name": "ra.validation.unique"}}
		}
		if strings.Contains(errStr, "library.path") {
			return &rest.ValidationError{Errors: map[string]string{"path": "ra.validation.unique"}}
		}
	}

	switch {
	case errors.Is(err, model.ErrNotFound):
		return rest.ErrNotFound
	case errors.Is(err, model.ErrNotAuthorized):
		return rest.ErrPermissionDenied
	default:
		return err
	}
}

// validateLibrary 校验音乐库字段，收集全部错误后一次性返回，
// 便于前端同时高亮所有非法字段。
func (r *libraryRepositoryWrapper) validateLibrary(library *model.Library) error {
	validationErrors := make(map[string]string)

	if library.Name == "" {
		validationErrors["name"] = "ra.validation.required"
	}

	if library.Path == "" {
		validationErrors["path"] = "ra.validation.required"
	} else {
		// Validate path format and accessibility
		if err := r.validateLibraryPath(library); err != nil {
			validationErrors["path"] = err.Error()
		}
	}

	if len(validationErrors) > 0 {
		return &rest.ValidationError{Errors: validationErrors}
	}

	return nil
}

// validateLibraryPath 校验库路径必须为绝对路径、存在且为可访问的目录。
// 校验过程中会把路径规范化后写回 library.Path。
// 返回值是 i18n 翻译键，由前端渲染为本地化文案。
func (r *libraryRepositoryWrapper) validateLibraryPath(library *model.Library) error {
	// Validate path format
	if !filepath.IsAbs(library.Path) {
		return fmt.Errorf("library path must be absolute")
	}

	// Clean the path to normalize it
	cleanPath := filepath.Clean(library.Path)
	library.Path = cleanPath

	// Check if path exists and is accessible using storage abstraction
	fileStore, err := storage.For(library.Path)
	if err != nil {
		return fmt.Errorf("invalid storage scheme: %w", err)
	}

	fsys, err := fileStore.FS()
	if err != nil {
		log.Warn(r.ctx, "Error validating library.path", "path", library.Path, err)
		return fmt.Errorf("resources.library.validation.pathInvalid")
	}

	// Check if root directory exists
	info, err := fs.Stat(fsys, ".")
	if err != nil {
		// Parse the error message to check for "not a directory"
		log.Warn(r.ctx, "Error stating library.path", "path", library.Path, err)
		errStr := err.Error()
		if strings.Contains(errStr, "not a directory") ||
			strings.Contains(errStr, "The directory name is invalid.") {
			return fmt.Errorf("resources.library.validation.pathNotDirectory")
		} else if os.IsNotExist(err) {
			return fmt.Errorf("resources.library.validation.pathNotFound")
		} else if os.IsPermission(err) {
			return fmt.Errorf("resources.library.validation.pathNotAccessible")
		} else {
			return fmt.Errorf("resources.library.validation.pathInvalid")
		}
	}

	if !info.IsDir() {
		return fmt.Errorf("resources.library.validation.pathNotDirectory")
	}

	return nil
}

// validateLibraryIDs 校验给定的库 ID 是否全部存在。
// 用一次 COUNT 比对数量，避免逐个查询。
func (s *libraryService) validateLibraryIDs(ctx context.Context, libraryIDs []int) error {
	if len(libraryIDs) == 0 {
		return nil
	}

	// Use CountAll to efficiently validate library IDs exist
	count, err := s.ds.Library(ctx).CountAll(model.QueryOptions{
		Filters: squirrel.Eq{"id": libraryIDs},
	})
	if err != nil {
		return fmt.Errorf("error validating library IDs: %w", err)
	}

	if int(count) != len(libraryIDs) {
		return fmt.Errorf("%w: one or more library IDs are invalid", model.ErrValidation)
	}

	return nil
}

// triggerScan 在后台协程中触发一次快速扫描，action 仅用于日志描述场景。
func (r *libraryRepositoryWrapper) triggerScan(lib *model.Library, action string) {
	log.Info(r.ctx, fmt.Sprintf("Triggering scan for %s library", action), "libraryID", lib.ID, "name", lib.Name, "path", lib.Path)
	start := time.Now()
	warnings, err := r.scanner.ScanAll(r.ctx, false) // Quick scan for new library
	if err != nil {
		log.Error(r.ctx, fmt.Sprintf("Error scanning %s library", action), "libraryID", lib.ID, "name", lib.Name, err)
	} else {
		log.Info(r.ctx, fmt.Sprintf("Scan completed for %s library", action), "libraryID", lib.ID, "name", lib.Name, "warnings", len(warnings), "elapsed", time.Since(start))
	}
}
