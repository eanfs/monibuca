# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Monibuca is a high-performance streaming server framework written in Go. It's designed to be a modular, scalable platform for real-time audio/video streaming with support for multiple protocols including RTMP, RTSP, HLS, WebRTC, GB28181, and more.

## Development Commands

### Building and Running

**Basic Run (with SQLite):**
```bash
cd example/default
go run -tags sqlite main.go
```

**Build Tags:**
- `sqlite` - Enable SQLite database support
- `sqliteCGO` - Enable SQLite with CGO
- `mysql` - Enable MySQL database support
- `postgres` - Enable PostgreSQL database support
- `duckdb` - Enable DuckDB database support
- `disable_rm` - Disable memory pool
- `fasthttp` - Use fasthttp instead of net/http
- `taskpanic` - Enable panics for testing
- `s3` - Enable AWS S3 / MinIO storage backend
- `cos` - Enable Tencent Cloud COS storage backend
- `oss` - Enable Alibaba Cloud OSS storage backend
- `enable_buddy` - Enable buddy memory allocator

**Protocol Buffer Generation:**
```bash
# Must use scripts — never use raw protoc commands
sh scripts/protoc.sh              # Generate all proto files
sh scripts/protoc.sh plugin_name  # Generate specific plugin proto
```

Windows:
```powershell
.\scripts\protoc.bat
.\scripts\protoc.bat plugin_name
```

**Release Building:**
```bash
goreleaser build
```

**Cross-compile (multi-arch, no CGO):**
- `CGO_ENABLED=0 GOOS=linux GOARCH={amd64,arm64} go build -tags "cluster sqlite s3" -o monibuca_${ARCH} ./example/cluster`
- `sqlite` tag = pure-Go `ncruces/go-sqlite3` (WASM) → cross-compiles incl. arm64 with no CGO (`sqliteCGO` = mattn/CGO variant; avoid for cross-builds)
- Multi-arch image: `./Dockerfile` copies pre-built `monibuca_${TARGETARCH}`; build with `docker buildx build --platform linux/amd64,linux/arm64`
- Push to Huawei SWR needs `--provenance=false --sbom=false` (SWR rejects buildkit attestation manifests → "Invalid image, fail to parse manifest.json"); `example/default/admin.zip` is fetched at build time, not in the repo

**Testing:**
```bash
go test ./...                                          # all tests
go test ./plugin/rtsp/pkg                              # single package
go test ./test -run '^TestRestart$' -count=1           # single test
go test ./pkg/util -bench . -run '^$'                  # benchmarks only
go test -race ./...                                    # race detector
```

**Lint / Static Analysis:**
```bash
gofmt -w <changed-files>
go vet ./...
staticcheck ./...    # config in staticcheck.conf
```

## Architecture Overview

### Core Components

**Server (`server.go`):** Main server instance that manages plugins, streams, and configurations. Implements the central event loop and lifecycle management. Managed via `task.RootManager[uint32, *Server]`.

**Plugin System (`plugin.go`):** Modular architecture where functionality is provided through plugins. Each plugin embeds `task.Work` (persistent queue manager) and implements the `IPlugin` interface. Plugins can provide:
- Protocol handlers (RTMP, RTSP, etc.)
- Media transformers
- Pull/Push proxies
- Recording capabilities
- Custom HTTP endpoints

**Configuration System (`pkg/config/`):** Hierarchical configuration system with priority order (high to low):
1. **Modify** — dynamic runtime modifications
2. **Env** — environment variables (uppercase, underscore-separated prefix)
3. **File** — config file (e.g., `config.yaml`)
4. **defaultYaml** — embedded default YAML
5. **Global** — global config section
6. **Default** — struct tag `default:"..."` values

**Task System (`pkg/task/`):** Advanced asynchronous task management system with multiple layers:
- **Task:** Basic unit of work with lifecycle management (Start/Run/Dispose)
- **Job:** Container that manages multiple child tasks and provides event loops
- **Work:** Special type of Job that acts as a persistent queue manager (keepalive=true)
- **Channel:** Event-driven task for handling continuous data streams

**Storage System (`pkg/storage/`):** Abstracted file storage with pluggable backends:
- **Local** — local filesystem (always available)
- **S3** — AWS S3 / MinIO (build tag: `s3`)
- **COS** — Tencent Cloud COS (build tag: `cos`)
- **OSS** — Alibaba Cloud OSS (build tag: `oss`)

All backends implement `Storage` and `File` interfaces. Upload retry logic is centralized in `pkg/storage/retry.go` via `UploadWithRetry()`.

### Task System Deep Dive

#### Task Hierarchy and Lifecycle
```
Work (Queue Manager, keepalive=true)
  └── Job (Container with Event Loop)
      └── Task (Basic Work Unit)
          ├── Start() - Initialization phase
          ├── Run() - Main execution phase
          └── Dispose() - Cleanup phase
```

#### Task Type Selection Rules
- **No child tasks** → embed `task.Task`
- **Need child tasks, stay alive** → embed `task.Work` (persistent queue manager)
- **Need child tasks, auto-exit when children done** → embed `task.Job`
- **Need timer** → embed `task.TickTask`
- **Need semaphore/signal** → embed `task.ChannelTask`

#### Critical Task Constraints
- **CAN** override: `Start()`, `Dispose()`
- **CANNOT** override: `Stop()` — use `Stop(reason)` to stop a task from outside
- **CANNOT** call any `task.Task` method directly except `Stop()`
- **CANNOT** call any `task.Job` method directly except `AddTask()`
- Return `task.ErrTaskComplete` for successful completion in `Run()`
- `Run()` runs **inline on the parent Job's event loop and blocks it**; `Go()` runs on its own goroutine — never block in `Run()`/`Start()`, use `Go()` for blocking/long work
- `util.Manager` Safe* (`Server.Streams.SafeGet`/`SafeRange`) route through the event loop when `L==nil` — calling them **re-entrantly from a publish/subscribe hook** (already on that loop) deadlocks; offload via `Post`/a child task or use the passed object reference

#### Task State Machine
```
INIT → STARTING → STARTED → RUNNING → GOING → DISPOSING → DISPOSED
```

#### Queue-based Asynchronous Processing
The Task system supports sophisticated queue-based processing patterns:

1. **Work as Queue Manager:** Work instances stay alive indefinitely and manage queues of tasks
2. **Task Queuing:** Use `workInstance.AddTask(task, logger)` to queue tasks
3. **Automatic Lifecycle:** Tasks are automatically started, executed, and disposed
4. **Error Handling:** Built-in retry mechanisms and error propagation

**Example Pattern (from S3 plugin):**
```go
type UploadQueueTask struct {
    task.Work  // Persistent queue manager
}

type FileUploadTask struct {
    task.Task  // Individual work item
    // ... task-specific fields
}

// Initialize queue manager (typically in init())
var uploadQueueTask UploadQueueTask
m7s.Servers.AddTask(&uploadQueueTask)

// Queue individual tasks
uploadQueueTask.AddTask(&FileUploadTask{...}, logger)
```

#### Cross-Plugin Task Cooperation
Tasks can coordinate across different plugins through:

1. **Global Instance Pattern:** Plugins expose global instances for cross-plugin access
2. **Event-based Triggers:** One plugin triggers tasks in another plugin
3. **Shared Queue Managers:** Multiple plugins can use the same Work instance

**Example (MP4 → S3 Integration):**
```go
// In MP4 plugin: trigger S3 upload after recording completes
s3plugin.TriggerUpload(filePath, deleteAfter)

// S3 plugin receives trigger and queues upload task
func TriggerUpload(filePath string, deleteAfter bool) {
    if s3PluginInstance != nil {
        s3PluginInstance.QueueUpload(filePath, objectKey, deleteAfter)
    }
}
```

### Key Interfaces

**Publisher:** Handles incoming media streams and manages track information
**Subscriber:** Handles outgoing media streams to clients
**Puller:** Pulls streams from external sources
**Pusher:** Pushes streams to external destinations
**Transformer:** Processes/transcodes media streams
**Recorder:** Records streams to storage

### Stream Processing Flow

1. **Publisher** receives media data and creates tracks
2. **Tracks** handle audio/video data with specific codecs
3. **Subscribers** attach to publishers to receive media
4. **Transformers** can process streams between publishers and subscribers
5. **Plugins** provide protocol-specific implementations

### Post-Recording Workflow

Monibuca implements a sophisticated post-recording processing pipeline:

1. **Recording Completion:** MP4 recorder finishes writing stream data
2. **Trailer Writing:** Asynchronous task moves MOOV box to file beginning for web compatibility
3. **File Optimization:** Temporary file operations ensure atomic updates
4. **External Storage Integration:** Automatic upload to S3-compatible services with retry
5. **Cleanup:** Optional local file deletion after successful upload

This workflow uses queue-based task processing to avoid blocking the main recording pipeline.

## Plugin Development

### Creating a Plugin

1. Implement the `IPlugin` interface (which inherits `task.IJob`)
2. Define plugin metadata using `PluginMeta`
3. Register with `InstallPlugin[YourPluginType](meta)` — auto-detects name and version via reflection
4. Optionally implement protocol-specific interfaces:
   - `ITCPPlugin` for TCP servers
   - `IUDPPlugin` for UDP servers
   - `IQUICPlugin` for QUIC servers
   - `IRegisterHandler` for HTTP endpoints
   - `IPublishHookPlugin` / `ISubscribeHookPlugin` for stream hooks

### Plugin Lifecycle

1. **Init:** Configuration parsing and initialization
2. **Start:** Network listeners and task registration
3. **Run:** Active operation
4. **Dispose:** Cleanup and shutdown

### Cross-Plugin Communication Patterns

#### 1. Global Instance Pattern
```go
// Expose global instance for cross-plugin access
var s3PluginInstance *S3Plugin

func (p *S3Plugin) Start() error {
    s3PluginInstance = p  // Set global instance
    // ... rest of start logic
}

// Provide public API functions
func TriggerUpload(filePath string, deleteAfter bool) {
    if s3PluginInstance != nil {
        s3PluginInstance.QueueUpload(filePath, objectKey, deleteAfter)
    }
}
```

#### 2. Event-Driven Integration
```go
// In one plugin: trigger event after completion
if t.filePath != "" {
    t.Info("MP4 file processing completed, triggering S3 upload")
    s3plugin.TriggerUpload(t.filePath, false)
}
```

#### 3. Shared Queue Managers
Multiple plugins can share Work instances for coordinated processing.

### Asynchronous Task Development Best Practices

#### 1. Implement Task Interfaces
```go
type MyTask struct {
    task.Task
    // ... custom fields
}

func (t *MyTask) Start() error {
    // Initialize resources, validate inputs
    return nil
}

func (t *MyTask) Run() error {
    // Main work execution
    // Return task.ErrTaskComplete for successful completion
    return nil
}
```

#### 2. Use Work for Queue Management
```go
type MyQueueManager struct {
    task.Work
}

var myQueue MyQueueManager

func init() {
    m7s.Servers.AddTask(&myQueue)
}

// Queue tasks from anywhere
myQueue.AddTask(&MyTask{...}, logger)
```

#### 3. Error Handling and Retry
- Tasks automatically support retry mechanisms
- Use `task.SetRetry(maxRetry, interval)` for custom retry behavior
- Return `task.ErrTaskComplete` for successful completion
- Return other errors to trigger retry or failure handling

## Configuration Structure

### YAML Config Rules
- **All field names must be lowercase** in YAML config files
- Field name normalization: lowercase, removes underscores/hyphens
- The `"plugin"` field is always skipped during parsing
- Config structs use `default:"..."` and `desc:"..."` tags

### Global Configuration
- HTTP/TCP/UDP/QUIC listeners
- Database connections (SQLite, MySQL, PostgreSQL, DuckDB)
- Authentication settings
- Admin interface settings
- Global stream alias mappings

### Plugin Configuration
Each plugin can define its own configuration structure that gets merged with global settings.

## Database Integration

Supports multiple database backends:
- **SQLite:** Default lightweight option
- **MySQL:** Production deployments
- **PostgreSQL:** Production deployments
- **DuckDB:** Analytics use cases

Automatic migration is handled for core models including users, proxies, and stream aliases.

## Protocol Support

### Built-in Plugins
- **RTMP:** Real-time messaging protocol
- **RTSP:** Real-time streaming protocol
- **HLS:** HTTP live streaming
- **WebRTC:** Web real-time communication
- **GB28181:** Chinese surveillance standard
- **FLV:** Flash video format
- **MP4:** MPEG-4 format with post-processing capabilities
- **SRT:** Secure reliable transport
- **S3:** File upload integration with AWS S3/MinIO compatibility

## Authentication & Security

- JWT-based authentication for admin interface
- Stream-level authentication with URL signing
- Role-based access control (admin/user)
- Webhook support for external auth integration

## Development Guidelines

### Code Style
- Follow existing patterns and naming conventions
- Use the task system for async operations; **never use bare goroutines** — prefer `AddTask`
- Low-level task primitives live in the fork `github.com/eanfs/gotask` (≥ v1.0.5) — import that, **not** `github.com/langhuihui/gotask`
- Implement proper error handling and logging
- Use the configuration system for all settings
- Dot imports are discouraged; exception: `staticcheck.conf` whitelists `. "m7s.live/v5/pkg"`

### Logging
- Use structured logging (`slog`): always pass **key-value pairs** — `t.Info("msg", "key", value)`
- Use task's built-in logger methods (`t.Info/Warn/Error/Debug`) rather than `log.Printf`
- Keep log messages short with context in key-value fields

### Proto Generation
- **Must use** `sh scripts/protoc.sh` (global) or `sh scripts/protoc.sh <plugin_name>` (per-plugin)
- **Never** use raw `protoc` command lines directly

### Storage Backend Development
- Storage backends (S3, COS, OSS) are guarded by build tags (`s3`, `cos`, `oss`)
- All backends implement the `Storage` and `File` interfaces in `pkg/storage/storage.go`
- Upload retry logic is centralized in `pkg/storage/retry.go` via `UploadWithRetry()`
- `File.Close()` triggers upload for object storage backends; use `defer` to ensure temp file cleanup

### Testing
- Unit tests should be placed alongside source files
- Integration tests can use the example configurations
- Use the mock.py script for protocol testing
- After edits, at minimum: `gofmt -w <changed-files>` then `go test <affected-package> -count=1`

### Performance Considerations
- Memory pool is enabled by default (disable with `disable_rm`)
- Zero-copy design for media data where possible
- Lock-free data structures for high concurrency
- Efficient buffer management with ring buffers
- Queue-based processing prevents blocking main threads

## Debugging

### Built-in Debug Plugin
- Performance monitoring and profiling
- Real-time metrics via Prometheus endpoint (`/api/metrics`)
- pprof integration for memory/cpu profiling

### Logging
- Structured logging with slog
- Configurable log levels
- Log rotation support
- Fatal crash logging

### Task System Debugging
- Tasks automatically include detailed logging with task IDs and types
- Use `task.Debug/Info/Warn/Error` methods for consistent logging
- Task state and progress can be monitored through descriptions
- Event loop status and queue lengths are logged automatically

## Web Admin Interface

- Web-based admin UI served from `admin.zip`
- RESTful API for all operations
- Real-time stream monitoring
- Configuration management
- User management (when auth enabled)

## Common Issues

### Port Conflicts
- Default HTTP port: 8080
- Default gRPC port: 50051
- Check plugin-specific port configurations

### Database Connection
- Ensure proper build tags for database support
- Check DSN configuration strings
- Verify database file permissions

### Plugin Loading
- Plugins are auto-discovered from imports
- Check plugin enable/disable status
- Verify configuration merging

### Task System Issues
- Ensure Work instances are added to server during initialization
- Check task queue status if tasks aren't executing
- Verify proper error handling in task implementation
- Monitor task retry counts and failure reasons in logs

## Common Pitfalls
- Forgetting required build tags for DB/protocol/storage-specific behavior
- Editing `.proto` but not regenerating with `scripts/protoc.sh`
- Adding non-whitelisted dot imports
- Overriding `Stop()` instead of using `Stop(reason)` from outside
- Calling `task.Task` methods directly (only `Stop()` is allowed)
- Using uppercase field names in YAML config files
- Using bare goroutines instead of the task system
- Bypassing task lifecycle conventions in async plugin code
- Importing `github.com/langhuihui/gotask` instead of the fork `github.com/eanfs/gotask`
- **Registering `OnStart` *after* `AddTask` — the callback never fires.** `Task.OnStart` only appends to `afterStartListeners` (no already-started check) and that slice is consumed once during the start sequence, so `AddTask(&x); x.OnStart(f)` silently drops `f`. Always register `OnStart` **before** `AddTask`. This silently killed both `UploadRetryScheduler` (upload retry never ran → failed uploads stranded in `pending_uploads` forever) and `CheckSubWaitTimeout` until 2026-07-16; regression tests in `upload_retry_register_test.go`. 已核查全仓其余 `.OnStart(` 调用点均在 AddTask 之前(Manager.Add 正确封装,经其路由的任务天然安全)。根治在上游 gotask:`OnStart` 检查任务已启动则立即补发(连带修监听器切片的并发写);上游修复后 `TestOnStartAfterAddTaskNeverFires` 会转红,即为可放宽此顺序约束的信号
- **`SetRetry` 不配 `SetMaxRetryInterval` = 指数退避无上限。** gotask 退避 = `RetryInterval×2^(retryCount-1)`,上限判断被 `MaxRetryInterval > 0` 门控——不设就无界。源/目标长时间中断后 retryDelay 涨到小时级,网络恢复了任务还在睡,**永不自愈只能重启进程**(2026-07-17 实证:IPS 封禁 16h 后 30 路 puller 全部 `retryDelay=11h22m40s`;`RetryInterval: 5s` 配置只是基数,不是"每 5s 重试")。凡 `SetRetry(非零,...)` 必须跟 `GetTask().SetMaxRetryInterval(30*time.Second)`;已修 puller/pusher/transformer/cascade 四处(recoder 早在 `ae298dab` 修过);语义钉在 `retry_backoff_test.go`。注意 `retryCount` 只在 Start 成功后归零(task.go:386),所以短暂抖动无感、长中断致命
- Pushing multi-arch images to SWR without `--provenance=false --sbom=false`
- mp4 record API: `POST /mp4/api/start|stop/<streamPath>` (gRPC-gateway, streamPath in URL), records **local** streams only; cluster HTTP is under `/cluster/api/cluster/*`
- mp4 start API 请求体所有字段都是 proto **string**——`duration`/`fragment` 必须传 JSON 字符串(`"900s"`/`"0"`),传数字会被 grpc-gateway 拒绝 **400** `invalid value for string type`。老版本(无该字段)因 `DiscardUnknown` 静默忽略数字字段,升级后才暴露(2026-07-15 196 演示环境 its-server 事故根因,报告见 `example/cluster-e2e/cluster-e2e-reports/report-2026-07-15-196-its-record-400.md`)
- mp4 start 不传 `fragment` 时**默认 1 分钟分片**(`StartRecord` handler 里 `fragment = time.Minute`);要录单文件必须显式 `"fragment":"0"`
- **指定 `fileName` + fragment>0 = 静默丢数据**:`CustomFileName` 对每个分片返回同一路径,分片轮转反复覆盖同一 S3 对象键/本地文件,最终只剩最后一个分片(见 Known Issues)
- `duration` 到点后 monibuca 自行停录,之后外部再调 `/mp4/api/stop` 会得 **500 not found**——调用方应把 not-found 当幂等成功
- Cross-node record on a non-owner node: trigger auto-relay first via a **subscribe**, then record the now-local stream. Whether a subscribe relays or 302-redirects depends on the per-plugin `proxyOnRedirect` flag (`default:"false"` = 302 redirect; set `true` = local pull proxy instead). With `proxyOnRedirect: false` FLV 302-redirects and won't relay — use RTSP/RTMP; with `proxyOnRedirect: true` (the 3-node e2e configs set this on rtsp+flv) FLV/RTSP relay and return 200

## Known Issues (待其它环境修复)

记录 cluster v1 工作中发现、但本环境无法或不应当处修的事项。

### mp4 分片录制 + 指定 fileName 时反复覆盖同一文件(数据丢失)

- **位置**: `plugin/mp4/pkg/record.go` `CustomFileName`——`RecConf.FileName` 非空时每个分片都返回 `filePath/fileName.mp4` 同一路径;仅 fileName 为空才用时间戳命名。
- **后果**: fragment>0(含 API 不传 fragment 的默认 1 分钟)且指定 fileName 时,每次分片轮转覆盖同一 S3 对象键/本地文件,**只保留最后一个分片**,且全程无告警(2026-07-15 三节点 FINAL2 批次 30 路 15min 录制每路只剩最后 1 分钟,全批作废)。
- **修复方向**: fileName 指定且 fragment>0 时给分片追加时间戳/序号(如 `fileName_2006-01-02-15-04-05.mp4`),或在 `StartRecord` 校验该组合直接拒绝;修复前调用方必须显式 `"fragment":"0"`。

### its-server(xde-media-spring-boot-starter)对接 v5.3.1 的 3 处待改

2026-07-15 196 演示环境事故(录制 400、视频未上传 MinIO)结论,详见 `example/cluster-e2e/cluster-e2e-reports/report-2026-07-15-196-its-record-400.md`:
1. `Mp4StartRequest.duration` 需 `Integer`→`String`(发 `"900"` 或 `"900s"`);
2. stop 收到 500 not-found 应视为幂等成功(duration 到点 monibuca 已自停);
3. 录单文件需补传 `"fragment":"0"`(否则默认 1 分钟分片,叠加上面 fileName 覆盖缺陷)。

### gotask 库 `-race` 下 EventLoop 数据竞争

- **症状**: `go test -race -tags cluster ./plugin/cluster/...` 跑全套时 fail。报 `WARNING: DATA RACE`,两次 write 都在 `github.com/langhuihui/gotask@v1.0.4/event_loop.go:111` vs `:116` 的同一 `EventLoop` struct 字段(offset `0x7b8`)。
- **触发**: 父 `Job`(如 `Membership` 或 `StreamRegistry`)的 child task dispose 时,`waitChildrenDispose` 在新 goroutine 里 spawn 又一次 `EventLoop.run`,与已经 finished 的 goroutine 通过 defer 写同一字段。
- **本仓影响**: 仅 `-race` 标志下抓得到。**正常 `go test -tags cluster` 全 29 case PASS,生产无功能问题**。
- **修复需要**: upstream `github.com/langhuihui/gotask` 加锁/原子保护 EventLoop 内部状态。
- **追踪**: 在 cluster spec §6.5 验收硬指标中"-race -count=3 全绿"暂未达成,等 upstream fix 后再跑。

### example/cluster-e2e/smoke.sh 半自动场景

- **位置**: `example/cluster-e2e/smoke.sh`。
- **自动覆盖**: 场景 1 (membership) / 2 (推流) / 3 (跨节点订阅) / 8 (origin 失联) / 10 (lb-suggest)。
- **手动验证留待**: 场景 4 (pull-proxy 复用计数) / 5 (录制 API 路由到 origin) / 6 (跨节点录制列表) / 7 (跨节点 /download 302) / 9 (first-write-wins)。
- **原因**: mp4 record API 路径 / record id schema 在多种部署下表现不同,需真实 docker-compose stack 上手动逐场景 curl + 看日志,无法纯 bash 一次断完。
- **修复需要**: 在已起的真 docker stack 上跑 smoke.sh,看哪些手动场景能自动化、补 assertion。

### cluster 必须节点本地 sqlite —— 不支持共享 DB(决策已定)

- **约束**: cluster 部署**每节点各自本地 sqlite**(`dsn: /data/m7s.db`),集群协调全走 Consul。`example/cluster-e2e/` 的 compose + 三份 config 已按此形态。
- **原因**: `PullProxyConfig`(`pull_proxy.go`)**无节点归属列**,共享 DB 时任一节点重启会加载全表 `pull_on_start` → **拉全网 + KV 属主冲突 + 负载激增**(2026-06-03 实证)。
- **代价(已接受)**: 录制元数据各存各的 → **跨节点录制列表聚合 / 跨节点 `/download` 302 不支持**;需在属主节点直接取回。
- **若将来要支持共享 DB**: 需给 `PullProxyConfig` 加节点归属列 + 按本节点过滤(含 migration),并重跑 cluster 全套回归。

### ~~cluster auto-relay 后 first-write-wins 永久失效~~(已修,2026-07-17)

- **真实根因**(修复时勘误,非最初判断的"OnDispose 泄漏"):relay pull-proxy 是**常驻按需路由**——`StopOnIdle` 只让空闲 publisher 5s 关闭,proxy 本体不销毁(下次订阅由 `Server.OnSubscribe` 重新 `Pull()`),所以 `activeRelays` 条目常驻是**设计使然**。缺陷在分类:`StreamRegistry.OnPublish` 只按 streamPath 命中 `activeRelays` 就把 publisher 当 relay 派生跳过 first-write-wins——入站推流(`Type="server"`)撞上曾 relay 过的路径即漏防 → 脑裂,全程无告警。
- **实证**(2026-07-16 报告 §3):同一条流同一节点,relay 前推同名流 `exit=224`(被停,正确);relay 后推 `exit=0`(漏防)。
- **修复**: `streamregistry.go` OnPublish 分类加 `pub.Type == PublishTypePull` 门槛(relay 派生必为 pull 派生,`PullJob.Init` 强制 PubType=pull)。回归测试 `plugin/cluster/streamregistry_c5_test.go`(push 撞 relay 路由必须 acquire / relay 本体仍跳过 / 普通 pull 照常注册)。
- **排障陷阱**: `/api/proxy/pull/list` 只列 **DB 持久化**的 pull proxy;`EnsurePullProxy` 建的内存 relay proxy 永远不在里面,别据此判断 relay proxy 已消失。

### progressive MP4 录制中崩溃不可恢复(结构性限制)

- **现象**: `record.type: mp4`(progressive)的 moov 仅在停止时写入(`writeTrailerTask.Start`),录制中磁盘上无 moov、mdat 占位 size=8;进程被 kill/断电则该段不可播,`plugin/mp4/recovery.go` 依赖 demuxer 找 moov 也无法重建。
- **缓解**: 配 `fragment` 分段可把损失限制在当前段;或改用 `record.type: fmp4` —— init moov 修复后 moof/mdat 自描述,录制中崩溃文件仍可播,天然崩溃安全。
- **彻底修复方向**: progressive 周期性 moov 快照(IO 代价大,31 路并发录制场景需评估)或崩溃后从裸 mdat 重建索引的恢复工具;与 trailer 零重写/fMP4 旁路计划同族,待排期。

### example/cluster-e2e 未实跑验证

- **位置**: `example/cluster-e2e/`(Dockerfile + docker-compose + 3 configs + smoke.sh + README)。
- **状态**: yaml + bash 语法都过(yq / `bash -n` / `docker buildx --check`),**但完整 docker stack + ffmpeg sample.mp4 在本环境未真跑**。
- **修复需要**: 在能跑 docker desktop 的环境上 `./smoke.sh`,看哪些场景需调端口/超时/yaml 字段名等。
