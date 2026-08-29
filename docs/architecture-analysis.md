# Navidrome 项目架构分析

> 分析时间：2026-08-28 · 分支 `master` · 基于代码知识图谱（9151 节点 / 28534 边）

## 一、项目定位

Navidrome 是一个开源的自托管音乐流媒体服务器，兼容 Subsonic/Airsonic/Madsonic 协议生态。
核心价值主张：把本地音乐库变成"私人 Spotify"，通过 Web UI 或任意 Subsonic 客户端访问。

- 单一二进制交付（Go + 内嵌前端资源），低资源占用
- SQLite 作为唯一数据库依赖，无外部中间件
- 多用户隔离（各自的播放次数、收藏、播放列表）
- 按需转码（ffmpeg），可按用户/播放器配置

## 二、技术栈

| 维度 | 选型 |
|---|---|
| 后端语言 | Go 1.25 |
| HTTP 框架 | go-chi/v5（+ cors、httprate、jwtauth） |
| SQL 构建 | Masterminds/squirrel（手写 SQL builder，非 ORM） |
| 数据库 | SQLite（`db/migrations` 管理迁移） |
| 依赖注入 | google/wire（编译期生成 `cmd/wire_gen.go`） |
| CLI | spf13/cobra + viper 配置 |
| 前端 | React + react-admin + Material-UI + Vite |
| 前端状态 | Redux（`ui/src/reducers`）+ 自定义 store |
| 音频标签 | taglib（cgo，`adapters/taglib`）+ dhowden/tag fork |
| 插件运行时 | wazero（WASM）+ Protocol Buffers |
| 测试 | Ginkgo/Gomega + cupaloy 快照 + Vitest（前端） |

文件规模：Go 503 个文件，JavaScript 253 个文件。

## 三、整体分层

```
main.go
  └── cmd/            CLI 入口（serve / scan / backup / user / plugin / pls / inspect）
        │             wire 装配全部依赖：cmd/wire_injectors.go
        ▼
     server/          HTTP 服务器 + 路由挂载 + 中间件 + 认证
        ├── subsonic/     Subsonic API 兼容层（33 个文件，协议主战场）
        ├── nativeapi/    自有 REST API（react-admin 数据源）
        ├── public/       公开分享端点（share / 公开封面）
        ├── events/       SSE 事件推送（扫描进度、库变更）
        └── backgrounds/  登录页背景图
        ▼
     core/            业务服务层
        ├── artwork/      封面获取与多级缓存、预热
        ├── agents/       外部元数据源：lastfm / spotify / deezer / listenbrainz
        ├── scrobbler/    听歌记录上报（含缓冲重试）
        ├── playback/     Jukebox 模式，通过 mpv 本地播放
        ├── ffmpeg/       转码进程封装
        ├── storage/      存储抽象 + local 实现 + 文件监听
        ├── lyrics/       歌词来源
        └── metrics/      Prometheus 指标 + insights 上报
        ▼
     model/           领域模型与接口定义
        ├── criteria/     智能播放列表查询 DSL（可序列化为 SQL）
        ├── metadata/     原始标签 → MediaFile/Album 的映射规则
        ├── datastore.go  仓储接口总集（DataStore）
        └── id/           ID 生成策略
        ▼
     persistence/     仓储实现（55 个文件）
        └── sql_base_repository.go   核心枢纽，被 30 个仓储复用
        ▼
     db/              连接、迁移、备份
```

横切模块：`log/`（logrus + 敏感信息脱敏 redactrus）、`conf/`（配置）、`consts/`、`utils/`（27 个工具子包）、`scheduler/`（cron 调度）、`resources/`（内嵌资源与 i18n）。

## 四、关键子系统

### 4.1 扫描器（`scanner/`）

四阶段流水线，基于泛型 `phase[T]` 接口与 pipeline 库（producer/stage）实现流式处理：

| 阶段 | 文件 | 职责 |
|---|---|---|
| Phase 1 | `phase_1_folders.go` | 遍历目录树（`walk_dir_tree.go`，支持 `.ndignore`），批量提取元数据（200 文件/批），生成 Album/Artist |
| Phase 2 | `phase_2_missing_tracks.go` | 通过 PID 匹配识别"移动过的文件"，只更新路径而非重建记录 |
| Phase 3 | `phase_3_refresh_albums.go` | 依据最新音轨元数据刷新专辑，更新播放统计 |
| Phase 4 | `phase_4_playlists.go` | 导入 M3U/NSP 播放列表，预热封面 |

- Phase 1→2 顺序执行；Phase 3、4 并行（`chain.RunParallel()`）
- 收尾：GC 清理孤立记录 → 统计刷新 → 库状态更新 → 数据库优化
- **外部扫描进程**（`external.go`）：默认开启，以 `navidrome scan --subprocess` 派生子进程，用 gob 编码通过管道回传进度，从根本上隔离扫描期的内存占用与泄漏风险
- **文件监听**（`watcher.go`）：每库一个 goroutine，事件节流合批后触发增量扫描

详细设计见 `scanner/README.md`。

### 4.2 插件系统（`plugins/`）

WASM 沙箱扩展机制，宿主与插件通过 protobuf 通信（`plugins/api/api.proto`）。

插件可实现的能力：

1. `MetadataAgent` — 补全艺人/专辑信息与图片
2. `Scrobbler` — 对接外部听歌记录服务
3. `SchedulerCallback` — 延时/周期任务
4. `WebSocketCallback` — WebSocket 交互
5. `LifecycleManagement` — 一次性 `OnInit` 初始化

宿主侧服务（`host_*.go`）：HTTP 请求、Cache（TTL）、Scheduler、Config、WebSocket、Artwork URL、SubsonicAPI 回调。每项均有权限清单校验（`manifest_permissions_test.go`、`host_http_permissions.go`）。

运行时特性：模块预编译并缓存于 `[CacheFolder]/plugins`，实例池化（默认上限 8、TTL 1 分钟，见 `wasm_instance_pool.go`）。

适配器把插件能力桥接到内部接口：`adapter_media_agent.go`、`adapter_scrobbler.go` 等。

详细文档见 `plugins/README.md`。

### 4.3 API 双轨

- **Subsonic API**（`server/subsonic/`）：对外兼容层，覆盖浏览、搜索、流媒体、书签、分享、播放列表、Jukebox、OpenSubsonic 扩展。响应结构集中在 `responses/`，请求过滤在 `filter/filters.go`。
- **Native API**（`server/nativeapi/`）：服务自家 Web UI，基于 `deluan/rest` 暴露仓储 CRUD，外加 library、queue、inspect、translations、missing 等专用端点。
- 路由在 `cmd/root.go:113` 起集中挂载，`server/server.go:53` 的 `MountRouter` 负责注册与日志。

### 4.4 前端（`ui/`）

react-admin 驱动的资源型后台结构：`album/`、`artist/`、`song/`、`playlist/`、`radio/`、`share/`、`user/`、`library/`、`missing/`。

- 播放器独立在 `audioplayer/` 与 `player/`，配合 `reducers/playerReducer.js`
- SSE 长连接在 `eventStream.js`，接收扫描进度与库更新
- 主题系统 `themes/`，i18n 由 POEditor 同步
- 快捷键 `hotkeys.js`，Service Worker `sw.js`

## 五、图谱度量与观察

聚类内聚度（Leiden 社区发现）：

| 社区 | 成员数 | 内聚度 |
|---|---|---|
| ui | 305 | 0.92 |
| ui（播放器/httpClient 簇） | 158 | 0.84 |
| persistence | 312 | 0.71 |
| model | 165 | 0.63 |
| server | 269 | 0.60 |
| core | 202 | 0.53 |
| scanner | 148 | 0.45 |
| server（中间件/错误处理簇） | 164 | 0.37 |

高扇入热点：`log.Error`(224)、`log.Debug`(167)、`model/participants.Join`(131)、`model/artist.ArtistRepository.Get`(79)、`model/metadata.New`(76)。

结论性观察：

- **持久化层内聚良好**：`sql_base_repository` 作为公共基类被广泛复用（fan-in 30），SQL 拼装、过滤、分页逻辑集中，改动面可控。
- **server 层跨包调用偏多**（内聚 0.37/0.60）：Subsonic handler 直接依赖 core 与 model 多个包，属协议适配层的合理耦合，但也是最容易积累重复的位置。
- **scanner 内聚偏低（0.45）符合预期**：它天然横跨 storage、model/metadata、persistence、core/artwork、plugins，是全局协调者。
- **测试基建扎实**：`tests/` 提供全套 mock 仓储（18 个），Ginkgo suite 覆盖各包，配合快照测试与 benchmark（`scanner_benchmark_test.go`）。

## 六、构建与开发

- `Makefile` 为统一入口（含 dev 热重载 reflex、wire 生成、protobuf 生成、i18n、发布打包）
- `Procfile.dev` + `reflex.conf` 支持前后端并行热重载
- `.golangci.yml` 约束 Go lint
- 交付形态：`Dockerfile`、`release/goreleaser.yml`、Linux 包脚本、Windows MSI（wix）
- 部署样例：`contrib/docker-compose`（Caddy / Traefik）、`contrib/k8s/manifest.yml`

## 七、可深入的方向

1. 扫描流水线的并发参数调优（`DevScannerThreads`、批大小）与大库表现
2. Subsonic 协议覆盖度与 OpenSubsonic 扩展的差异
3. 插件权限模型的边界（HTTP/WebSocket 白名单机制）
4. 前端状态分层（Redux 与 react-admin store 的职责划分）
5. `model/criteria` 智能播放列表 DSL 到 SQL 的翻译路径
