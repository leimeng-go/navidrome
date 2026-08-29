package plugins

import (
	"context"
	"crypto/md5"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/plugins/api"
	"github.com/navidrome/navidrome/plugins/host/artwork"
	"github.com/navidrome/navidrome/plugins/host/cache"
	"github.com/navidrome/navidrome/plugins/host/config"
	"github.com/navidrome/navidrome/plugins/host/http"
	"github.com/navidrome/navidrome/plugins/host/scheduler"
	"github.com/navidrome/navidrome/plugins/host/subsonicapi"
	"github.com/navidrome/navidrome/plugins/host/websocket"
	"github.com/navidrome/navidrome/plugins/schema"
	"github.com/tetratelabs/wazero"
	wazeroapi "github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// maxParallelCompilations 限制并发编译数：WASM 编译很吃 CPU，
// 放开并发会在启动时抢占扫描与请求处理的资源。
const maxParallelCompilations = 2 // Limit to 2 concurrent compilations

var (
	compileSemaphore = make(chan struct{}, maxParallelCompilations)
	compilationCache wazero.CompilationCache
	cacheOnce        sync.Once
	runtimePool      sync.Map // map[string]*cachingRuntime
)

// createRuntime returns a function that creates a new wazero runtime and instantiates the required host functions
// based on the given plugin permissions
//
// createRuntime 返回创建运行时的工厂函数。
// 每个插件共享一个运行时（编译结果与实例池可复用），
// 但每次调用都套一层 scopedRuntime，使 Close 只关闭本次的模块实例而非整个运行时。
// 用 LoadOrStore 原子写入，避免并发首次创建时产生多个运行时。
func (m *managerImpl) createRuntime(pluginID string, permissions schema.PluginManifestPermissions) api.WazeroNewRuntime {
	return func(ctx context.Context) (wazero.Runtime, error) {
		// Check if runtime already exists
		if rt, ok := runtimePool.Load(pluginID); ok {
			log.Trace(ctx, "Using existing runtime", "plugin", pluginID, "runtime", fmt.Sprintf("%p", rt))
			// Return a new wrapper for each call, so each instance gets its own module capture
			return newScopedRuntime(rt.(wazero.Runtime)), nil
		}

		// Create new runtime with all the setup
		cachingRT, err := m.createCachingRuntime(ctx, pluginID, permissions)
		if err != nil {
			return nil, err
		}

		// Use LoadOrStore to atomically check and store, preventing race conditions
		if existing, loaded := runtimePool.LoadOrStore(pluginID, cachingRT); loaded {
			// Another goroutine created the runtime first, close ours and return the existing one
			log.Trace(ctx, "Race condition detected, using existing runtime", "plugin", pluginID, "runtime", fmt.Sprintf("%p", existing))
			_ = cachingRT.Close(ctx)
			return newScopedRuntime(existing.(wazero.Runtime)), nil
		}

		log.Trace(ctx, "Created new runtime", "plugin", pluginID, "runtime", fmt.Sprintf("%p", cachingRT))
		return newScopedRuntime(cachingRT), nil
	}
}

// createCachingRuntime handles the complex logic of setting up a new cachingRuntime
// createCachingRuntime 创建带编译缓存与实例池的运行时，并装载宿主服务。
func (m *managerImpl) createCachingRuntime(ctx context.Context, pluginID string, permissions schema.PluginManifestPermissions) (*cachingRuntime, error) {
	// Get compilation cache
	compCache, err := getCompilationCache()
	if err != nil {
		return nil, fmt.Errorf("failed to get compilation cache: %w", err)
	}

	// Create the runtime
	runtimeConfig := wazero.NewRuntimeConfig().WithCompilationCache(compCache)
	r := wazero.NewRuntimeWithConfig(ctx, runtimeConfig)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, r); err != nil {
		return nil, err
	}

	// Setup host services
	if err := m.setupHostServices(ctx, r, pluginID, permissions); err != nil {
		_ = r.Close(ctx)
		return nil, err
	}

	return newCachingRuntime(r, pluginID), nil
}

// setupHostServices configures all the permitted host services for a plugin
//
// setupHostServices 按 manifest 声明的权限装载宿主服务。
// 这是插件沙箱的核心：未授权的服务根本不会注入到运行时，
// 插件即便调用也只会得到「函数不存在」的链接错误。
func (m *managerImpl) setupHostServices(ctx context.Context, r wazero.Runtime, pluginID string, permissions schema.PluginManifestPermissions) error {
	// Define all available host services
	type hostService struct {
		name        string
		isPermitted bool
		loadFunc    func() (map[string]wazeroapi.FunctionDefinition, error)
	}

	// List of all available host services with their permissions and loading functions
	availableServices := []hostService{
		{"config", permissions.Config != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			return loadHostLibrary[config.ConfigService](ctx, config.Instantiate, &configServiceImpl{pluginID: pluginID})
		}},
		{"scheduler", permissions.Scheduler != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			return loadHostLibrary[scheduler.SchedulerService](ctx, scheduler.Instantiate, m.schedulerService.HostFunctions(pluginID))
		}},
		{"cache", permissions.Cache != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			return loadHostLibrary[cache.CacheService](ctx, cache.Instantiate, newCacheService(pluginID))
		}},
		{"artwork", permissions.Artwork != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			return loadHostLibrary[artwork.ArtworkService](ctx, artwork.Instantiate, &artworkServiceImpl{})
		}},
		{"http", permissions.Http != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			httpPerms, err := parseHTTPPermissions(permissions.Http)
			if err != nil {
				return nil, fmt.Errorf("invalid http permissions for plugin %s: %w", pluginID, err)
			}
			return loadHostLibrary[http.HttpService](ctx, http.Instantiate, &httpServiceImpl{
				pluginID:    pluginID,
				permissions: httpPerms,
			})
		}},
		{"websocket", permissions.Websocket != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			wsPerms, err := parseWebSocketPermissions(permissions.Websocket)
			if err != nil {
				return nil, fmt.Errorf("invalid websocket permissions for plugin %s: %w", pluginID, err)
			}
			return loadHostLibrary[websocket.WebSocketService](ctx, websocket.Instantiate, m.websocketService.HostFunctions(pluginID, wsPerms))
		}},
		{"subsonicapi", permissions.Subsonicapi != nil, func() (map[string]wazeroapi.FunctionDefinition, error) {
			if router := m.subsonicRouter.Load(); router != nil {
				service := newSubsonicAPIService(pluginID, m.subsonicRouter.Load(), m.ds, permissions.Subsonicapi)
				return loadHostLibrary[subsonicapi.SubsonicAPIService](ctx, subsonicapi.Instantiate, service)
			}
			log.Error(ctx, "SubsonicAPI service requested but router not available", "plugin", pluginID)
			return nil, fmt.Errorf("SubsonicAPI router not available for plugin %s", pluginID)
		}},
	}

	// Load only permitted services
	var grantedPermissions []string
	var libraries []map[string]wazeroapi.FunctionDefinition
	for _, service := range availableServices {
		if service.isPermitted {
			lib, err := service.loadFunc()
			if err != nil {
				return fmt.Errorf("error loading %s lib: %w", service.name, err)
			}
			libraries = append(libraries, lib)
			grantedPermissions = append(grantedPermissions, service.name)
		}
	}
	log.Trace(ctx, "Granting permissions for plugin", "plugin", pluginID, "permissions", grantedPermissions)

	// Combine the permitted libraries
	return combineLibraries(ctx, r, libraries...)
}

// purgeCacheBySize removes the oldest files in dir until its total size is
// lower than or equal to maxSize. maxSize should be a human-readable string
// like "10MB" or "200K". If parsing fails or maxSize is "0", the function is
// a no-op.
//
// purgeCacheBySize 按 LRU（修改时间）清理编译缓存目录直至不超过上限。
// 遍历中的单个文件错误只记日志跳过，避免个别坏文件导致整体清理失败。
// 删完文件后顺带清空目录，防止缓存目录堆积大量空壳。
func purgeCacheBySize(dir, maxSize string) {
	sizeLimit, err := humanize.ParseBytes(maxSize)
	if err != nil || sizeLimit == 0 {
		return
	}

	type fileInfo struct {
		path string
		size uint64
		mod  int64
	}

	var files []fileInfo
	var total uint64

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			log.Trace("Failed to access plugin cache entry", "path", path, err)
			return nil //nolint:nilerr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			log.Trace("Failed to get file info for plugin cache entry", "path", path, err)
			return nil //nolint:nilerr
		}
		files = append(files, fileInfo{
			path: path,
			size: uint64(info.Size()),
			mod:  info.ModTime().UnixMilli(),
		})
		total += uint64(info.Size())
		return nil
	}

	if err := filepath.WalkDir(dir, walk); err != nil {
		if !os.IsNotExist(err) {
			log.Warn("Failed to traverse plugin cache directory", "path", dir, err)
		}
		return
	}

	log.Trace("Current plugin cache size", "path", dir, "size", humanize.Bytes(total), "sizeLimit", humanize.Bytes(sizeLimit))
	if total <= sizeLimit {
		return
	}

	log.Debug("Purging plugin cache", "path", dir, "sizeLimit", humanize.Bytes(sizeLimit), "currentSize", humanize.Bytes(total))
	sort.Slice(files, func(i, j int) bool { return files[i].mod < files[j].mod })
	for _, f := range files {
		if total <= sizeLimit {
			break
		}
		if err := os.Remove(f.path); err != nil {
			log.Warn("Failed to remove plugin cache entry", "path", f.path, "size", humanize.Bytes(f.size), err)
			continue
		}
		total -= f.size
		log.Debug("Removed plugin cache entry", "path", f.path, "size", humanize.Bytes(f.size), "time", time.UnixMilli(f.mod), "remainingSize", humanize.Bytes(total))

		// Remove empty parent directories
		dirPath := filepath.Dir(f.path)
		for dirPath != dir {
			if err := os.Remove(dirPath); err != nil {
				break
			}
			dirPath = filepath.Dir(dirPath)
		}
	}
}

// getCompilationCache returns the global compilation cache, creating it if necessary
// getCompilationCache 返回全局编译缓存，首次使用时先做一次容量清理。
func getCompilationCache() (wazero.CompilationCache, error) {
	var err error
	cacheOnce.Do(func() {
		cacheDir := filepath.Join(conf.Server.CacheFolder, "plugins")
		purgeCacheBySize(cacheDir, conf.Server.Plugins.CacheSize)
		compilationCache, err = wazero.NewCompilationCacheWithDir(cacheDir)
	})
	return compilationCache, err
}

// newWazeroModuleConfig creates the correct ModuleConfig for plugins
// newWazeroModuleConfig 配置模块：用 _initialize 而非 _start 作为入口
// （插件是响应式库而非独立程序），并把插件的 stderr 接到日志系统。
func newWazeroModuleConfig() wazero.ModuleConfig {
	return wazero.NewModuleConfig().WithStartFunctions("_initialize").WithStderr(log.Writer())
}

// pluginCompilationTimeout returns the timeout for plugin compilation
// pluginCompilationTimeout 返回编译超时，开发环境可覆盖。
func pluginCompilationTimeout() time.Duration {
	if conf.Server.DevPluginCompilationTimeout > 0 {
		return conf.Server.DevPluginCompilationTimeout
	}
	return time.Minute
}

// precompilePlugin compiles the WASM module in the background and updates the pluginState.
//
// precompilePlugin 后台预编译插件。
// 无论成功失败都必须关闭 compilationReady，否则等待方会一直阻塞到超时。
// 通过信号量限制并发编译数量。
func precompilePlugin(p *plugin) {
	compileSemaphore <- struct{}{}
	defer func() { <-compileSemaphore }()
	ctx := context.Background()
	r, err := p.Runtime(ctx)
	if err != nil {
		p.compilationErr = fmt.Errorf("failed to create runtime for plugin %s: %w", p.ID, err)
		close(p.compilationReady)
		return
	}

	b, err := os.ReadFile(p.WasmPath)
	if err != nil {
		p.compilationErr = fmt.Errorf("failed to read wasm file: %w", err)
		close(p.compilationReady)
		return
	}

	// We know r is always a *scopedRuntime from createRuntime
	scopedRT := r.(*scopedRuntime)
	cachingRT := scopedRT.GetCachingRuntime()
	if cachingRT == nil {
		p.compilationErr = fmt.Errorf("failed to get cachingRuntime for plugin %s", p.ID)
		close(p.compilationReady)
		return
	}

	_, err = cachingRT.CompileModule(ctx, b)
	if err != nil {
		p.compilationErr = fmt.Errorf("failed to compile WASM for plugin %s: %w", p.ID, err)
		log.Warn("Plugin compilation failed", "name", p.ID, "path", p.WasmPath, "err", err)
	} else {
		p.compilationErr = nil
		log.Debug("Plugin compilation completed", "name", p.ID, "path", p.WasmPath)
	}
	close(p.compilationReady)
}

// loadHostLibrary loads the given host library and returns its exported functions
//
// loadHostLibrary 借一个临时运行时把宿主服务实例化，只为取出其函数定义。
// 因为生成代码固定注册到 env 模块，多个服务无法直接共存于同一运行时，
// 只能先分别提取再由 combineLibraries 合并。
func loadHostLibrary[S any](
	ctx context.Context,
	instantiateFn func(context.Context, wazero.Runtime, S) error,
	service S,
) (map[string]wazeroapi.FunctionDefinition, error) {
	r := wazero.NewRuntime(ctx)
	if err := instantiateFn(ctx, r, service); err != nil {
		return nil, err
	}
	m := r.Module("env")
	return m.ExportedFunctionDefinitions(), nil
}

// combineLibraries combines the given host libraries into a single "env" module
// combineLibraries 把各宿主服务的函数合并成单个 env 模块，
// 因为 WASM 模块只能导入一个同名宿主模块。
func combineLibraries(ctx context.Context, r wazero.Runtime, libs ...map[string]wazeroapi.FunctionDefinition) error {
	// Merge the libraries
	hostLib := map[string]wazeroapi.FunctionDefinition{}
	for _, lib := range libs {
		maps.Copy(hostLib, lib)
	}

	// Create the combined host module
	envBuilder := r.NewHostModuleBuilder("env")
	for name, fd := range hostLib {
		fn, ok := fd.GoFunction().(wazeroapi.GoModuleFunction)
		if !ok {
			return fmt.Errorf("invalid function definition: %s", fd.DebugName())
		}
		envBuilder.NewFunctionBuilder().
			WithGoModuleFunction(fn, fd.ParamTypes(), fd.ResultTypes()).
			WithParameterNames(fd.ParamNames()...).Export(name)
	}

	// Instantiate the combined host module
	if _, err := envBuilder.Instantiate(ctx); err != nil {
		return err
	}
	return nil
}

const (
	// WASM Instance pool configuration
	// defaultPoolSize is the maximum number of instances per plugin that are kept in the pool for reuse
	defaultPoolSize = 8
	// defaultInstanceTTL is the time after which an instance is considered stale and can be evicted
	defaultInstanceTTL = time.Minute
	// defaultMaxConcurrentInstances is the hard limit on total instances that can exist simultaneously
	defaultMaxConcurrentInstances = 10
	// defaultGetTimeout is the maximum time to wait when getting an instance if at the concurrent limit
	defaultGetTimeout = 5 * time.Second

	// Compiled module cache configuration
	// defaultCompiledModuleTTL is the time after which a compiled module is evicted from the cache
	defaultCompiledModuleTTL = 5 * time.Minute
)

// cachedCompiledModule encapsulates a compiled WebAssembly module with TTL management
// cachedCompiledModule 缓存编译结果并按 TTL 过期。
// 编译产物占用内存较大，长期不用的插件应及时释放。
// 以 WASM 字节的 MD5 作为键，插件文件更新后自动失效。
type cachedCompiledModule struct {
	module     wazero.CompiledModule
	hash       [16]byte
	lastAccess time.Time
	timer      *time.Timer
	mu         sync.Mutex
	pluginID   string // for logging purposes
}

// newCachedCompiledModule creates a new cached compiled module with TTL management
// newCachedCompiledModule 创建带 TTL 定时器的缓存项。
func newCachedCompiledModule(module wazero.CompiledModule, wasmBytes []byte, pluginID string) *cachedCompiledModule {
	c := &cachedCompiledModule{
		module:     module,
		hash:       md5.Sum(wasmBytes),
		lastAccess: time.Now(),
		pluginID:   pluginID,
	}

	// Set up the TTL timer
	c.timer = time.AfterFunc(defaultCompiledModuleTTL, c.evict)

	return c
}

// get returns the cached module if the hash matches, nil otherwise
// Also resets the TTL timer on successful access
// get 命中时顺带续期，使活跃插件不会被 TTL 淘汰。
func (c *cachedCompiledModule) get(wasmHash [16]byte) wazero.CompiledModule {
	c.mu.Lock() // Use write lock because we modify state in resetTimer
	defer c.mu.Unlock()

	if c.module != nil && c.hash == wasmHash {
		// Reset TTL timer on access
		c.resetTimer()
		return c.module
	}

	return nil
}

// resetTimer resets the TTL timer (must be called with lock held)
func (c *cachedCompiledModule) resetTimer() {
	c.lastAccess = time.Now()

	if c.timer != nil {
		c.timer.Stop()
		c.timer = time.AfterFunc(defaultCompiledModuleTTL, c.evict)
	}
}

// evict removes the cached module and cleans up resources
func (c *cachedCompiledModule) evict() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.module != nil {
		log.Trace("cachedCompiledModule: evicting due to TTL expiry", "plugin", c.pluginID, "ttl", defaultCompiledModuleTTL)
		c.module.Close(context.Background())
		c.module = nil
		c.hash = [16]byte{}
		c.lastAccess = time.Time{}
	}

	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}

// close cleans up the cached module and stops the timer
func (c *cachedCompiledModule) close(ctx context.Context) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}

	if c.module != nil {
		c.module.Close(ctx)
		c.module = nil
	}
}

// pooledModule wraps a wazero Module and returns it to the pool when closed.
// pooledModule 把 Close 语义改为「归还实例池」，
// 使调用方沿用惯常的 defer Close 写法即可实现复用。
type pooledModule struct {
	wazeroapi.Module
	pool   *wasmInstancePool[wazeroapi.Module]
	closed bool
}

func (m *pooledModule) Close(ctx context.Context) error {
	if !m.closed {
		m.closed = true
		m.pool.Put(ctx, m.Module)
	}
	return nil
}

func (m *pooledModule) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	return m.Close(ctx)
}

func (m *pooledModule) IsClosed() bool {
	return m.closed
}

// newScopedRuntime creates a new scopedRuntime that wraps the given runtime
// newScopedRuntime 包装共享运行时。
func newScopedRuntime(runtime wazero.Runtime) *scopedRuntime {
	return &scopedRuntime{Runtime: runtime}
}

// scopedRuntime wraps a cachingRuntime and captures a specific module
// so that Close() only affects that module, not the entire shared runtime
type scopedRuntime struct {
	wazero.Runtime
	capturedModule wazeroapi.Module
}

func (w *scopedRuntime) InstantiateModule(ctx context.Context, code wazero.CompiledModule, config wazero.ModuleConfig) (wazeroapi.Module, error) {
	module, err := w.Runtime.InstantiateModule(ctx, code, config)
	if err != nil {
		return nil, err
	}
	// Capture the module for later cleanup
	w.capturedModule = module
	log.Trace(ctx, "scopedRuntime: captured module", "moduleID", getInstanceID(module))
	return module, nil
}

func (w *scopedRuntime) Close(ctx context.Context) error {
	// Close only the captured module, not the entire runtime
	if w.capturedModule != nil {
		log.Trace(ctx, "scopedRuntime: closing captured module", "moduleID", getInstanceID(w.capturedModule))
		return w.capturedModule.Close(ctx)
	}
	log.Trace(ctx, "scopedRuntime: no captured module to close")
	return nil
}

func (w *scopedRuntime) CloseWithExitCode(ctx context.Context, exitCode uint32) error {
	return w.Close(ctx)
}

// GetCachingRuntime returns the underlying cachingRuntime for internal use
// GetCachingRuntime 取出底层的缓存运行时，供预编译等内部流程使用。
func (w *scopedRuntime) GetCachingRuntime() *cachingRuntime {
	if cr, ok := w.Runtime.(*cachingRuntime); ok {
		return cr
	}
	return nil
}

// cachingRuntime wraps wazero.Runtime and pools module instances per plugin,
// while also caching the compiled module in memory.
type cachingRuntime struct {
	wazero.Runtime

	// pluginID is required to differentiate between different plugins that use the same file to initialize their
	// runtime. The runtime will serve as a singleton for all instances of a given plugin.
	pluginID string

	// cachedModule manages the compiled module cache with TTL
	cachedModule atomic.Pointer[cachedCompiledModule]

	// pool manages reusable module instances
	pool *wasmInstancePool[wazeroapi.Module]

	// poolInitOnce ensures the pool is initialized only once
	poolInitOnce sync.Once

	// compilationMu ensures only one compilation happens at a time per runtime
	compilationMu sync.Mutex
}

// newCachingRuntime 创建缓存运行时，实例池延迟到首次实例化时建立。
func newCachingRuntime(runtime wazero.Runtime, pluginID string) *cachingRuntime {
	return &cachingRuntime{
		Runtime:  runtime,
		pluginID: pluginID,
	}
}

// initPool 惰性初始化实例池：池的工厂函数依赖编译结果与模块配置，
// 只有到首次 InstantiateModule 时才具备。
func (r *cachingRuntime) initPool(code wazero.CompiledModule, config wazero.ModuleConfig) {
	r.poolInitOnce.Do(func() {
		r.pool = newWasmInstancePool[wazeroapi.Module](r.pluginID, defaultPoolSize, defaultMaxConcurrentInstances, defaultGetTimeout, defaultInstanceTTL, func(ctx context.Context) (wazeroapi.Module, error) {
			log.Trace(ctx, "cachingRuntime: creating new module instance", "plugin", r.pluginID)
			return r.Runtime.InstantiateModule(ctx, code, config)
		})
	})
}

func (r *cachingRuntime) InstantiateModule(ctx context.Context, code wazero.CompiledModule, config wazero.ModuleConfig) (wazeroapi.Module, error) {
	r.initPool(code, config)
	mod, err := r.pool.Get(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &pooledModule{Module: mod, pool: r.pool}
	log.Trace(ctx, "cachingRuntime: created wrapper for module", "plugin", r.pluginID, "underlyingModuleID", fmt.Sprintf("%p", mod), "wrapperID", fmt.Sprintf("%p", wrapped))
	return wrapped, nil
}

func (r *cachingRuntime) Close(ctx context.Context) error {
	log.Trace(ctx, "cachingRuntime: closing runtime", "plugin", r.pluginID)

	// Clean up compiled module cache
	if cached := r.cachedModule.Swap(nil); cached != nil {
		cached.close(ctx)
	}

	// Close the instance pool
	if r.pool != nil {
		r.pool.Close(ctx)
	}
	// Close the underlying runtime
	return r.Runtime.Close(ctx)
}

// setCachedModule stores a newly compiled module in the cache with TTL management
func (r *cachingRuntime) setCachedModule(module wazero.CompiledModule, wasmBytes []byte) {
	newCached := newCachedCompiledModule(module, wasmBytes, r.pluginID)

	// Replace old cached module and clean it up
	if old := r.cachedModule.Swap(newCached); old != nil {
		old.close(context.Background())
	}
}

// CompileModule checks if the provided bytes match our cached hash and returns
// the cached compiled module if so, avoiding both file read and compilation.
//
// CompileModule 采用双重检查：先无锁读缓存（命中是常态，避免锁竞争），
// 未命中再加锁并重查一次，防止并发下重复编译同一模块。
func (r *cachingRuntime) CompileModule(ctx context.Context, wasmBytes []byte) (wazero.CompiledModule, error) {
	incomingHash := md5.Sum(wasmBytes)

	// Try to get from cache first (without lock for performance)
	if cached := r.cachedModule.Load(); cached != nil {
		if module := cached.get(incomingHash); module != nil {
			log.Trace(ctx, "cachingRuntime: using cached compiled module", "plugin", r.pluginID)
			return module, nil
		}
	}

	// Synchronize compilation to prevent concurrent compilation issues
	r.compilationMu.Lock()
	defer r.compilationMu.Unlock()

	// Double-check cache after acquiring lock (another goroutine might have compiled it)
	if cached := r.cachedModule.Load(); cached != nil {
		if module := cached.get(incomingHash); module != nil {
			log.Trace(ctx, "cachingRuntime: using cached compiled module (after lock)", "plugin", r.pluginID)
			return module, nil
		}
	}

	// Fall back to normal compilation for different bytes
	log.Trace(ctx, "cachingRuntime: hash doesn't match cache, compiling normally", "plugin", r.pluginID)
	module, err := r.Runtime.CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, err
	}

	// Cache the newly compiled module
	r.setCachedModule(module, wasmBytes)

	return module, nil
}
