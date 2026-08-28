# Monibuca S3 启动恢复与历史录像解析 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 消除 MinIO 启动竞态造成的永久 local 降级，使显式 fallback 仅临时生效并自动切回 S3，同时让历史录像始终按自身 `storage_type` 解析后端。

**Architecture:** `pkg/storage.Registry` 按类型并发安全地缓存已连接后端；`Server` 用一个 `atomic.Pointer` 保存“活动后端 + 不可变状态快照”，所有消费者经 getter 读取。S3 初始化失败时根据 `storageallowlocalfallback` 选择 unavailable 占位或临时 local，但两者都保持 degraded；Server 专属 `StorageReconnectWork` 中的 `TickTask` 以一秒轮询、自管截止时间退避恢复，避免阻塞 `Records`。MP4 下载、Demux 和删除统一调用 `GetStorageForType`，不再比较当前全局类型。

**Tech Stack:** Go 1.26、`github.com/eanfs/gotask v1.0.5`、AWS SDK for Go v1.55.7、SQLite build tag、S3 build tag、MinIO、Docker Compose、标准库 `sync/atomic` / `httptest` / `testing`。

**Spec:** `docs/superpowers/specs/2026-08-27-storage-recovery-design.md`

## Global Constraints

- 本次只验收 S3/MinIO；不修改 xde-installer，不部署或重启 172.16.12.74 / 172.16.12.183。
- 未配置对象存储时继续正常使用 local，`degraded=false`。
- 配置 S3 时 `storageallowlocalfallback` 默认 `false`；失败期间不建立录像记录并返回 HTTP 503。
- 显式开启 fallback 时只临时写 local：`degraded=true`、`fallbackActive=true`、持续重连；恢复后新录像原子切回 S3。
- fallback 期间已开始/已完成的 local 录像不迁移；其 `storage_type=local` 保持不变并按 local 下载。
- 生产异步逻辑必须使用 gotask，禁止裸 goroutine；`OnStart` 必须在对应 `AddTask` 之前注册。
- 日志必须使用结构化 key-value；不得记录 access key、secret key、RTSP userinfo 或预签名 URL。
- proto 如有变更只能运行 `sh scripts/protoc.sh`；本方案采用直接 HTTP readiness，不改 proto。
- 每个实现任务遵循 RED → GREEN → IMPROVE，并在独立测试通过后提交。
- 当前 worktree 分支为 `feat/fix-storage-failed`，开始执行前再次确认 `git status --short`；不得覆盖用户改动。

## 执行修订（最终实现优先）

> **重要：** 本节是评审后的绑定修订。Task 5 与 Task 8 下方较早的代码片段保留为计划历史，**不得**再按其原样实现或据此判断最终架构。

### Task 5 最终修订

- 原“`StorageReconnectTask` 嵌入 `task.Task`、返回错误交给 gotask native retry、挂到 `Records`”方案已废止。最终实现由 `Server` 持有独立 `StorageReconnectWork`，其 `OnStart` 在 `AddTask` 前注册并加入一个 `task.TickTask`。
- Tick 每秒轮询自管的 `nextAttempt`；截止时间退避为 5s/10s/20s/40s/80s/160s/5m（封顶）。生产路径无 `sleep`、无裸 goroutine，也不把连接错误返回给 gotask。
- 瞬态失败更新脱敏 degraded 状态后继续；成功原子切到 S3，永久错误停止继续尝试。成功与永久错误都以通用 `task.ErrTaskComplete` 完成，readiness 在永久错误后仍为 503。
- `NoSuchBucket` 对启动连接分类仍可重试；上传错误分类维持既有永久错误语义。`Records.OnStart` 中的上传补偿调度器同样必须先于 `s.AddTask(&s.Records)` 注册。

### Task 8 最终修订

- 原固定 sleep、host 跟随对象存储重定向、latest-row/storage-type-only SQL、导出凭据环境变量、忽略 teardown 失败和无边界 `COPY . .` 的片段全部废止。
- 最终交付为六项：原五项外加 `Dockerfile.storage-race.dockerignore`。脚本拥有确定性的 `/control/start-minio` 门闩；下载在 monibuca 网络命名空间内完成，只验证 body/media，不输出响应头、重定向 URL 或 curl stderr。
- SQLite 只读打开，按唯一文件名查询并要求恰好一条、`deleted_at IS NULL`、`end_time IS NOT NULL`、期望 `file_path` 与 `storage_type`；required-S3 拒绝场景在恢复前后持续检查零 ghost row。
- 每个场景凭据写入 mode-0600 `--env-file`，不 export 到 host 子进程；teardown 是有界且可确认的屏障，失败时保留 mode-0700/0600 工件并返回非零。镜像只构建一次，场景使用 `up --no-build`。
- Dockerfile 使用定向 COPY；Dockerfile-specific ignore 默认拒绝，并在所有 broad embed re-include 后重复敏感文件最终 deny。最终 deny 后只允许两个仓库已跟踪、构建所需的精确 TLS embed 文件，不允许目录级 key/cert 例外。
- 本次 Task 9 **没有执行** Docker daemon、镜像、网络或五轮场景；仅执行 bash/shellcheck、Compose client `config --quiet`、模板/脚本/Dockerignore 静态策略检查。

### Task 9 验证修订

- 变更文件必须从 `git diff --name-only d1b7abee8ca17dc5867f944a11c4163d718dc2b8...HEAD` 派生；只对其中 Go 文件运行 gofmt。
- 默认/S3/race 使用 registry、runtime、reconnect classifier、record gate、MP4/GB28181 历史存储契约的精确测试模式；S3 下的 `plugin/mp4/...` 只做 `-run '^$'` 编译门禁，不能把依赖绝对媒体夹具的 broad suite 宣称为通过。
- build/vet 只覆盖受影响路径。根包 `go vet ./` 的 `plugin.go:243` copylocks 与 GB28181 `broadcast.go` IPv6 地址格式均为未修改基线；全仓 crypto 编译错误和 broad gotask EventLoop race不在本任务修复范围。
- 安全扫描覆盖上述提交范围及本设计/计划，只报告模式名、文件名和计数，不输出匹配值；预存在示例凭据需另立轮换事项。

---

## 文件结构

```text
pkg/storage/
├── registry.go                         [新] 按类型 single-flight 构造、Close 生命周期屏障
├── registry_test.go                    [新] 失败不缓存、并发只构造一次、并发/重复 Close
├── unavailable.go                      [新] degraded 且禁止 fallback 时的 Null Object
├── unavailable_test.go                 [新] ErrStorageNotAvailable 契约
├── retry.go / retry_test.go            [改] 启动连接永久错误分类，不改变上传分类
├── s3.go / s3_validate_test.go         [改] 配置校验 sentinel
└── storage.go                          [改] 新增 registry/config 错误

storage_runtime.go                      [新] atomic snapshot、GetStorageForType、统一状态/错误脱敏
storage_runtime_test.go                 [新] 并发切换、按类型解析、纯 local 状态
storage_reconnect.go                    [新] 专属 Work + 一秒 TickTask + 自管截止时间退避
storage_reconnect_test.go               [新] 启动 fallback 状态机
storage_reconnect_tick_test.go          [新] 退避、永久错误、恢复、隔离生命周期与脱敏
server.go                               [改] 配置开关、initStorage、handler 注册、OnStart 顺序、Dispose
recoder.go                              [改] degraded 且无 fallback 时返回 codes.Unavailable
recoder_storage_test.go                 [新] 禁止 fallback=503、允许 fallback=local
api.go                                  [改] DeleteRecord 按记录 StorageType 删除；readiness handler
api_delete_record_storage_test.go       [新] 混合后端删除、回滚与错误脱敏
storage_status_api_test.go              [新] ready=200、degraded=503、方法约束与错误脱敏

plugin/mp4/api.go                       [改] 单文件和范围下载按 StorageType 解析并脱敏 backend 错误
plugin/mp4/api_storage_test.go          [新] 历史对象解析、资源关闭与错误脱敏
plugin/mp4/api_security_test.go         [新] 预签名重定向不进入日志
plugin/mp4/pkg/demux-range.go           [改] 注入 resolver，直接按记录类型 OpenFile
plugin/mp4/pkg/demux_range_storage_test.go [新] 对象记录分段不再静默跳过
plugin/mp4/pkg/pull-recorder.go         [改] 注入 Server.GetStorageForType

plugin/gb28181/download_handler.go      [改] GetStorage 机械迁移并直接流式服务对象文件
plugin/gb28181/download_handler_test.go [新] 对象下载与 URL 日志脱敏
plugin/flv/pkg/pull-recorder.go         [改] GetStorage 机械迁移
plugin/mp4/exception.go                 [改] GetStorage 机械迁移
plugin/snap/api.go                      [改] GetStorage 机械迁移
upload_retry.go / upload_retry_test.go  [改] active 类型不匹配时禁止补传到错误后端

example/record-test/
├── docker-compose.storage-race.yml             [新] 脚本控制 MinIO + Monibuca + publisher
├── Dockerfile.storage-race                     [新] sqlite+s3 专用集成测试镜像
├── Dockerfile.storage-race.dockerignore        [新] deny-by-default 构建上下文策略
├── config.storage-race.yaml.tmpl               [新] 不含凭据的配置模板
├── scripts/verify_storage_race.sh              [新] 5 次竞态验证（本次只静态检查，不执行）
└── .gitignore                                  [改] 排除 storage-race 工件
```

每个新文件只承担一种职责。不要把 registry、运行状态、重连任务继续堆入 `server.go`。

---

### Task 1: Storage Registry 与 unavailable 占位实现

**Files:**
- Create: `pkg/storage/registry.go`
- Create: `pkg/storage/registry_test.go`
- Create: `pkg/storage/unavailable.go`
- Create: `pkg/storage/unavailable_test.go`
- Modify: `pkg/storage/storage.go:110-115`

**Interfaces:**
- Consumes: 现有 `storage.CreateStorage(t string, config any) (Storage, error)` 与 `Storage` 接口。
- Produces: `NewRegistry(map[string]any) *Registry`、`(*Registry).HasConfig(string) bool`、`(*Registry).GetOrCreate(string) (Storage, error)`、`(*Registry).Close() error`、`NewUnavailableStorage(string) Storage`、`ErrStorageTypeNotConfigured`。

- [ ] **Step 1: 在 `pkg/storage/storage.go` 写新增错误的失败测试**

在 `pkg/storage/registry_test.go` 先写：

```go
package storage

import (
    "context"
    "errors"
    "sync"
    "sync/atomic"
    "testing"
)

type registryTestStorage struct {
    key        string
    closeCount atomic.Int32
}

func (s *registryTestStorage) CreateFile(context.Context, string) (File, error) { return nil, nil }
func (s *registryTestStorage) OpenFile(context.Context, string) (File, error)   { return nil, nil }
func (s *registryTestStorage) Delete(context.Context, string) error             { return nil }
func (s *registryTestStorage) Exists(context.Context, string) (bool, error)      { return true, nil }
func (s *registryTestStorage) GetSize(context.Context, string) (int64, error)    { return 0, nil }
func (s *registryTestStorage) GetURL(context.Context, string) (string, error)    { return "", nil }
func (s *registryTestStorage) List(context.Context, string) ([]FileInfo, error)  { return nil, nil }
func (s *registryTestStorage) Close() error {
    s.closeCount.Add(1)
    return nil
}
func (s *registryTestStorage) GetKey() string { return s.key }

func TestRegistryRejectsUnconfiguredType(t *testing.T) {
    registry := newRegistry(map[string]any{}, func(string, any) (Storage, error) {
        t.Fatal("creator must not run for an unconfigured type")
        return nil, nil
    })

    _, err := registry.GetOrCreate("s3")
    if !errors.Is(err, ErrStorageTypeNotConfigured) {
        t.Fatalf("expected ErrStorageTypeNotConfigured, got %v", err)
    }
}
```

同时从 import 中删除尚未使用的包，随着后续测试逐步加回；禁止用空白标识符掩盖未使用 import。

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./pkg/storage -run '^TestRegistryRejectsUnconfiguredType$' -count=1`

Expected: FAIL，提示 `newRegistry` / `ErrStorageTypeNotConfigured` 未定义。

- [ ] **Step 3: 添加错误与 registry 最小骨架**

`pkg/storage/storage.go` 错误块新增：

```go
ErrStorageTypeNotConfigured = fmt.Errorf("storage type not configured")
```

`pkg/storage/registry.go` 写入以下接口；`newRegistry` 保持包内可见，便于无网络单测注入 creator：

```go
package storage

import (
    "errors"
    "fmt"
    "maps"
    "sync"
)

type storageCreator func(string, any) (Storage, error)

type registryEntry struct {
    mu       sync.Mutex
    config   any
    instance Storage
}

type Registry struct {
    mu      sync.RWMutex
    entries map[string]*registryEntry
    creator storageCreator
}

func NewRegistry(configs map[string]any) *Registry {
    return newRegistry(configs, CreateStorage)
}

func newRegistry(configs map[string]any, creator storageCreator) *Registry {
    cloned := maps.Clone(configs)
    if cloned == nil {
        cloned = make(map[string]any)
    }
    if _, ok := cloned[string(StorageTypeLocal)]; !ok {
        cloned[string(StorageTypeLocal)] = "."
    }
    entries := make(map[string]*registryEntry, len(cloned))
    for key, config := range cloned {
        entries[key] = &registryEntry{config: config}
    }
    return &Registry{entries: entries, creator: creator}
}

func (r *Registry) HasConfig(storageType string) bool {
    r.mu.RLock()
    defer r.mu.RUnlock()
    _, ok := r.entries[storageType]
    return ok
}

func (r *Registry) GetOrCreate(storageType string) (Storage, error) {
    r.mu.RLock()
    entry, ok := r.entries[storageType]
    r.mu.RUnlock()
    if !ok {
        return nil, fmt.Errorf("%w: %s", ErrStorageTypeNotConfigured, storageType)
    }

    entry.mu.Lock()
    defer entry.mu.Unlock()
    if entry.instance != nil {
        return entry.instance, nil
    }
    instance, err := r.creator(storageType, entry.config)
    if err != nil {
        return nil, fmt.Errorf("create storage %s: %w", storageType, err)
    }
    entry.instance = instance
    return instance, nil
}

func (r *Registry) Close() error {
    r.mu.RLock()
    entries := make([]*registryEntry, 0, len(r.entries))
    for _, entry := range r.entries {
        entries = append(entries, entry)
    }
    r.mu.RUnlock()

    var closeErrors []error
    for _, entry := range entries {
        entry.mu.Lock()
        instance := entry.instance
        entry.instance = nil
        entry.mu.Unlock()
        if instance != nil {
            if err := instance.Close(); err != nil {
                closeErrors = append(closeErrors, err)
            }
        }
    }
    return errors.Join(closeErrors...)
}
```

- [ ] **Step 4: 运行最小测试确认 GREEN**

Run: `gofmt -w pkg/storage/storage.go pkg/storage/registry.go pkg/storage/registry_test.go && go test ./pkg/storage -run '^TestRegistryRejectsUnconfiguredType$' -count=1`

Expected: PASS。

- [ ] **Step 5: 添加“失败不缓存、成功缓存”测试**

追加到 `registry_test.go`：

```go
func TestRegistryCachesOnlySuccessfulConstruction(t *testing.T) {
    var attempts atomic.Int32
    expected := &registryTestStorage{key: "fake"}
    registry := newRegistry(map[string]any{"fake": map[string]any{"enabled": true}}, func(storageType string, _ any) (Storage, error) {
        if storageType != "fake" {
            t.Fatalf("unexpected type %q", storageType)
        }
        if attempts.Add(1) <= 2 {
            return nil, errors.New("connection refused")
        }
        return expected, nil
    })

    for attempt := 1; attempt <= 2; attempt++ {
        if _, err := registry.GetOrCreate("fake"); err == nil {
            t.Fatalf("attempt %d should fail", attempt)
        }
    }
    first, err := registry.GetOrCreate("fake")
    if err != nil {
        t.Fatal(err)
    }
    second, err := registry.GetOrCreate("fake")
    if err != nil {
        t.Fatal(err)
    }
    if first != expected || second != expected || attempts.Load() != 3 {
        t.Fatalf("success must be cached: first=%p second=%p attempts=%d", first, second, attempts.Load())
    }
}
```

- [ ] **Step 6: 添加并发 single-flight 与 Close 测试**

追加：

```go
func TestRegistryConcurrentGetOrCreateBuildsOnce(t *testing.T) {
    var attempts atomic.Int32
    expected := &registryTestStorage{key: "fake"}
    registry := newRegistry(map[string]any{"fake": struct{}{}}, func(string, any) (Storage, error) {
        attempts.Add(1)
        return expected, nil
    })

    const workers = 32
    results := make(chan Storage, workers)
    var wait sync.WaitGroup
    wait.Add(workers)
    for range workers {
        go func() {
            defer wait.Done()
            instance, err := registry.GetOrCreate("fake")
            if err != nil {
                t.Errorf("GetOrCreate: %v", err)
                return
            }
            results <- instance
        }()
    }
    wait.Wait()
    close(results)
    for instance := range results {
        if instance != expected {
            t.Fatalf("unexpected instance %p", instance)
        }
    }
    if attempts.Load() != 1 {
        t.Fatalf("expected one construction, got %d", attempts.Load())
    }
    if err := registry.Close(); err != nil {
        t.Fatal(err)
    }
    if expected.closeCount.Load() != 1 {
        t.Fatalf("expected one close, got %d", expected.closeCount.Load())
    }
}
```

从 import 中移除示例里未使用的 `io`、`os`；最终只保留实际使用项。

- [ ] **Step 7: 写 unavailableStorage 失败测试**

`pkg/storage/unavailable_test.go`：

```go
package storage

import (
    "context"
    "errors"
    "testing"
)

func TestUnavailableStorageReturnsSentinel(t *testing.T) {
    unavailable := NewUnavailableStorage("s3")
    if unavailable.GetKey() != "s3" {
        t.Fatalf("key = %q", unavailable.GetKey())
    }
    if _, err := unavailable.CreateFile(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("CreateFile error = %v", err)
    }
    if _, err := unavailable.OpenFile(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("OpenFile error = %v", err)
    }
    if err := unavailable.Delete(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("Delete error = %v", err)
    }
    if _, err := unavailable.Exists(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("Exists error = %v", err)
    }
    if _, err := unavailable.GetSize(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("GetSize error = %v", err)
    }
    if _, err := unavailable.GetURL(context.Background(), "x.mp4"); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("GetURL error = %v", err)
    }
    if _, err := unavailable.List(context.Background(), ""); !errors.Is(err, ErrStorageNotAvailable) {
        t.Fatalf("List error = %v", err)
    }
    if err := unavailable.Close(); err != nil {
        t.Fatalf("Close error = %v", err)
    }
}
```

- [ ] **Step 8: 运行 unavailable 测试确认 RED**

Run: `go test ./pkg/storage -run '^TestUnavailableStorageReturnsSentinel$' -count=1`

Expected: FAIL，提示 `NewUnavailableStorage` 未定义。

- [ ] **Step 9: 实现 unavailableStorage**

`pkg/storage/unavailable.go`：

```go
package storage

import "context"

type unavailableStorage struct {
    configuredType string
}

func NewUnavailableStorage(configuredType string) Storage {
    return &unavailableStorage{configuredType: configuredType}
}

func (s *unavailableStorage) CreateFile(context.Context, string) (File, error) {
    return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) OpenFile(context.Context, string) (File, error) {
    return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) Delete(context.Context, string) error { return ErrStorageNotAvailable }
func (s *unavailableStorage) Exists(context.Context, string) (bool, error) {
    return false, ErrStorageNotAvailable
}
func (s *unavailableStorage) GetSize(context.Context, string) (int64, error) {
    return 0, ErrStorageNotAvailable
}
func (s *unavailableStorage) GetURL(context.Context, string) (string, error) {
    return "", ErrStorageNotAvailable
}
func (s *unavailableStorage) List(context.Context, string) ([]FileInfo, error) {
    return nil, ErrStorageNotAvailable
}
func (s *unavailableStorage) Close() error   { return nil }
func (s *unavailableStorage) GetKey() string { return s.configuredType }
```

- [ ] **Step 10: 运行 package 测试与 race**

Run: `gofmt -w pkg/storage/registry.go pkg/storage/registry_test.go pkg/storage/unavailable.go pkg/storage/unavailable_test.go pkg/storage/storage.go && go test -race ./pkg/storage -run 'TestRegistry|TestUnavailable' -count=1`

Expected: PASS，无 data race。

- [ ] **Step 11: 提交 Task 1**

Run: `git add pkg/storage/registry.go pkg/storage/registry_test.go pkg/storage/unavailable.go pkg/storage/unavailable_test.go pkg/storage/storage.go && git commit -m "feat(storage): add backend registry and unavailable state"`

---

### Task 2: 原子 Storage snapshot 与全仓调用点迁移

**Files:**
- Create: `storage_runtime.go`
- Create: `storage_runtime_test.go`
- Modify: `server.go:101-135, 793-802, 841-859`
- Modify: `recoder.go:91-99`
- Modify: `upload_retry.go:30-31, 105`
- Modify: `api.go:1011`
- Modify: `plugin/gb28181/download_handler.go:49`
- Modify: `plugin/flv/pkg/pull-recorder.go:82`
- Modify: `plugin/mp4/api.go:138, 507`
- Modify: `plugin/mp4/exception.go:199, 233, 302`
- Modify: `plugin/mp4/pkg/pull-recorder.go:67`
- Modify: `plugin/snap/api.go:285, 691, 721, 736, 741, 830, 835`

**Interfaces:**
- Consumes: Task 1 的 `storage.Registry`。
- Produces: `StorageStatus`、`(*Server).GetStorage() storage.Storage`、`(*Server).GetStorageStatus() StorageStatus`、私有 `activateStorage(storage.Storage, StorageStatus)`。

- [ ] **Step 1: 写原子 snapshot 的失败测试**

`storage_runtime_test.go`：

```go
package m7s

import (
    "context"
    "sync"
    "testing"
    "time"

    "m7s.live/v5/pkg/storage"
)

type runtimeTestStorage struct{ key string }

func (s *runtimeTestStorage) CreateFile(context.Context, string) (storage.File, error) { return nil, nil }
func (s *runtimeTestStorage) OpenFile(context.Context, string) (storage.File, error)   { return nil, nil }
func (s *runtimeTestStorage) Delete(context.Context, string) error                    { return nil }
func (s *runtimeTestStorage) Exists(context.Context, string) (bool, error)             { return true, nil }
func (s *runtimeTestStorage) GetSize(context.Context, string) (int64, error)           { return 0, nil }
func (s *runtimeTestStorage) GetURL(context.Context, string) (string, error)           { return "", nil }
func (s *runtimeTestStorage) List(context.Context, string) ([]storage.FileInfo, error) { return nil, nil }
func (s *runtimeTestStorage) Close() error                                             { return nil }
func (s *runtimeTestStorage) GetKey() string                                           { return s.key }

func TestStorageSnapshotSwitchIsAtomic(t *testing.T) {
    server := &Server{}
    local := &runtimeTestStorage{key: "local"}
    s3 := &runtimeTestStorage{key: "s3"}
    localStatus := StorageStatus{DesiredType: "s3", ActiveType: "local", Degraded: true, FallbackActive: true}
    s3Status := StorageStatus{DesiredType: "s3", ActiveType: "s3", LastCheckTime: time.Now()}
    server.activateStorage(local, localStatus)

    const iterations = 1_000
    var wait sync.WaitGroup
    wait.Add(2)
    go func() {
        defer wait.Done()
        for range iterations {
            server.activateStorage(s3, s3Status)
            server.activateStorage(local, localStatus)
        }
    }()
    go func() {
        defer wait.Done()
        for range iterations * 2 {
            backend, status := server.loadStorageSnapshot()
            if backend == nil || backend.GetKey() != status.ActiveType {
                t.Errorf("inconsistent snapshot: backend=%v status=%+v", backend, status)
                return
            }
        }
    }()
    wait.Wait()
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./ -run '^TestStorageSnapshotSwitchIsAtomic$' -count=1`

Expected: FAIL，提示 `StorageStatus`、`activateStorage`、`loadStorageSnapshot` 未定义。

- [ ] **Step 3: 实现一个 immutable atomic snapshot**

`storage_runtime.go`：

```go
package m7s

import (
    "errors"
    "sync/atomic"
    "time"

    "m7s.live/v5/pkg/storage"
)

type StorageStatus struct {
    DesiredType   string    `json:"desiredType"`
    ActiveType    string    `json:"activeType"`
    Degraded      bool      `json:"degraded"`
    FallbackActive bool     `json:"fallbackActive"`
    LastCheckTime time.Time `json:"lastCheckTime"`
    LastError     string    `json:"lastError,omitempty"`
}

type storageSnapshot struct {
    backend storage.Storage
    status  StorageStatus
}

type storageRuntime struct {
    snapshot atomic.Pointer[storageSnapshot]
    registry *storage.Registry
}

func (s *Server) activateStorage(backend storage.Storage, status StorageStatus) {
    if s.storageRuntime == nil {
        s.storageRuntime = &storageRuntime{}
    }
    s.storageRuntime.snapshot.Store(&storageSnapshot{backend: backend, status: status})
}

func (s *Server) loadStorageSnapshot() (storage.Storage, StorageStatus) {
    if s.storageRuntime == nil {
        return nil, StorageStatus{}
    }
    snapshot := s.storageRuntime.snapshot.Load()
    if snapshot == nil {
        return nil, StorageStatus{}
    }
    return snapshot.backend, snapshot.status
}

func (s *Server) GetStorage() storage.Storage {
    backend, _ := s.loadStorageSnapshot()
    return backend
}

func (s *Server) GetStorageStatus() StorageStatus {
    _, status := s.loadStorageSnapshot()
    return status
}

func storageErrorSummary(err error) string {
    switch {
    case err == nil:
        return ""
    case errors.Is(err, storage.ErrUnsupportedStorageType):
        return "configured storage type is unavailable in this build"
    case errors.Is(err, storage.ErrStorageTypeNotConfigured):
        return "record storage type is no longer configured"
    default:
        return "configured storage is temporarily unavailable"
    }
}

func newStorageStatus(desired, active string, degraded, fallback bool, checkedAt time.Time, err error) StorageStatus {
    return StorageStatus{
        DesiredType: desired, ActiveType: active, Degraded: degraded,
        FallbackActive: fallback, LastCheckTime: checkedAt, LastError: storageErrorSummary(err),
    }
}
```

上述 import 集合可以直接编译；Task 3 增加错误包装时再引入 `fmt`。

`server.go` 的 `Server` struct：删除公开字段 `Storage storage.Storage`，新增：

```go
storageRuntime *storageRuntime
```

- [ ] **Step 4: 临时改造 initStorage 以保持旧行为并使用 snapshot**

在 Task 5 重写状态机前，先让 `server.go:initStorage` 使用 registry 和原子存储，但保持“失败 fallback local”的旧语义：

```go
func (s *Server) initStorage() {
    s.storageRuntime = &storageRuntime{registry: storage.NewRegistry(s.ServerConfig.Storage)}
    for storageType := range s.ServerConfig.Storage {
        st, err := s.storageRuntime.registry.GetOrCreate(storageType)
        if err == nil {
            s.activateStorage(st, newStorageStatus(storageType, storageType, false, false, time.Now(), nil))
            s.Info("global storage created", "type", storageType)
            return
        }
        s.Warn("create storage failed", "type", storageType, "err", err)
    }
    st, err := s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeLocal))
    if err != nil {
        s.Error("fallback local storage failed", "err", err)
        return
    }
    s.activateStorage(st, newStorageStatus("local", "local", false, false, time.Now(), nil))
    s.Info("fallback to local storage", "path", ".")
}
```

`server.go:Dispose` 在关闭 DB 后追加：

```go
if s.storageRuntime != nil && s.storageRuntime.registry != nil {
    if err := s.storageRuntime.registry.Close(); err != nil {
        s.Error("close storage registry failed", "err", err)
    }
}
```

- [ ] **Step 5: 机械迁移全部 `Server.Storage` 读取点**

精确替换如下，禁止保留裸字段访问：

```text
recordJob.Plugin.Server.Storage         -> recordJob.Plugin.Server.GetStorage()
u.s.Storage                              -> u.s.GetStorage()
s.Storage（api.go Server 方法）          -> s.GetStorage()
gb.Server.Storage                        -> gb.Server.GetStorage()
pullJob.Plugin.Server.Storage            -> pullJob.Plugin.Server.GetStorage()
p.Server.Storage                         -> p.Server.GetStorage()
```

对链式调用先保存局部变量并判 nil，例如 `plugin/gb28181/download_handler.go`：

```go
st := gb.Server.GetStorage()
if st == nil {
    http.Error(w, storage.ErrStorageNotAvailable.Error(), http.StatusServiceUnavailable)
    return
}
url, err := st.GetURL(r.Context(), filePath)
```

用下面的只读检查确保没有漏点：

Run: `rg -n '\.Storage([.(]|\s*=)' --glob '*.go' --glob '!pkg/storage/**' .`

Expected: 只剩配置字段 `ServerConfig.Storage`、结构体字面量配置和测试夹具；不能再出现运行期 `Server.Storage` 读取/赋值。

- [ ] **Step 6: 运行 atomic/race 测试和受影响包编译**

Run: `gofmt -w storage_runtime.go storage_runtime_test.go server.go recoder.go upload_retry.go api.go plugin/gb28181/download_handler.go plugin/flv/pkg/pull-recorder.go plugin/mp4/api.go plugin/mp4/exception.go plugin/mp4/pkg/pull-recorder.go plugin/snap/api.go && go test -race ./ -run '^TestStorageSnapshotSwitchIsAtomic$' -count=1`

Expected: PASS，无 data race。

Run: `go test ./plugin/mp4/... ./plugin/gb28181/... ./plugin/flv/... ./plugin/snap/... -run '^$' -count=1`

Expected: 所有目标包编译成功；若命中仓库预存在的 `plugin/crypto/pkg/transform.go` 错误，记录为无关已知问题，不改 crypto。

- [ ] **Step 7: 提交 Task 2**

Run: `git add storage_runtime.go storage_runtime_test.go server.go recoder.go upload_retry.go api.go plugin/gb28181/download_handler.go plugin/flv/pkg/pull-recorder.go plugin/mp4/api.go plugin/mp4/exception.go plugin/mp4/pkg/pull-recorder.go plugin/snap/api.go && git commit -m "refactor(storage): make active backend switching atomic"`

---

### Task 3: 按记录 `storage_type` 解析后端

**Files:**
- Modify: `storage_runtime.go`
- Modify: `storage_runtime_test.go`

**Interfaces:**
- Consumes: Task 1 Registry、Task 2 snapshot。
- Produces: `(*Server).GetStorageForType(string) (storage.Storage, error)`，供 MP4 与 DeleteRecord 使用。

- [ ] **Step 1: 写“active local 仍解析对象记录”的失败测试**

追加到 `storage_runtime_test.go`：

```go
func TestGetStorageForTypeDoesNotDependOnActiveBackend(t *testing.T) {
    const objectType = "storage-runtime-object-test"
    original, existed := storage.Factory[objectType]
    t.Cleanup(func() {
        if existed {
            storage.Factory[objectType] = original
        } else {
            delete(storage.Factory, objectType)
        }
    })
    objectBackend := &runtimeTestStorage{key: objectType}
    storage.Factory[objectType] = func(any) (storage.Storage, error) { return objectBackend, nil }

    server := &Server{storageRuntime: &storageRuntime{registry: storage.NewRegistry(map[string]any{objectType: struct{}{}})}}
    server.activateStorage(&runtimeTestStorage{key: "local"}, StorageStatus{DesiredType: objectType, ActiveType: "local", Degraded: true, FallbackActive: true})

    resolved, err := server.GetStorageForType(objectType)
    if err != nil {
        t.Fatal(err)
    }
    if resolved != objectBackend {
        t.Fatalf("resolved %p, expected %p", resolved, objectBackend)
    }
    if server.GetStorage().GetKey() != "local" {
        t.Fatal("resolving a historical backend must not change active storage")
    }
}

func TestGetStorageForTypeNormalizesLegacyLocal(t *testing.T) {
    server := &Server{storageRuntime: &storageRuntime{registry: storage.NewRegistry(nil)}}
    for _, storageType := range []string{"", "local"} {
        resolved, err := server.GetStorageForType(storageType)
        if err != nil {
            t.Fatalf("type %q: %v", storageType, err)
        }
        if resolved.GetKey() != "local" {
            t.Fatalf("type %q resolved to %q", storageType, resolved.GetKey())
        }
    }
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./ -run 'TestGetStorageForType' -count=1`

Expected: FAIL，提示方法未定义。

- [ ] **Step 3: 实现统一 resolver**

追加到 `storage_runtime.go`：

```go
func normalizeRecordStorageType(storageType string) string {
    if storageType == "" {
        return string(storage.StorageTypeLocal)
    }
    return storageType
}

func (s *Server) GetStorageForType(storageType string) (storage.Storage, error) {
    normalized := normalizeRecordStorageType(storageType)
    if s.storageRuntime == nil || s.storageRuntime.registry == nil {
        return nil, storage.ErrStorageNotAvailable
    }
    resolved, err := s.storageRuntime.registry.GetOrCreate(normalized)
    if err != nil {
        return nil, fmt.Errorf("resolve record storage %s: %w", normalized, err)
    }
    return resolved, nil
}
```

此处可以使用 `fmt`；底层错误原文只在内部 error chain 中保留，进入 HTTP/日志前必须经固定摘要脱敏。

- [ ] **Step 4: 运行测试与 race**

Run: `gofmt -w storage_runtime.go storage_runtime_test.go && go test -race ./ -run 'TestGetStorageForType|TestStorageSnapshot' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交 Task 3**

Run: `git add storage_runtime.go storage_runtime_test.go && git commit -m "feat(storage): resolve record backends by storage type"`

---

### Task 4: 修复 MP4 下载、范围 Demux 与物理删除

**Files:**
- Create: `plugin/mp4/api_storage_test.go`
- Create: `plugin/mp4/pkg/demux_range_storage_test.go`
- Modify: `plugin/mp4/api.go:60-240, 462-545`
- Modify: `plugin/mp4/pkg/demux-range.go:23-143`
- Modify: `plugin/mp4/pkg/pull-recorder.go:60-70`
- Modify: `api.go:985-1034`

**Interfaces:**
- Consumes: Task 3 `Server.GetStorageForType`。
- Produces: MP4 单文件/范围下载、DemuxRange、DeleteRecord 都按记录类型访问后端；不再出现 `storage type mismatch` 分支。

- [ ] **Step 1: 抽取并测试 MP4 record storage resolver**

在 `plugin/mp4/api_storage_test.go` 用 package `plugin_mp4`（与 `api.go` 相同，不能写 `plugin_mp4_test`）写测试。为避免构造完整 S3，注入一个 fake resolver，其 `GetURL` 返回 `https://object.invalid/record.mp4`；测试应返回 302 而不是 500：

```go
package plugin_mp4

import (
    "context"
    "net/http"
    "net/http/httptest"
    "testing"

    m7s "m7s.live/v5"
    "m7s.live/v5/pkg/storage"
)

type redirectStorage struct{ key string }

func (s *redirectStorage) CreateFile(context.Context, string) (storage.File, error) { return nil, nil }
func (s *redirectStorage) OpenFile(context.Context, string) (storage.File, error)   { return nil, nil }
func (s *redirectStorage) Delete(context.Context, string) error                    { return nil }
func (s *redirectStorage) Exists(context.Context, string) (bool, error)             { return true, nil }
func (s *redirectStorage) GetSize(context.Context, string) (int64, error)           { return 1, nil }
func (s *redirectStorage) GetURL(context.Context, string) (string, error)           { return "https://object.invalid/record.mp4", nil }
func (s *redirectStorage) List(context.Context, string) ([]storage.FileInfo, error) { return nil, nil }
func (s *redirectStorage) Close() error                                             { return nil }
func (s *redirectStorage) GetKey() string                                           { return s.key }

func TestDownloadSingleFileResolvesRecordStorageInsteadOfActiveStorage(t *testing.T) {
    const objectType = "mp4-object-test"
    objectBackend := &redirectStorage{key: objectType}
    var resolvedType string
    resolver := func(storageType string) (storage.Storage, error) {
        resolvedType = storageType
        return objectBackend, nil
    }
    plugin := &MP4Plugin{}
    stream := &m7s.RecordStream{StorageType: objectType, FilePath: "record.mp4"}
    request := httptest.NewRequest(http.MethodGet, "/mp4/download/live/test?id=1", nil)
    response := httptest.NewRecorder()

    plugin.downloadSingleFileWithResolver(resolver, stream, 0, response, request)

    if resolvedType != objectType {
        t.Fatalf("resolved type=%q, want %q", resolvedType, objectType)
    }
    if response.Code != http.StatusFound {
        t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
    }
    if response.Header().Get("Location") != "https://object.invalid/record.mp4" {
        t.Fatalf("location=%q", response.Header().Get("Location"))
    }
}
```

生产方法保留现有签名，内部只负责注入真实 resolver：

```go
func (p *MP4Plugin) downloadSingleFile(stream *m7s.RecordStream, flag mp4.Flag, w http.ResponseWriter, r *http.Request) {
    p.downloadSingleFileWithResolver(p.Server.GetStorageForType, *stream, flag, w, r)
}
```

测试直接调用 `downloadSingleFileWithResolver`，无需导出 Server 内部状态，也不修改全局 Factory。

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./plugin/mp4 -run '^TestDownloadSingleFileResolvesRecordStorageInsteadOfActiveStorage$' -count=1`

Expected: FAIL；当前代码读取 active local，与记录类型不等，返回 500。

- [ ] **Step 3: 改造 `downloadSingleFile`**

把原方法主体移动到以下可注入 helper，原方法仅按上一步代码传入 `p.Server.GetStorageForType`：

```go
type recordStorageResolver func(string) (storage.Storage, error)

func (p *MP4Plugin) downloadSingleFileWithResolver(resolve recordStorageResolver, stream m7s.RecordStream, flag mp4.Flag, w http.ResponseWriter, r *http.Request) {
    // 保留绝对路径和 HTTP URL 的既有处理；相对路径按下面逻辑解析。
}
```

在相对路径分支最前面统一解析：

```go
st, resolveErr := p.Server.GetStorageForType(stream.StorageType)
isLocalStorage := stream.StorageType == "" || stream.StorageType == string(storage.StorageTypeLocal)
if resolveErr != nil {
    p.Error("resolve record storage failed", "storageType", stream.StorageType, "error", m7s.StorageErrorSummary(resolveErr))
    http.Error(w, "record storage is temporarily unavailable", http.StatusServiceUnavailable)
    return
}
```

删除 `globalStorageType`、`useGlobalStorage` 以及两处 `storage type mismatch`。普通 MP4：local 继续 `http.ServeFile`；对象存储继续 `GetURL` + 302。fMP4：local 继续 `OpenFileFromStorageLevel`，对象存储必须直接：

```go
file, err = st.OpenFile(r.Context(), stream.FilePath)
```

不要先 `GetURL` 再把预签名 URL 传给 `OpenFile`；`S3Storage.OpenFile` 接收 object key。

- [ ] **Step 4: 改造范围合并下载循环**

在 `plugin/mp4/api.go:462-545` 每个 `stream` 的相对路径分支调用：

```go
st, resolveErr := p.Server.GetStorageForType(stream.StorageType)
if resolveErr != nil {
    p.Error("resolve record storage failed", "storageType", stream.StorageType, "error", m7s.StorageErrorSummary(resolveErr))
    http.Error(w, "record storage is temporarily unavailable", http.StatusServiceUnavailable)
    return
}
```

local 用 `OpenFileFromStorageLevel`；对象存储直接 `st.OpenFile(r.Context(), stream.FilePath)`。删除 mismatch 分支和 `GetURL`→`OpenFile(url)` 组合。

- [ ] **Step 5: 写 DemuxRange resolver 失败测试**

`plugin/mp4/pkg/demux_range_storage_test.go` 构造一个 resolver，记录收到的 StorageType，并返回 fake File；测试重点不是完整 MP4 解复用，而是抽取一个新 helper `DemuxerRange.openRecordFile` 后直接测试：

```go
package mp4

import (
    "context"
    "os"
    "path/filepath"
    "testing"

    m7s "m7s.live/v5"
    "m7s.live/v5/pkg/storage"
)

type openFileStorage struct {
    key  string
    file storage.File
}

func (s *openFileStorage) CreateFile(context.Context, string) (storage.File, error) { return nil, nil }
func (s *openFileStorage) OpenFile(context.Context, string) (storage.File, error)   { return s.file, nil }
func (s *openFileStorage) Delete(context.Context, string) error                    { return nil }
func (s *openFileStorage) Exists(context.Context, string) (bool, error)             { return true, nil }
func (s *openFileStorage) GetSize(context.Context, string) (int64, error)           { return 0, nil }
func (s *openFileStorage) GetURL(context.Context, string) (string, error)           { return "", nil }
func (s *openFileStorage) List(context.Context, string) ([]storage.FileInfo, error) { return nil, nil }
func (s *openFileStorage) Close() error                                             { return nil }
func (s *openFileStorage) GetKey() string                                           { return s.key }

func TestOpenRecordFileUsesRecordStorageType(t *testing.T) {
    path := filepath.Join(t.TempDir(), "record.mp4")
    if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
        t.Fatal(err)
    }
    osFile, err := os.Open(path)
    if err != nil {
        t.Fatal(err)
    }
    expectedFile := &storage.LocalFile{File: osFile}
    var resolvedType string
    demuxer := &DemuxerRange{StorageResolver: func(storageType string) (storage.Storage, error) {
        resolvedType = storageType
        return &openFileStorage{file: expectedFile, key: storageType}, nil
    }}
    stream := m7s.RecordStream{StorageType: "s3", FilePath: "records/a.mp4"}

    file, cleanup, err := demuxer.openRecordFile(context.Background(), stream)
    if err != nil {
        t.Fatal(err)
    }
    defer cleanup()
    if resolvedType != "s3" || file != expectedFile {
        t.Fatalf("resolvedType=%q file=%p", resolvedType, file)
    }
}
```

这里用临时文件实现完整的 `storage.File` seeker 契约，不引入内存文件或额外测试依赖。

- [ ] **Step 6: 运行 DemuxRange 测试确认 RED**

Run: `go test ./plugin/mp4/pkg -run '^TestOpenRecordFileUsesRecordStorageType$' -count=1`

Expected: FAIL，提示 `StorageResolver` / `openRecordFile` 未定义。

- [ ] **Step 7: 实现 DemuxRange.openRecordFile 并注入 resolver**

`DemuxerRange` 新字段：

```go
StorageResolver func(string) (storage.Storage, error)
```

新增：

```go
func (d *DemuxerRange) openRecordFile(ctx context.Context, stream m7s.RecordStream) (storage.File, func(), error) {
    if strings.HasPrefix(stream.FilePath, "http://") || strings.HasPrefix(stream.FilePath, "https://") {
        return d.downloadRemoteFile(ctx, stream.FilePath)
    }
    if filepath.IsAbs(stream.FilePath) {
        file, err := os.Open(stream.FilePath)
        if err != nil {
            return nil, func() {}, err
        }
        return &storage.LocalFile{File: file}, func() { file.Close() }, nil
    }
    if d.StorageResolver == nil {
        return nil, func() {}, storage.ErrStorageNotAvailable
    }
    st, err := d.StorageResolver(stream.StorageType)
    if err != nil {
        return nil, func() {}, err
    }
    if stream.StorageType == "" || stream.StorageType == string(storage.StorageTypeLocal) {
        if local, ok := st.(*storage.LocalStorage); ok {
            file, openErr := os.Open(local.GetFullPath(stream.FilePath, stream.StorageLevel))
            if openErr != nil {
                return nil, func() {}, openErr
            }
            return &storage.LocalFile{File: file}, func() { file.Close() }, nil
        }
    }
    file, err := st.OpenFile(ctx, stream.FilePath)
    if err != nil {
        return nil, func() {}, err
    }
    return file, func() { file.Close() }, nil
}
```

将 `Demux()` 里 36-42 和 97-134 的全局类型比较整体替换为 `openRecordFile`；失败时记录：

```go
d.Error("open record segment failed", "storageType", stream.StorageType, "path", stream.FilePath, "error", m7s.StorageErrorSummary(err))
continue
```

注意日志只记录对象 key，不记录预签名 URL。`plugin/mp4/pkg/pull-recorder.go` 构造 `DemuxerRange` 时设置：

```go
StorageResolver: pullJob.Plugin.Server.GetStorageForType,
```

- [ ] **Step 8: 改造 DeleteRecord 物理删除**

根 `api.go:deletePhysicalFile` 删除全局 key 比较，改成：

```go
st, err := s.GetStorageForType(recordFile.StorageType)
if err != nil {
    s.Error("resolve record storage for delete failed",
        "storageType", recordFile.StorageType,
        "error", storageErrorSummary(err))
    return newSanitizedStorageError("record storage unavailable", err)
}
if recordFile.StorageType == "" || recordFile.StorageType == string(storage.StorageTypeLocal) {
    if local, ok := st.(*storage.LocalStorage); ok {
        return os.Remove(local.GetFullPath(filePath, recordFile.StorageLevel))
    }
}
if err := st.Delete(ctx, filePath); err != nil {
    s.Error("delete record storage file failed",
        "storageType", recordFile.StorageType,
        "error", storageErrorSummary(err))
    return newSanitizedStorageError("record storage operation failed", err)
}
return nil
```

保持现有数据库事务/物理删除顺序不变；只修后端解析。

- [ ] **Step 9: 搜索并禁止 mismatch 旧逻辑**

Run: `rg -n 'storage type mismatch|useGlobalStorage|globalStorageType' plugin/mp4 api.go`

Expected: 无输出。

- [ ] **Step 10: 跑 MP4 聚焦测试**

Run: `gofmt -w plugin/mp4/api.go plugin/mp4/api_storage_test.go plugin/mp4/pkg/demux-range.go plugin/mp4/pkg/demux_range_storage_test.go plugin/mp4/pkg/pull-recorder.go api.go && go test ./plugin/mp4/... -run 'TestDownloadSingleFileResolvesRecordStorage|TestOpenRecordFileUsesRecordStorageType' -count=1`

Expected: PASS。

Run: `go test -tags s3 ./pkg/storage/... ./plugin/mp4/... -count=1`

Expected: PASS；不连接真实 S3，因为测试 factory 是 fake。

- [ ] **Step 11: 提交 Task 4**

Run: `git add plugin/mp4/api.go plugin/mp4/api_storage_test.go plugin/mp4/pkg/demux-range.go plugin/mp4/pkg/demux_range_storage_test.go plugin/mp4/pkg/pull-recorder.go api.go && git commit -m "fix(mp4): resolve recordings by persisted storage type"`

---

### Task 5: S3 启动状态机与显式临时 fallback

> **已被顶部“Task 5 最终修订”覆盖：** 下方 Step 8-13 的 `task.Task.Run`、native retry 与挂入 `Records` 的代码仅保留为历史，最终实现必须是 Server-owned `StorageReconnectWork` + 一秒 `TickTask` + 自管截止时间退避。

**Files:**
- Create: `storage_reconnect.go`
- Create: `storage_reconnect_test.go`
- Modify: `server.go:60-84, 301, 438-451, 841-859`
- Modify: `storage_runtime.go`
- Modify: `pkg/storage/retry.go:33-50`
- Create: `pkg/storage/retry_test.go`

**Interfaces:**
- Consumes: Task 1 Registry、Task 2 snapshot。
- Produces: `StorageAllowLocalFallback bool` 配置、专属 `StorageReconnectWork`、`StorageReconnectTask` TickTask、S3 degraded/fallback/recovered 状态机。

- [ ] **Step 1: 给启动状态机写表驱动失败测试**

`storage_reconnect_test.go` 先定义固定 S3 factory 的保存/恢复 helper；测试不调用 `t.Parallel()`，避免污染全局 Factory：

```go
func installS3TestFactory(t *testing.T, factory func(any) (storage.Storage, error)) {
    t.Helper()
    original, existed := storage.Factory["s3"]
    storage.Factory["s3"] = factory
    t.Cleanup(func() {
        if existed {
            storage.Factory["s3"] = original
        } else {
            delete(storage.Factory, "s3")
        }
    })
}

func newS3InitTestServer(t *testing.T, allowFallback bool, s3Error error) *Server {
    t.Helper()
    installS3TestFactory(t, func(any) (storage.Storage, error) {
        if s3Error != nil {
            return nil, s3Error
        }
        return &runtimeTestStorage{key: "s3"}, nil
    })
    return &Server{ServerConfig: ServerConfig{
        Storage: map[string]any{"s3": struct{}{}},
        StorageAllowLocalFallback: allowFallback,
    }}
}
```

表格覆盖：

```go
func TestInitS3StorageFallbackPolicy(t *testing.T) {
    tests := []struct {
        name             string
        allowFallback    bool
        s3Error          error
        wantActive       string
        wantDegraded     bool
        wantFallback     bool
    }{
        {name: "s3 ready", s3Error: nil, wantActive: "s3"},
        {name: "default blocks local fallback", s3Error: errors.New("connection refused"), wantActive: "s3", wantDegraded: true},
        {name: "explicit fallback is degraded local", allowFallback: true, s3Error: errors.New("connection refused"), wantActive: "local", wantDegraded: true, wantFallback: true},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            server := newS3InitTestServer(t, test.allowFallback, test.s3Error)
            server.initStorage()
            backend, status := server.loadStorageSnapshot()
            if backend.GetKey() != test.wantActive || status.Degraded != test.wantDegraded || status.FallbackActive != test.wantFallback {
                t.Fatalf("backend=%q status=%+v", backend.GetKey(), status)
            }
        })
    }
}

func TestInitStorageWithoutS3RemainsHealthyLocal(t *testing.T) {
    server := &Server{ServerConfig: ServerConfig{Storage: nil}}
    server.initStorage()
    backend, status := server.loadStorageSnapshot()
    if backend.GetKey() != "local" || status.Degraded || status.FallbackActive {
        t.Fatalf("backend=%q status=%+v", backend.GetKey(), status)
    }
}
```

默认禁止 fallback 的 unavailable backend `GetKey()` 约定返回期望类型 `s3`；状态的 `ActiveType` 也填 `s3`，但 `Degraded=true` 明确它不可用。

- [ ] **Step 2: 运行启动测试确认 RED**

Run: `go test ./ -run 'TestInitS3StorageFallbackPolicy|TestInitStorageWithoutS3RemainsHealthyLocal' -count=1`

Expected: 至少“默认禁止 fallback”和“显式 fallback 仍 degraded”失败。

- [ ] **Step 3: 添加配置字段并重写 initStorage**

`ServerConfig` 在 `Storage` 后新增：

```go
StorageAllowLocalFallback bool `default:"false" desc:"配置的S3不可用时是否临时使用本地存储;启用后仍保持降级状态并后台重连"`
```

YAML key 按配置系统规则为全小写 `storageallowlocalfallback`。

`initStorage` 按以下固定分支重写；只有配置 key `s3` 进入新恢复语义，OSS/COS 继续 legacy 初始化路径，不做本次承诺：

```go
func (s *Server) initStorage() {
    s.storageRuntime = &storageRuntime{registry: storage.NewRegistry(s.ServerConfig.Storage)}
    _, hasS3 := s.ServerConfig.Storage[string(storage.StorageTypeS3)]
    if !hasS3 {
        s.initLegacyStorage()
        return
    }

    checkedAt := time.Now()
    st, err := s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeS3))
    if err == nil {
        s.activateStorage(st, newStorageStatus("s3", "s3", false, false, checkedAt, nil))
        s.Info("global storage created", "type", "s3")
        return
    }

    if s.StorageAllowLocalFallback {
        local, localErr := s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeLocal))
        if localErr != nil {
            s.Error("create fallback local storage failed", "err", localErr)
            s.activateStorage(storage.NewUnavailableStorage("s3"), newStorageStatus("s3", "s3", true, false, checkedAt, err))
        } else {
            s.activateStorage(local, newStorageStatus("s3", "local", true, true, checkedAt, err))
            s.Warn("S3 unavailable, temporary local fallback active", "type", "s3")
        }
    } else {
        s.activateStorage(storage.NewUnavailableStorage("s3"), newStorageStatus("s3", "s3", true, false, checkedAt, err))
        s.Error("S3 unavailable, recording disabled until recovery", "type", "s3")
    }
    s.scheduleStorageReconnect()
}
```

`initLegacyStorage()` 保存原有无 S3 行为；空配置时 desired/active 都是 local 且 healthy。显式配置 OSS/COS 的原有行为不在本次改动中扩展。

- [ ] **Step 4: 运行状态机测试确认 GREEN**

Run: `gofmt -w server.go storage_reconnect_test.go && go test ./ -run 'TestInitS3StorageFallbackPolicy|TestInitStorageWithoutS3RemainsHealthyLocal' -count=1`

Expected: PASS。

- [ ] **Step 5: 补齐 S3 连接永久错误分类测试**

`pkg/storage/retry_test.go`：

```go
func TestIsPermanentConnectionError(t *testing.T) {
    tests := []struct {
        message   string
        permanent bool
    }{
        {message: "AccessDenied", permanent: true},
        {message: "InvalidAccessKeyId", permanent: true},
        {message: "SignatureDoesNotMatch", permanent: true},
        {message: "connection refused", permanent: false},
        {message: "i/o timeout", permanent: false},
        {message: "NoSuchBucket", permanent: false},
    }
    for _, test := range tests {
        if got := IsPermanentConnectionError(errors.New(test.message)); got != test.permanent {
            t.Errorf("%q: got %v want %v", test.message, got, test.permanent)
        }
    }
}
```

`NoSuchBucket` 对上传仍是 permanent，但对启动恢复视为 retryable，允许 MinIO API ready 后由初始化任务创建 bucket。

- [ ] **Step 6: 运行错误分类测试确认 RED**

Run: `go test ./pkg/storage -run '^TestIsPermanentConnectionError$' -count=1`

Expected: FAIL，函数未定义。

- [ ] **Step 7: 实现连接错误分类**

`pkg/storage/retry.go` 新增：

```go
func IsPermanentConnectionError(err error) bool {
    if err == nil {
        return false
    }
    message := err.Error()
    permanentPatterns := []string{
        "AccessDenied", "Forbidden", "InvalidAccessKeyId", "SignatureDoesNotMatch",
        "InvalidBucketName", "MalformedXML", "InvalidObjectName",
    }
    for _, pattern := range permanentPatterns {
        if strings.Contains(message, pattern) {
            return true
        }
    }
    return false
}
```

复用文件现有 `strings` import；不要改变 `IsPermanentError` 的上传语义。

- [ ] **Step 8: 写重连任务失败/恢复测试**

追加 `storage_reconnect_test.go`：

```go
func TestStorageReconnectRunKeepsFallbackDegradedUntilSuccess(t *testing.T) {
    var attempts atomic.Int32
    recovered := &runtimeTestStorage{key: "s3"}
    installS3TestFactory(t, func(any) (storage.Storage, error) {
        if attempts.Add(1) == 1 {
            return nil, errors.New("connection refused")
        }
        return recovered, nil
    })
    local, err := storage.NewLocalStorage(t.TempDir())
    if err != nil {
        t.Fatal(err)
    }
    server := &Server{storageRuntime: &storageRuntime{
        registry: storage.NewRegistry(map[string]any{"s3": struct{}{}}),
    }}
    server.activateStorage(local, newStorageStatus("s3", "local", true, true, time.Now(), errors.New("connection refused")))
    taskUnderTest := &StorageReconnectTask{s: server}

    if err := taskUnderTest.Run(); err == nil {
        t.Fatal("first run must request retry")
    }
    if current := server.GetStorageStatus(); !current.Degraded || !current.FallbackActive || server.GetStorage().GetKey() != "local" {
        t.Fatalf("first status=%+v active=%s", current, server.GetStorage().GetKey())
    }
    if err := taskUnderTest.Run(); !errors.Is(err, task.ErrTaskComplete) {
        t.Fatalf("second run=%v", err)
    }
    if current := server.GetStorageStatus(); current.Degraded || current.FallbackActive || server.GetStorage() != recovered {
        t.Fatalf("recovered status=%+v active=%s", current, server.GetStorage().GetKey())
    }
}
```

该测试直接构造 registry 和 fallback snapshot：第一次 task `Run` 失败后保持 local/degraded，第二次成功后切换为同一个 recovered S3 实例。

- [ ] **Step 9: 运行重连测试确认 RED**

Run: `go test ./ -run '^TestStorageReconnectRunKeepsFallbackDegradedUntilSuccess$' -count=1`

Expected: FAIL，`StorageReconnectTask` 未定义。

- [ ] **Step 10: 实现 gotask 重连任务**

`storage_reconnect.go`：

```go
package m7s

import (
    "errors"
    "time"

    task "github.com/eanfs/gotask"
    "m7s.live/v5/pkg/config"
    "m7s.live/v5/pkg/storage"
)

const (
    storageReconnectBaseInterval = 5 * time.Second
    storageReconnectMaxInterval  = 5 * time.Minute
)

type StorageReconnectTask struct {
    task.Task
    s *Server
}

func newStorageReconnectTask(s *Server) *StorageReconnectTask {
    reconnect := &StorageReconnectTask{s: s}
    reconnect.SetRetry(-1, storageReconnectBaseInterval)
    reconnect.GetTask().SetMaxRetryInterval(storageReconnectMaxInterval)
    return reconnect
}

func (t *StorageReconnectTask) Run() error {
    st, err := t.s.storageRuntime.registry.GetOrCreate(string(storage.StorageTypeS3))
    checkedAt := time.Now()
    if err != nil {
        current := t.s.GetStorageStatus()
        t.s.activateStorage(t.s.GetStorage(), newStorageStatus("s3", current.ActiveType, true, current.FallbackActive, checkedAt, err))
        RaiseUploadAlarm(t.s.DB, config.AlarmStorageException, "S3 storage unavailable", "", "s3", storageErrorSummary(err))
        t.Warn("S3 reconnect failed", "err", err)
        if errors.Is(err, storage.ErrUnsupportedStorageType) || storage.IsPermanentConnectionError(err) {
            return task.ErrTaskComplete
        }
        return err
    }

    t.s.activateStorage(st, newStorageStatus("s3", "s3", false, false, checkedAt, nil))
    RaiseUploadAlarm(t.s.DB, config.AlarmStorageExceptionRecover, "S3 storage recovered", "", "s3", "S3 storage is ready")
    t.Info("S3 storage recovered", "type", "s3")
    return task.ErrTaskComplete
}

func (s *Server) scheduleStorageReconnect() {
    reconnect := newStorageReconnectTask(s)
    s.Records.OnStart(func() {
        s.Records.AddTask(reconnect)
    })
}
```

关键：`scheduleStorageReconnect()` 在 `initStorage()` 中执行，而 `initStorage()` 早于 `s.AddTask(&s.Records)`，所以回调不会错过。

- [ ] **Step 11: 修复已有 UploadRetryScheduler 的 OnStart 顺序陷阱**

把 `server.go:447-451` 的既有 block 移到 `s.AddTask(&s.Records)` **之前**：

```go
if s.DB != nil {
    s.Records.OnStart(func() {
        s.Records.AddTask(&UploadRetryScheduler{s: s})
    })
}
s.AddTask(&s.Records)
```

但注意此处原逻辑在 DB 初始化后才知道 `s.DB`。位置应放在 DB 初始化完成且仍早于 line 438 的 Records AddTask；不要把回调移到 `initStorage()`（那里 DB 尚未初始化）。

- [ ] **Step 12: 跑状态机、重连、错误分类和 race 测试**

Run: `gofmt -w server.go storage_reconnect.go storage_reconnect_test.go pkg/storage/retry.go pkg/storage/retry_test.go && go test -race ./pkg/storage ./ -run 'TestInitS3Storage|TestInitStorageWithoutS3|TestStorageReconnect|TestIsPermanentConnectionError' -count=1`

Expected: PASS，无 data race。

- [ ] **Step 13: 提交 Task 5**

Run: `git add server.go storage_reconnect.go storage_reconnect_test.go storage_runtime.go pkg/storage/retry.go pkg/storage/retry_test.go && git commit -m "fix(storage): recover S3 after startup race"`

---

### Task 6: 录像入口拒绝策略与 fallback 语义

**Files:**
- Create: `recoder_storage_test.go`
- Modify: `recoder.go:83-109`
- Modify: `plugin/mp4/api.go:710-771`

**Interfaces:**
- Consumes: `Server.GetStorageStatus()`、`Server.GetStorage()`。
- Produces: `(*Server).ValidateRecordingStorage() error`；degraded 且无 fallback 时 `codes.Unavailable`，degraded 且 fallback 时继续 local；HTTP StartRecord 在创建 `RecordJob` 前失败，不能留下随后自动重试的幽灵任务。

- [ ] **Step 1: 写纯判定 helper 的失败测试**

为避免单测构造完整 `RecordJob`，先在 `recoder.go` 抽取 `recordingStorage()` 并测试：

```go
func TestRecordingStorageRejectsDegradedWithoutFallback(t *testing.T) {
    server := &Server{}
    server.activateStorage(storage.NewUnavailableStorage("s3"), StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true})

    _, err := server.recordingStorage()
    if status.Code(err) != codes.Unavailable {
        t.Fatalf("code=%s err=%v", status.Code(err), err)
    }
}

func TestRecordingStorageAllowsExplicitFallback(t *testing.T) {
    local, err := storage.NewLocalStorage(t.TempDir())
    if err != nil {
        t.Fatal(err)
    }
    server := &Server{}
    server.activateStorage(local, StorageStatus{DesiredType: "s3", ActiveType: "local", Degraded: true, FallbackActive: true})

    resolved, err := server.recordingStorage()
    if err != nil || resolved.GetKey() != "local" {
        t.Fatalf("storage=%v err=%v", resolved, err)
    }
}
```

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./ -run '^TestRecordingStorage' -count=1`

Expected: FAIL，`recordingStorage` 未定义。

- [ ] **Step 3: 实现判定并接入 CreateStream**

`recoder.go`：

```go
func (s *Server) ValidateRecordingStorage() error {
    backend, current := s.loadStorageSnapshot()
    if current.Degraded && !current.FallbackActive {
        return status.Error(codes.Unavailable, "configured storage is not ready")
    }
    if backend == nil {
        return status.Error(codes.Unavailable, "storage is not initialized")
    }
    return nil
}

func (s *Server) recordingStorage() (storage.Storage, error) {
    if err := s.ValidateRecordingStorage(); err != nil {
        return nil, err
    }
    return s.GetStorage(), nil
}
```

在 `DefaultRecorder.CreateStream` 用：

```go
recordJob.storage, err = recordJob.Plugin.Server.recordingStorage()
if err != nil {
    return err
}
storageType := recordJob.storage.GetKey()
```

删除原 `recordJob.storage == nil` 的裸 `fmt.Errorf("storage config is required")`，避免 grpc-gateway 映射成 500。不要在错误里包含 lastError，以免向客户端泄露 endpoint/bucket。

`plugin/mp4/api.go:StartRecord` 在检查 `recordExists`、查 Publisher、调用 `p.Record` **之前**增加：

```go
if err = p.Server.ValidateRecordingStorage(); err != nil {
    return nil, err
}
```

该预检确保一次返回 503 的手工 API 请求不会先创建带无限重试的 `RecordJob`，避免 S3 恢复后请求方不知情地自动开始录像；配置驱动的自动录像仍由 `CreateStream` 防线保护并保留原有 task retry 语义。

- [ ] **Step 4: 跑测试与 MP4 StartRecord 编译**

Run: `gofmt -w recoder.go recoder_storage_test.go plugin/mp4/api.go && go test ./ -run '^TestRecordingStorage' -count=1 && go test ./plugin/mp4 -run '^$' -count=1`

Expected: PASS。

- [ ] **Step 5: 提交 Task 6**

Run: `git add recoder.go recoder_storage_test.go plugin/mp4/api.go && git commit -m "fix(record): reject starts while required S3 is unavailable"`

---

### Task 7: 存储 readiness API 与告警可观测性

**Files:**
- Create: `storage_status_api_test.go`
- Modify: `api.go`
- Modify: `server.go:310-318`

**Interfaces:**
- Consumes: `Server.GetStorageStatus()`。
- Produces: `GET /api/storage/status`，ready=200，degraded=503。

- [ ] **Step 1: 写 readiness HTTP 失败测试**

`storage_status_api_test.go`：

```go
package m7s

import (
    "encoding/json"
    "errors"
    "net/http"
    "net/http/httptest"
    "strings"
    "testing"
)

func TestStorageStatusHTTPUsesReadinessStatusCode(t *testing.T) {
    tests := []struct {
        name     string
        status   StorageStatus
        wantCode int
    }{
        {name: "ready", status: StorageStatus{DesiredType: "s3", ActiveType: "s3"}, wantCode: http.StatusOK},
        {name: "degraded unavailable", status: StorageStatus{DesiredType: "s3", ActiveType: "s3", Degraded: true}, wantCode: http.StatusServiceUnavailable},
        {name: "degraded fallback", status: StorageStatus{DesiredType: "s3", ActiveType: "local", Degraded: true, FallbackActive: true}, wantCode: http.StatusServiceUnavailable},
    }
    for _, test := range tests {
        t.Run(test.name, func(t *testing.T) {
            server := &Server{}
            server.activateStorage(&runtimeTestStorage{key: test.status.ActiveType}, test.status)
            response := httptest.NewRecorder()
            server.GetStorageStatusHTTP(response, httptest.NewRequest(http.MethodGet, "/api/storage/status", nil))
            if response.Code != test.wantCode {
                t.Fatalf("code=%d body=%s", response.Code, response.Body.String())
            }
            var got StorageStatus
            if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
                t.Fatal(err)
            }
            if got.Degraded != test.status.Degraded || got.FallbackActive != test.status.FallbackActive {
                t.Fatalf("got=%+v want=%+v", got, test.status)
            }
        })
    }
}

func TestStorageStatusErrorIsSanitized(t *testing.T) {
    secret := "secret-access-key-must-not-appear"
    summary := storageErrorSummary(errors.New("connection refused: " + secret))
    if strings.Contains(summary, secret) {
        t.Fatalf("summary leaked secret: %q", summary)
    }
}
```

该测试文件的最终 import 只保留 `encoding/json`、`errors`、`net/http`、`net/http/httptest`、`strings`、`testing`。

- [ ] **Step 2: 运行测试确认 RED**

Run: `go test ./ -run 'TestStorageStatusHTTP|TestStorageStatusErrorIsSanitized' -count=1`

Expected: HTTP 测试 FAIL，handler 未定义；脱敏测试应已通过。

- [ ] **Step 3: 实现 readiness handler**

`api.go` 新增：

```go
func (s *Server) GetStorageStatusHTTP(w http.ResponseWriter, _ *http.Request) {
    current := s.GetStorageStatus()
    w.Header().Set("Content-Type", "application/json; charset=utf-8")
    if current.Degraded {
        w.WriteHeader(http.StatusServiceUnavailable)
    } else {
        w.WriteHeader(http.StatusOK)
    }
    if err := json.NewEncoder(w).Encode(current); err != nil {
        s.Error("encode storage status failed", "err", err)
    }
}
```

`server.go` handler map新增：

```go
"/api/storage/status": s.GetStorageStatusHTTP,
```

readiness 使用直接 HTTP handler，因此不改 proto、不生成 pb 文件。

- [ ] **Step 4: 验证状态字段与错误脱敏**

Run: `gofmt -w api.go server.go storage_status_api_test.go && go test ./ -run 'TestStorageStatusHTTP|TestStorageStatusErrorIsSanitized' -count=1`

Expected: PASS；degraded 两种分支均为 503。

- [ ] **Step 5: 确认日志/响应无凭据字段**

Run: `rg -n 'AccessKey|SecretAccess|Presign|RawQuery' storage_runtime.go storage_reconnect.go api.go storage_status_api_test.go`

Expected: 生产状态/告警代码无 access key、secret 或预签名 URL 输出；配置 struct 原定义不在本次检查范围。

- [ ] **Step 6: 提交 Task 7**

Run: `git add api.go server.go storage_status_api_test.go && git commit -m "feat(storage): expose degraded readiness status"`

---

### Task 8: 容器级 MinIO 启动竞态测试脚本

> **已被顶部“Task 8 最终修订”覆盖：** 下方固定 delay、host download、latest-row SQL、export 凭据、宽泛 COPY/ignore 和容错 cleanup 片段仅保留为历史，不能用于实现或验收最终六项 harness。

**Files（历史初稿；最终以顶部六项修订为准）:**
- Create: `example/record-test/Dockerfile.storage-race`
- Create: `example/record-test/Dockerfile.storage-race.dockerignore`
- Create: `example/record-test/docker-compose.storage-race.yml`
- Create: `example/record-test/config.storage-race.yaml.tmpl`
- Create: `example/record-test/scripts/verify_storage_race.sh`
- Modify: `example/record-test/.gitignore`

**Interfaces:**
- Consumes: `/api/storage/status`、MP4 start/stop/download API、SQLite `record_streams.storage_type`。
- Produces: 可重复 5 次的黑盒竞态验证；本次按安全边界只做语法/静态验证，未查询 Docker daemon 状态，也不宣称五轮实跑通过。

- [ ] **Step 1: 写专用测试镜像**

`example/record-test/Dockerfile.storage-race`：

```dockerfile
FROM golang:1.26 AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -tags "sqlite s3" -o /out/monibuca ./example/default

FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=build /out/monibuca /usr/local/bin/monibuca
ENTRYPOINT ["monibuca"]
CMD ["-c", "/run/monibuca/config.yaml"]
```

`sqlite` build tag 使用 `github.com/ncruces/go-sqlite3/embed`，保持 `CGO_ENABLED=0`；不要改成 `sqliteCGO`。

- [ ] **Step 2: 写无凭据配置模板**

`example/record-test/config.storage-race.yaml.tmpl`：

```yaml
global:
  http:
    listenaddr: :8080
  db:
    type: sqlite
    dsn: /data/storage-race.db
  storageallowlocalfallback: __ALLOW_LOCAL_FALLBACK__
  storage:
    s3:
      endpoint: http://minio:9000
      region: us-east-1
      accesskeyid: __MINIO_ROOT_USER__
      secretaccesskey: __MINIO_ROOT_PASSWORD__
      bucket: storage-race
      pathprefix: recordings
      forcepathstyle: true
      usessl: false
      connecttimeout: 2s
mp4:
  enable: true
rtmp:
  tcp:
    listenaddr: :1935
```

模板中只有占位符，不提交真实或固定测试凭据。

- [ ] **Step 3: 写 Compose 场景**

`example/record-test/docker-compose.storage-race.yml`：

```yaml
services:
  minio:
    image: quay.io/minio/minio:RELEASE.2025-07-23T15-54-02Z
    entrypoint: ["/bin/sh", "-c"]
    command: ["sleep $${MINIO_DELAY_SECONDS}; exec minio server /data --console-address :9001"]
    environment:
      MINIO_ROOT_USER: ${MINIO_ROOT_USER:?required}
      MINIO_ROOT_PASSWORD: ${MINIO_ROOT_PASSWORD:?required}
      MINIO_DELAY_SECONDS: ${MINIO_DELAY_SECONDS:-15}
    volumes:
      - ${STORAGE_RACE_MINIO_DIR:?required}:/data
    healthcheck:
      test: ["CMD", "curl", "-fsS", "http://localhost:9000/minio/health/ready"]
      interval: 2s
      timeout: 1s
      retries: 30
  minio-init:
    image: minio/mc:RELEASE.2025-07-21T05-28-08Z
    depends_on:
      minio:
        condition: service_healthy
    entrypoint: ["/bin/sh", "-c"]
    command: ["mc alias set race http://minio:9000 \"$${MINIO_ROOT_USER}\" \"$${MINIO_ROOT_PASSWORD}\" >/dev/null && mc mb --ignore-existing race/storage-race >/dev/null"]
    environment:
      MINIO_ROOT_USER: ${MINIO_ROOT_USER:?required}
      MINIO_ROOT_PASSWORD: ${MINIO_ROOT_PASSWORD:?required}
  monibuca:
    build:
      context: ../..
      dockerfile: example/record-test/Dockerfile.storage-race
    command: ["-c", "/run/monibuca/config.yaml"]
    volumes:
      - ${STORAGE_RACE_CONFIG:?required}:/run/monibuca/config.yaml:ro
      - ${STORAGE_RACE_DATA_DIR:?required}:/data
    ports:
      - "18080:8080"
      - "11935:1935"
  publisher:
    image: linuxserver/ffmpeg:7.1.1
    depends_on:
      - monibuca
    restart: on-failure
    command: ["-re", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=15", "-f", "lavfi", "-i", "sine=frequency=1000", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-c:a", "aac", "-f", "flv", "rtmp://monibuca/live/storage-race"]
```

Monibuca 故意不依赖 MinIO，确保能复现竞态；`minio-init` 创建 bucket，重连任务把 NoSuchBucket 视为 retryable。

- [ ] **Step 4: 写验证脚本的凭据与清理骨架**

`verify_storage_race.sh` 开头必须：

```bash
#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
COMPOSE_FILE="$ROOT_DIR/example/record-test/docker-compose.storage-race.yml"
WORK_DIR="$(mktemp -d)"
chmod 700 "$WORK_DIR"
export MINIO_ROOT_USER="race$(openssl rand -hex 8)"
export MINIO_ROOT_PASSWORD="$(openssl rand -hex 24)"
export STORAGE_RACE_CONFIG="$WORK_DIR/config.yaml"
export STORAGE_RACE_DATA_DIR="$WORK_DIR/monibuca-data"
export STORAGE_RACE_MINIO_DIR="$WORK_DIR/minio-data"
mkdir -p "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR"
cleanup() {
    docker compose -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$WORK_DIR"
    unset MINIO_ROOT_USER MINIO_ROOT_PASSWORD
}
trap cleanup EXIT
```

模板渲染实现放在下一步的 `render_config` 函数中；它只通过环境变量读取凭据，生成文件立即设为 600，任何凭据都不进入 argv。

- [ ] **Step 5: 写 readiness、HTTP 和 DB 断言函数**

脚本中实现以下完整 helper：

```bash
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
assert_eq() { local expected="$1" actual="$2" label="$3"; [ "$expected" = "$actual" ] || fail "$label: expected=$expected actual=$actual"; }
storage_status_code() { curl -sS -o "$WORK_DIR/storage-status.json" -w '%{http_code}' http://127.0.0.1:18080/api/storage/status; }
json_field() { python3 -c 'import json,sys; print(str(json.load(open(sys.argv[1]))[sys.argv[2]]).lower())' "$WORK_DIR/storage-status.json" "$1"; }
record_start_code() { local name="$1"; curl -sS -o "$WORK_DIR/start.json" -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d "{\"duration\":\"0\",\"fragment\":\"0\",\"filePath\":\"live/storage-race\",\"fileName\":\"$name.mp4\"}" http://127.0.0.1:18080/mp4/api/start/live/storage-race; }
record_stop_code() { curl -sS -o "$WORK_DIR/stop.json" -w '%{http_code}' -X POST http://127.0.0.1:18080/mp4/api/stop/live/storage-race; }
latest_record() { python3 - "$STORAGE_RACE_DATA_DIR/storage-race.db" <<'PY'
import sqlite3, sys
try:
    with sqlite3.connect(sys.argv[1]) as db:
        row = db.execute("select id, storage_type from record_streams order by id desc limit 1").fetchone()
        print("" if row is None else f"{row[0]} {row[1]}")
except sqlite3.OperationalError:
    print("")
PY
}
wait_for_code() { local expected="$1" attempts="$2"; for _ in $(seq 1 "$attempts"); do [ "$(storage_status_code 2>/dev/null || true)" = "$expected" ] && return 0; sleep 1; done; return 1; }
wait_for_stream() { for _ in $(seq 1 30); do [ "$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1:18080/api/stream/info/live/storage-race 2>/dev/null || true)" = "200" ] && return 0; sleep 1; done; return 1; }
wait_for_record_type() { local wanted="$1"; for _ in $(seq 1 30); do local row; row="$(latest_record)"; [ "${row##* }" = "$wanted" ] && { printf '%s\n' "$row"; return 0; }; sleep 1; done; return 1; }
download_record() { local id="$1" output="$2"; curl -fsSL "http://127.0.0.1:18080/mp4/download/live/storage-race?id=$id" -o "$output"; ffprobe -v error -show_entries format=duration -of default=noprint_wrappers=1:nokey=1 "$output" >/dev/null; }
render_config() { python3 - "$ROOT_DIR/example/record-test/config.storage-race.yaml.tmpl" "$STORAGE_RACE_CONFIG" <<'PY'
import os, pathlib, sys
source = pathlib.Path(sys.argv[1]).read_text()
rendered = source.replace("__ALLOW_LOCAL_FALLBACK__", os.environ["ALLOW_LOCAL_FALLBACK"]).replace("__MINIO_ROOT_USER__", os.environ["MINIO_ROOT_USER"]).replace("__MINIO_ROOT_PASSWORD__", os.environ["MINIO_ROOT_PASSWORD"])
pathlib.Path(sys.argv[2]).write_text(rendered)
PY
chmod 600 "$STORAGE_RACE_CONFIG"; }
reset_case_volumes() { docker compose -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true; rm -rf "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR"; mkdir -p "$STORAGE_RACE_DATA_DIR" "$STORAGE_RACE_MINIO_DIR"; }
```

脚本启动前执行 `command -v curl >/dev/null`、`command -v python3 >/dev/null`、`command -v openssl >/dev/null`、`command -v ffprobe >/dev/null` 和 `docker compose version >/dev/null`；任一缺失立即 `fail`。

- [ ] **Step 6: 写默认禁止 fallback 场景函数**

```bash
run_no_fallback_case() {
    local round="$1" row record_id record_type
    printf 'Round %s: fallback disabled\n' "$round"
    reset_case_volumes
    export ALLOW_LOCAL_FALLBACK=false MINIO_DELAY_SECONDS=15
    render_config
    docker compose -f "$COMPOSE_FILE" up -d --build
    wait_for_code 503 15 || fail "round $round: degraded status did not appear"
    assert_eq "s3" "$(json_field desiredType)" "desired storage"
    assert_eq "true" "$(json_field degraded)" "degraded flag"
    assert_eq "false" "$(json_field fallbackActive)" "fallback flag"
    wait_for_stream || fail "round $round: publisher did not become ready"
    assert_eq "503" "$(record_start_code "no-fallback-$round")" "record start while S3 unavailable"
    assert_eq "" "$(latest_record)" "no local record while fallback disabled"
    wait_for_code 200 90 || fail "round $round: S3 did not recover"
    assert_eq "s3" "$(json_field activeType)" "active storage after recovery"
    assert_eq "false" "$(json_field degraded)" "ready after recovery"
    assert_eq "200" "$(record_start_code "s3-$round")" "S3 record start"
    sleep 6
    assert_eq "200" "$(record_stop_code)" "S3 record stop"
    row="$(wait_for_record_type s3)" || fail "round $round: S3 record not persisted"
    read -r record_id record_type <<<"$row"
    assert_eq "s3" "$record_type" "persisted S3 storage type"
    download_record "$record_id" "$WORK_DIR/no-fallback-$round-s3.mp4"
}
```

每个失败只打印状态字段和 HTTP code，不打印 config、环境变量或预签名 Location。

- [ ] **Step 7: 写显式临时 fallback 场景函数**

```bash
run_explicit_fallback_case() {
    local round local_row local_id local_type s3_row s3_id s3_type
    printf 'Round %s: fallback enabled\n' "$round"
    reset_case_volumes
    export ALLOW_LOCAL_FALLBACK=true MINIO_DELAY_SECONDS=30
    render_config
    docker compose -f "$COMPOSE_FILE" up -d --build
    wait_for_code 503 15 || fail "round $round: fallback degraded status did not appear"
    assert_eq "local" "$(json_field activeType)" "temporary active storage"
    assert_eq "true" "$(json_field degraded)" "fallback remains degraded"
    assert_eq "true" "$(json_field fallbackActive)" "fallback active flag"
    wait_for_stream || fail "round $round: publisher did not become ready"
    assert_eq "200" "$(record_start_code "local-$round")" "fallback local record start"
    sleep 6
    assert_eq "200" "$(record_stop_code)" "fallback local record stop"
    local_row="$(wait_for_record_type local)" || fail "round $round: local fallback record not persisted"
    read -r local_id local_type <<<"$local_row"
    assert_eq "local" "$local_type" "persisted fallback storage type"
    download_record "$local_id" "$WORK_DIR/fallback-$round-local-before-recovery.mp4"
    wait_for_code 200 120 || fail "round $round: fallback case S3 did not recover"
    assert_eq "s3" "$(json_field activeType)" "active S3 after fallback recovery"
    assert_eq "false" "$(json_field fallbackActive)" "fallback cleared after recovery"
    assert_eq "200" "$(record_start_code "s3-after-fallback-$round")" "post-recovery S3 record start"
    sleep 6
    assert_eq "200" "$(record_stop_code)" "post-recovery S3 record stop"
    s3_row="$(wait_for_record_type s3)" || fail "round $round: post-recovery S3 record not persisted"
    read -r s3_id s3_type <<<"$s3_row"
    assert_eq "s3" "$s3_type" "post-recovery storage type"
    download_record "$s3_id" "$WORK_DIR/fallback-$round-s3.mp4"
    download_record "$local_id" "$WORK_DIR/fallback-$round-local-after-recovery.mp4"
}
```

最后一次 local 下载验证 active 已变 S3 时仍按记录自己的 `storage_type=local` 读取；Task 4 的 Go 测试反向覆盖 active local + 历史对象记录。

- [ ] **Step 8: 外层重复 5 轮并输出摘要**

脚本主流程：

```bash
command -v curl >/dev/null || fail "curl is required"
command -v python3 >/dev/null || fail "python3 is required"
command -v openssl >/dev/null || fail "openssl is required"
command -v ffprobe >/dev/null || fail "ffprobe is required"
docker compose version >/dev/null || fail "docker compose is required"
for round in 1 2 3 4 5; do
    run_no_fallback_case "$round"
    run_explicit_fallback_case "$round"
done
printf 'PASS: 5 rounds, no-fallback and explicit-fallback storage race scenarios\n'
```

- [ ] **Step 9: 更新 gitignore**

`example/record-test/.gitignore` 新增：

```gitignore
.storage-race/
```

脚本使用系统临时目录，规则用于防止开发者手动改成仓内目录后误提交 DB/凭据。

- [ ] **Step 10: 只做静态验证（本次禁止调用 Docker daemon）**

Run: `bash -n example/record-test/scripts/verify_storage_race.sh`

Expected: 退出码 0。

先在仓库外创建 mode-0600、仅含非敏感静态占位值且覆盖所有必需变量的 env file，再运行：

Run: `docker compose --env-file /tmp/monibuca-storage-task9-compose.env -f example/record-test/docker-compose.storage-race.yml config --quiet`

Expected: 退出码 0；仅做客户端插值与 Compose model 解析，不输出渲染模型、不写运行配置、不访问 daemon。不要宣称容器集成测试已运行。

- [ ] **Step 11: 提交 Task 8**

Run: `git add example/record-test/Dockerfile.storage-race example/record-test/Dockerfile.storage-race.dockerignore example/record-test/docker-compose.storage-race.yml example/record-test/config.storage-race.yaml.tmpl example/record-test/scripts/verify_storage_race.sh example/record-test/.gitignore && git commit -m "test(storage): cover delayed MinIO startup recovery"`

---

### Task 9: 全量验证、审查与交付边界

> **最终验收以顶部“Task 9 验证修订”为准：** 下方静态 gofmt 文件表、broad `plugin/mp4/...` S3/race 命令、空 working-tree diff 扫描和“root vet 应通过”的预期均已过时，不得据此宣称全仓或 broad suite 通过。

**Files:**
- Modify only if review finds defects: 本计划已修改文件
- Do not modify: `plugin/crypto/pkg/transform.go`、xde-installer、现场服务器

**Interfaces:**
- Consumes: Tasks 1-8 全部交付物。
- Produces: 可审查的测试证据和已知限制清单。

- [ ] **Step 1: 动态派生并格式化变更 Go 文件**

Run: `git diff --name-only --diff-filter=ACMRT d1b7abee8ca17dc5867f944a11c4163d718dc2b8...HEAD -- '*.go' | xargs gofmt -w`

Expected: 退出码 0，格式化后无代码 working-tree diff。

- [ ] **Step 2: 跑默认 focused contract**

Run: `go test ./pkg/storage -count=1`

Run: `go test ./ -run '^(TestDeleteRecord|TestRecordingStorage|TestValidateRecordingStorage|TestInitS3Storage|TestInitStorageWithoutS3|TestStorageReconnect|TestNewStorageReconnect|TestDedicatedStorageReconnect|TestGetStorageForType|TestStorageSnapshot|TestInitStorageFallsBack|TestStorageStatus|TestRetryUploadSkips)' -count=1`

Run: `go test ./plugin/mp4 ./plugin/mp4/pkg ./plugin/gb28181 -run '^(TestRedirectToStorageURL|TestDownloadSingleFile|TestDownloadRange|TestOpenRecordFile|TestServeStoredRecordFile)' -count=1`

Run: `go test ./plugin/flv/pkg ./plugin/snap -run '^$' -count=1`

Expected: 全部 PASS；最后一条是无运行测试的编译门禁。

- [ ] **Step 3: 跑 S3 focused contract 与 broad compile-only gate**

在 Step 2 的 focused 命令上加 `-tags s3`；随后运行：

Run: `go test -tags s3 ./ ./pkg/storage/... ./plugin/mp4/... ./plugin/gb28181/... ./plugin/flv/pkg ./plugin/snap -run '^$' -count=1`

Expected: PASS，不访问真实对象存储。不得运行/宣称 broad S3 MP4 测试通过；其遗留测试依赖仓库外绝对媒体夹具。

- [ ] **Step 4: 跑 direct race contract**

Run: `go test -race ./pkg/storage -run '^(TestRegistry|TestUnavailable|TestIsPermanentConnection|TestNoSuchBucket)' -count=1`

Run: `go test -race ./ -run '^(TestDeleteRecord|TestRecordingStorage|TestValidateRecordingStorage|TestInitS3Storage|TestInitStorageWithoutS3|TestStorageReconnectBackoff|TestNewStorageReconnectTask|TestStorageReconnectTick|TestGetStorageForType|TestStorageSnapshot|TestInitStorageFallsBack|TestStorageStatus|TestRetryUploadSkips)' -count=1`

Run: `go test -race ./plugin/mp4 ./plugin/mp4/pkg ./plugin/gb28181 -run '^(TestRedirectToStorageURL|TestDownloadSingleFile|TestDownloadRange|TestOpenRecordFile|TestServeStoredRecordFile)' -count=1`

以上再以 `-tags s3` 运行 S3 相关直接契约。Expected: 本次新增代码无 data race；不要把 `TestDedicatedStorageReconnectWorkLifecycleNonRace` 或 broad gotask lifecycle suites 放进 race 门禁，以免触发已知上游 EventLoop race。

- [ ] **Step 5: 编译受影响路径**

Run: `go build ./ ./pkg/storage/... ./plugin/mp4/... ./plugin/gb28181/... ./plugin/flv/pkg ./plugin/snap ./example/default`

Run: `go build -tags s3 ./ ./pkg/storage/... ./plugin/mp4/... ./plugin/gb28181/... ./plugin/flv/pkg ./plugin/snap ./example/default`

Expected: 两条都 PASS。不得用 `go build ./...` 作 acceptance gate；`plugin/crypto/pkg/transform.go` 有预存在编译错误。

- [ ] **Step 6: 运行 go vet 的可验证范围**

Run: `go vet ./pkg/storage/... ./plugin/mp4/... ./plugin/flv/pkg ./plugin/snap`

Run: `go vet -tags s3 ./pkg/storage/... ./plugin/mp4/... ./plugin/flv/pkg ./plugin/snap`

Run: `go vet -copylocks=false ./`

Expected: 以上通过。另行运行并如实记录 `go vet ./` 的未修改 `plugin.go:243` copylocks，以及 `go vet ./plugin/gb28181/...` 的未修改 `broadcast.go` IPv6 地址格式诊断；本任务不修 baseline。

- [ ] **Step 7: 安全与架构静态检查**

Run: `git diff --check d1b7abee8ca17dc5867f944a11c4163d718dc2b8...HEAD`

对该 committed range 的新增行及两个 docs 扫描 YAML 非占位凭据、AWS key id、private-key block、预签名参数和 RTSP userinfo；只输出模式名、文件名、计数。测试中的恶意 sentinel 与文档中的模式名必须单独分类，production/docs value leak 计数应为 0。

同时检查：无裸 `Server.Storage` 访问、无旧 mismatch 逻辑；MP4/DeleteRecord 均传 persisted `StorageType` 给 `GetStorageForType`；所有 `OnStart` 先于对应 `AddTask`；storage/S3 error 进入日志/HTTP 前统一脱敏。

Expected: whitespace/direct-access/invariant gate PASS；不得读取或输出仓库中预存在凭据值。

- [ ] **Step 8: 按绑定清单完成最终人工审查并处理结论**

审查重点必须明确列出：

```text
1. atomic snapshot 是否覆盖全部 Server.Storage 运行期访问；
2. Registry 并发构造/Close 是否竞态或重复 Close；
3. OnStart 是否全部在 AddTask 之前注册；
4. fallback=true 是否仍 degraded 并持续重连；
5. fallback=false 是否可能写入 local 记录；
6. 历史 S3/local 记录是否都按自身 StorageType；
7. HTTP/日志是否泄露凭据或预签名 URL；
8. 永久错误停止重试后 readiness 是否仍为 503。
```

修复 CRITICAL/HIGH；能安全修复的 MEDIUM 一并处理，然后重跑 Steps 1-7。

- [ ] **Step 9: 最终状态与未运行项如实报告**

最终交付必须包含：

```text
- 实际通过的 Go 测试命令和结果；
- 实际通过的 build/vet 命令和结果；
- Docker 脚本只完成 bash/shellcheck/Compose/Dockerignore 静态检查，未调用 Docker daemon 或实际启动；
- 未操作 172.16.12.74 / 172.16.12.183 或其它现场服务器；
- fallback 期间已完成的 local 录像不会自动迁移到 S3；
- OSS/COS、运行期二次掉线、FLV/HLS/Snap 的 storage_type 缺口不在本次范围。
```

- [ ] **Step 10: 提交审查修复（仅有实际改动时）**

Run（仅当有实际审查修复）: `git add api.go api_delete_record_storage_test.go storage_runtime.go storage_status_api_test.go plugin/mp4/api.go plugin/mp4/api_storage_test.go plugin/mp4/pkg/demux-range.go && git commit -m "fix(storage): sanitize backend errors in record paths"`

如果审查未产生文件改动，不创建空提交。

## Task 9 最终执行记录

- Review base 为 `d1b7abee8ca17dc5867f944a11c4163d718dc2b8`；变更文件动态派生。最终审查发现历史 backend resolver/删除错误会把可含 endpoint userinfo 或签名的底层错误带入 MP4 日志与 `DeleteRecord` API error，先以 RED 测试复现，再通过统一固定摘要和保留 cause、但 `Error()` 安全的包装修复，独立提交为 `0e7daa0e`。
- 默认与 S3 focused contract、直接 registry/runtime/reconnect/MP4 race、默认与 S3 受影响路径 build 均通过。覆盖率按遗留 package 诚实报告，而不是宣称全仓 80%；新增关键状态机/重连函数有直接契约证据。
- 可通过的默认/S3 focused vet 通过；根包 `go vet ./` 仍只报未修改的 `plugin.go:243` copylocks，GB28181 focused vet 仍报未修改 `broadcast.go` 的 IPv6 地址格式。未运行全仓 `./...` acceptance、broad gotask lifecycle race 或依赖绝对媒体夹具的 broad S3 MP4 suite。
- 容器只完成 `bash -n`、shellcheck、Compose client `config --quiet`（mode-0600 非敏感 env file）以及脚本/Dockerignore 静态策略；没有调用 Docker daemon、拉取/检查镜像、创建网络或运行五轮场景。
- 未连接、部署、重启或修改 172.16.12.74 / 172.16.12.183，也未操作其它现场服务器。fallback-local 记录不会自动迁移；OSS/COS、运行期二次掉线、FLV/HLS/Snap `storage_type` 缺口仍不在范围。
- 仓库预存在且本计划未修改的示例对象存储凭据未被读取或输出；需另立事项轮换/移除。

---

## Self-Review 结果

### Spec coverage

- 启动竞态不再永久 local：Task 5。
- 默认禁止 fallback：Task 5 + Task 6。
- 显式 fallback 仍 degraded、持续重连、恢复切 S3：Task 5。
- 历史记录按 `storage_type`：Task 3 + Task 4。
- readiness 200/503：Task 7。
- 纯 local 兼容：Task 5 单测。
- atomic 并发安全：Task 2 + Task 9 race。
- 容器竞态重复测试：Task 8（按用户选择只提交、不实跑）。
- 凭据脱敏：Task 7、Task 8、Task 9。

### Type consistency

- Registry API 在 Task 1 定义，后续只使用 `GetOrCreate` / `HasConfig` / `Close`。
- Atomic 状态统一使用 `StorageStatus` 和单个 `storageSnapshot`；无 `atomic.Pointer[interface]`。
- 后端读取统一使用 `GetStorage()`；历史记录统一使用 `GetStorageForType()`。
- fallback 状态统一用 `Degraded=true + FallbackActive=true`；恢复统一两者 false。
- readiness 直接编码 `StorageStatus`，不新增重复 DTO。

### Scope boundary

两个现象虽可独立复现，但共同依赖 Storage registry 与运行期状态，因此保留在一个存储子系统计划中；每个 Task 仍可独立测试和审查。
