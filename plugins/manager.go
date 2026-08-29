package plugins

//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative api/api.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/http/http.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/config/config.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/websocket/websocket.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/scheduler/scheduler.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/cache/cache.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/artwork/artwork.proto
//go:generate protoc --go-plugin_out=. --go-plugin_opt=paths=source_relative host/subsonicapi/subsonicapi.proto

import (
	"fmt"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/navidrome/navidrome/conf"
	"github.com/navidrome/navidrome/core/agents"
	"github.com/navidrome/navidrome/core/metrics"
	"github.com/navidrome/navidrome/core/scrobbler"
	"github.com/navidrome/navidrome/log"
	"github.com/navidrome/navidrome/model"
	"github.com/navidrome/navidrome/plugins/api"
	"github.com/navidrome/navidrome/plugins/schema"
	"github.com/navidrome/navidrome/utils/singleton"
	"github.com/navidrome/navidrome/utils/slice"
	"github.com/tetratelabs/wazero"
)

// 插件可声明的能力类型，对应 manifest 中的 capabilities 字段。
const (
	CapabilityMetadataAgent       = "MetadataAgent"
	CapabilityScrobbler           = "Scrobbler"
	CapabilitySchedulerCallback   = "SchedulerCallback"
	CapabilityWebSocketCallback   = "WebSocketCallback"
	CapabilityLifecycleManagement = "LifecycleManagement"
)

// pluginCreators maps capability types to their respective creator functions
// pluginConstructor 是各能力适配器的构造函数签名。
type pluginConstructor func(wasmPath, pluginID string, m *managerImpl, runtime api.WazeroNewRuntime, mc wazero.ModuleConfig) WasmPlugin

// pluginCreators 按能力类型分发适配器构造函数。
// LifecycleManagement 不在其中：它只是初始化钩子，不对外暴露调用接口。
var pluginCreators = map[string]pluginConstructor{
	CapabilityMetadataAgent:     newWasmMediaAgent,
	CapabilityScrobbler:         newWasmScrobblerPlugin,
	CapabilitySchedulerCallback: newWasmSchedulerCallback,
	CapabilityWebSocketCallback: newWasmWebSocketCallback,
}

// WasmPlugin is the base interface that all WASM plugins implement
type WasmPlugin interface {
	// PluginID returns the unique identifier of the plugin (folder name)
	PluginID() string
}

// plugin 保存一个已注册插件的元信息与运行时配置。
// WASM 编译耗时较长，故放到后台进行，compilationReady 用于让调用方等待编译完成。
type plugin struct {
	ID               string
	Path             string
	Capabilities     []string
	WasmPath         string
	Manifest         *schema.PluginManifest // Loaded manifest
	Runtime          api.WazeroNewRuntime
	ModConfig        wazero.ModuleConfig
	compilationReady chan struct{}
	compilationErr   error
}

// waitForCompilation 阻塞等待插件编译完成。
// 设超时是为了避免某个插件编译卡死时拖垮所有调用方。
func (p *plugin) waitForCompilation() error {
	timeout := pluginCompilationTimeout()
	select {
	case <-p.compilationReady:
	case <-time.After(timeout):
		err := fmt.Errorf("timed out waiting for plugin %s to compile", p.ID)
		log.Error("Timed out waiting for plugin compilation", "name", p.ID, "path", p.WasmPath, "timeout", timeout, "err", err)
		return err
	}
	if p.compilationErr != nil {
		log.Error("Failed to compile plugin", "name", p.ID, "path", p.WasmPath, p.compilationErr)
	}
	return p.compilationErr
}

// SubsonicRouter 让插件可以回调服务端自身的 Subsonic API。
type SubsonicRouter http.Handler

// Manager 管理插件的发现、编译、注册与加载。
type Manager interface {
	SetSubsonicRouter(router SubsonicRouter)
	EnsureCompiled(name string) error
	PluginList() map[string]schema.PluginManifest
	PluginNames(capability string) []string
	LoadPlugin(name string, capability string) WasmPlugin
	LoadMediaAgent(name string) (agents.Interface, bool)
	LoadScrobbler(name string) (scrobbler.Scrobbler, bool)
	ScanPlugins()
}

// managerImpl is a singleton that manages plugins
type managerImpl struct {
	plugins          map[string]*plugin             // Map of plugin folder name to plugin info
	pluginsMu        sync.RWMutex                   // Protects plugins map
	subsonicRouter   atomic.Pointer[SubsonicRouter] // Subsonic API router
	schedulerService *schedulerService              // Service for handling scheduled tasks
	websocketService *websocketService              // Service for handling WebSocket connections
	lifecycle        *pluginLifecycleManager        // Manages plugin lifecycle and initialization
	adapters         map[string]WasmPlugin          // Map of plugin folder name + capability to adapter
	ds               model.DataStore                // DataStore for accessing persistent data
	metrics          metrics.Metrics
}

// GetManager returns the singleton instance of managerImpl
// GetManager 返回插件管理器单例。未启用插件时返回空实现，
// 让调用方无需到处判断开关。
func GetManager(ds model.DataStore, metrics metrics.Metrics) Manager {
	if !conf.Server.Plugins.Enabled {
		return &noopManager{}
	}
	return singleton.GetInstance(func() *managerImpl {
		return createManager(ds, metrics)
	})
}

// createManager creates a new managerImpl instance. Used in tests
func createManager(ds model.DataStore, metrics metrics.Metrics) *managerImpl {
	m := &managerImpl{
		plugins:   make(map[string]*plugin),
		lifecycle: newPluginLifecycleManager(metrics),
		ds:        ds,
		metrics:   metrics,
	}

	// Create the host services
	m.schedulerService = newSchedulerService(m)
	m.websocketService = newWebsocketService(m)

	return m
}

// SetSubsonicRouter sets the SubsonicRouter after managerImpl initialization
// SetSubsonicRouter 在管理器创建后再注入路由，解决二者的循环依赖。
// 用原子指针存放，因为注入与插件调用可能并发发生。
func (m *managerImpl) SetSubsonicRouter(router SubsonicRouter) {
	m.subsonicRouter.Store(&router)
}

// registerPlugin adds a plugin to the registry with the given parameters
// Used internally by ScanPlugins to register plugins
//
// registerPlugin 注册插件并为其每个能力创建适配器。
// 每个插件拥有独立的 wazero 运行时，宿主函数按 manifest 声明的权限裁剪，
// 未授权的能力对该插件根本不存在。
// 插件目录是符号链接则视为开发模式，仅用于日志区分。
func (m *managerImpl) registerPlugin(pluginID, pluginDir, wasmPath string, manifest *schema.PluginManifest) *plugin {
	// Create custom runtime function
	customRuntime := m.createRuntime(pluginID, manifest.Permissions)

	// Configure module and determine plugin name
	mc := newWazeroModuleConfig()

	// Check if it's a symlink, indicating development mode
	isSymlink := false
	if fileInfo, err := os.Lstat(pluginDir); err == nil {
		isSymlink = fileInfo.Mode()&os.ModeSymlink != 0
	}

	// Store plugin info
	p := &plugin{
		ID:               pluginID,
		Path:             pluginDir,
		Capabilities:     slice.Map(manifest.Capabilities, func(cap schema.PluginManifestCapabilitiesElem) string { return string(cap) }),
		WasmPath:         wasmPath,
		Manifest:         manifest,
		Runtime:          customRuntime,
		ModConfig:        mc,
		compilationReady: make(chan struct{}),
	}

	// Register the plugin first
	m.pluginsMu.Lock()
	m.plugins[pluginID] = p

	// Register one plugin adapter for each capability
	for _, capability := range manifest.Capabilities {
		capabilityStr := string(capability)
		constructor := pluginCreators[capabilityStr]
		if constructor == nil {
			// Warn about unknown capabilities, except for LifecycleManagement (it does not have an adapter)
			if capability != CapabilityLifecycleManagement {
				log.Warn("Unknown plugin capability type", "capability", capability, "plugin", pluginID)
			}
			continue
		}
		adapter := constructor(wasmPath, pluginID, m, customRuntime, mc)
		if adapter == nil {
			log.Error("Failed to create plugin adapter", "plugin", pluginID, "capability", capabilityStr, "path", wasmPath)
			continue
		}
		m.adapters[pluginID+"_"+capabilityStr] = adapter
	}
	m.pluginsMu.Unlock()

	log.Info("Discovered plugin", "folder", pluginID, "name", manifest.Name, "capabilities", manifest.Capabilities, "wasm", wasmPath, "dev_mode", isSymlink)
	return m.plugins[pluginID]
}

// initializePluginIfNeeded calls OnInit on plugins that implement LifecycleManagement
// initializePluginIfNeeded 调用插件的 OnInit。
// 初始化失败即注销该插件：带着半初始化状态运行只会产生难以排查的问题。
func (m *managerImpl) initializePluginIfNeeded(plugin *plugin) {
	// Skip if already initialized
	if m.lifecycle.isInitialized(plugin) {
		return
	}

	// Check if the plugin implements LifecycleManagement
	if slices.Contains(plugin.Manifest.Capabilities, CapabilityLifecycleManagement) {
		if err := m.lifecycle.callOnInit(plugin); err != nil {
			m.unregisterPlugin(plugin.ID)
		}
	}
}

// unregisterPlugin removes a plugin from the manager
// unregisterPlugin 注销插件并清理其适配器与初始化状态。
func (m *managerImpl) unregisterPlugin(pluginID string) {
	m.pluginsMu.Lock()
	defer m.pluginsMu.Unlock()

	plugin, ok := m.plugins[pluginID]
	if !ok {
		return
	}

	// Clear initialization state from lifecycle manager
	m.lifecycle.clearInitialized(plugin)

	// Unregister plugin adapters
	for _, capability := range plugin.Manifest.Capabilities {
		delete(m.adapters, pluginID+"_"+string(capability))
	}

	// Unregister plugin
	delete(m.plugins, pluginID)
	log.Info("Unregistered plugin", "plugin", pluginID)
}

// ScanPlugins scans the plugins directory, discovers all valid plugins, and registers them for use.
//
// ScanPlugins 重新扫描插件目录。
// 编译与初始化放到后台 goroutine，避免阻塞启动流程；
// 但必须等全部注册完成后再启动，否则初始化失败触发的注销会与注册竞争。
func (m *managerImpl) ScanPlugins() {
	// Clear existing plugins
	m.pluginsMu.Lock()
	m.plugins = make(map[string]*plugin)
	m.adapters = make(map[string]WasmPlugin)
	m.pluginsMu.Unlock()

	// Get plugins directory from config
	root := conf.Server.Plugins.Folder
	log.Debug("Scanning plugins folder", "root", root)

	// Fail fast if the compilation cache cannot be initialized
	_, err := getCompilationCache()
	if err != nil {
		log.Error("Failed to initialize plugins compilation cache. Disabling plugins", err)
		return
	}

	// Discover all plugins using the shared discovery function
	discoveries := DiscoverPlugins(root)

	var validPluginNames []string
	var registeredPlugins []*plugin
	for _, discovery := range discoveries {
		if discovery.Error != nil {
			// Handle global errors (like directory read failure)
			if discovery.ID == "" {
				log.Error("Plugin discovery failed", discovery.Error)
				return
			}
			// Handle individual plugin errors
			log.Error("Failed to process plugin", "plugin", discovery.ID, discovery.Error)
			continue
		}

		// Log discovery details
		log.Debug("Processing entry", "name", discovery.ID, "isSymlink", discovery.IsSymlink)
		if discovery.IsSymlink {
			log.Debug("Processing symlinked plugin directory", "name", discovery.ID, "target", discovery.Path)
		}
		log.Debug("Checking for plugin.wasm", "wasmPath", discovery.WasmPath)
		log.Debug("Manifest loaded successfully", "folder", discovery.ID, "name", discovery.Manifest.Name, "capabilities", discovery.Manifest.Capabilities)

		validPluginNames = append(validPluginNames, discovery.ID)

		// Register the plugin
		plugin := m.registerPlugin(discovery.ID, discovery.Path, discovery.WasmPath, discovery.Manifest)
		if plugin != nil {
			registeredPlugins = append(registeredPlugins, plugin)
		}
	}

	// Start background processing for all registered plugins after registration is complete
	// This avoids race conditions between registration and goroutines that might unregister plugins
	for _, p := range registeredPlugins {
		go func(plugin *plugin) {
			precompilePlugin(plugin)
			// Check if this plugin implements InitService and hasn't been initialized yet
			m.initializePluginIfNeeded(plugin)
		}(p)
	}

	log.Debug("Found valid plugins", "count", len(validPluginNames), "plugins", validPluginNames)
}

// PluginList returns a map of all registered plugins with their manifests
// PluginList 返回所有已注册插件及其 manifest。
func (m *managerImpl) PluginList() map[string]schema.PluginManifest {
	m.pluginsMu.RLock()
	defer m.pluginsMu.RUnlock()

	// Create a map to hold the plugin manifests
	pluginList := make(map[string]schema.PluginManifest, len(m.plugins))
	for name, plugin := range m.plugins {
		// Use the plugin ID as the key and the manifest as the value
		pluginList[name] = *plugin.Manifest
	}
	return pluginList
}

// PluginNames returns the folder names of all plugins that implement the specified capability
// PluginNames 返回实现了指定能力的插件名。
func (m *managerImpl) PluginNames(capability string) []string {
	m.pluginsMu.RLock()
	defer m.pluginsMu.RUnlock()

	var names []string
	for name, plugin := range m.plugins {
		for _, c := range plugin.Manifest.Capabilities {
			if string(c) == capability {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

// getPlugin 同时取出插件信息与对应能力的适配器。
func (m *managerImpl) getPlugin(name string, capability string) (*plugin, WasmPlugin, error) {
	m.pluginsMu.RLock()
	defer m.pluginsMu.RUnlock()
	info, infoOk := m.plugins[name]
	adapter, adapterOk := m.adapters[name+"_"+capability]

	if !infoOk {
		return nil, nil, fmt.Errorf("plugin not registered: %s", name)
	}
	if !adapterOk {
		return nil, nil, fmt.Errorf("plugin adapter not registered: %s, capability: %s", name, capability)
	}
	return info, adapter, nil
}

// LoadPlugin instantiates and returns a plugin by folder name
// LoadPlugin 按名称与能力取出插件适配器，必要时等待编译完成。
// 出错只记日志并返回 nil：单个插件不可用不应影响主流程。
func (m *managerImpl) LoadPlugin(name string, capability string) WasmPlugin {
	info, adapter, err := m.getPlugin(name, capability)
	if err != nil {
		log.Warn("Error loading plugin", err)
		return nil
	}

	log.Debug("Loading plugin", "name", name, "path", info.Path)

	// Wait for the plugin to be ready before using it.
	if err := info.waitForCompilation(); err != nil {
		log.Error("Plugin is not ready, cannot be loaded", "plugin", name, "capability", capability, "err", err)
		return nil
	}

	if adapter == nil {
		log.Warn("Plugin adapter not found", "name", name, "capability", capability)
		return nil
	}
	return adapter
}

// EnsureCompiled waits for a plugin to finish compilation and returns any compilation error.
// This is useful when you need to wait for compilation without loading a specific capability,
// such as during plugin refresh operations or health checks.
func (m *managerImpl) EnsureCompiled(name string) error {
	m.pluginsMu.RLock()
	plugin, ok := m.plugins[name]
	m.pluginsMu.RUnlock()

	if !ok {
		return fmt.Errorf("plugin not found: %s", name)
	}

	return plugin.waitForCompilation()
}

// LoadMediaAgent instantiates and returns a media agent plugin by folder name
func (m *managerImpl) LoadMediaAgent(name string) (agents.Interface, bool) {
	plugin := m.LoadPlugin(name, CapabilityMetadataAgent)
	if plugin == nil {
		return nil, false
	}
	agent, ok := plugin.(*wasmMediaAgent)
	return agent, ok
}

// LoadScrobbler instantiates and returns a scrobbler plugin by folder name
func (m *managerImpl) LoadScrobbler(name string) (scrobbler.Scrobbler, bool) {
	plugin := m.LoadPlugin(name, CapabilityScrobbler)
	if plugin == nil {
		return nil, false
	}
	s, ok := plugin.(scrobbler.Scrobbler)
	return s, ok
}

// noopManager 是插件功能关闭时的空实现。
type noopManager struct{}

func (n noopManager) SetSubsonicRouter(router SubsonicRouter) {}

func (n noopManager) EnsureCompiled(name string) error { return nil }

func (n noopManager) PluginList() map[string]schema.PluginManifest { return nil }

func (n noopManager) PluginNames(capability string) []string { return nil }

func (n noopManager) LoadPlugin(name string, capability string) WasmPlugin { return nil }

func (n noopManager) LoadMediaAgent(name string) (agents.Interface, bool) { return nil, false }

func (n noopManager) LoadScrobbler(name string) (scrobbler.Scrobbler, bool) { return nil, false }

func (n noopManager) ScanPlugins() {}
