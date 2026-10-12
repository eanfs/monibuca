# S3 启动恢复与历史录像存储解析设计

## 背景

现场在 x86（172.16.12.74）和 ARM64（172.16.12.183）上都复现了相同行为：服务器重启时 Monibuca 与 MinIO 几乎同时启动，Monibuca 的一次性 `HeadBucket` 检查先于 MinIO readiness 完成而失败。`server.go:initStorage` 随即静默切到 local，进程生命周期内不再尝试 S3。结果是：

- 重启后的新录像全部写成 `storage_type=local`；
- 重启前已经写入 `storage_type=s3` 的录像，因为下载路径要求“当前全局存储类型”和记录类型相等而返回 HTTP 500；
- `/api/sysinfo` 仍返回成功，无法区分“服务可响应”和“配置的对象存储不可用”。

这不是架构或负载问题，而是启动竞态、错误回退策略和历史记录后端解析三个问题叠加。

## 目标

1. 显式配置 S3 时，短暂启动竞态不能造成永久 local 降级。
2. 默认禁止 S3 失败后回退 local；存储未就绪时拒绝新录像并返回 HTTP 503。
3. 保留**显式、可观测、临时**的 local fallback：启用后可以在 S3 不可用期间录制到 local，但状态仍为 degraded，后台继续重连，S3 恢复后新录像自动切回 S3。
4. 历史记录必须依据其自身 `storage_type` 解析后端，不依赖当前主存储类型。
5. 提供存储 readiness 接口：ready 返回 HTTP 200，degraded 返回 HTTP 503，并携带期望类型、当前类型、fallback 状态、最近检查时间和脱敏错误。
6. 保持纯 local 部署行为不变。
7. 所有运行期主存储读写无数据竞争。

## 非目标

- 不修改 xde-installer、现场服务器或容器健康检查模板。
- 不承诺 OSS/COS 的供应商级恢复语义；通用 registry 可能自然适用，但本次只测试和验收 S3/MinIO。
- 不迁移 fallback 期间已完成的 local 录像到 S3；它们保留 `storage_type=local` 并继续按记录类型下载。
- 不处理运行期已经 ready 的 S3 后续掉线检测；本次只解决启动时不可用、稍后恢复。
- 不修复 FLV/HLS/Snap 中未携带 `storage_type` 的其它历史读取缺口。
- 未经用户另行授权，不部署或重启 172.16.12.74 / 172.16.12.183。

## 状态语义

| 配置与运行状态 | active storage | degraded | fallbackActive | 新录像 |
|---|---|---:|---:|---|
| 未配置对象存储 | local | false | false | 正常写 local |
| S3 首次成功 | s3 | false | false | 正常写 S3 |
| S3 失败，默认 `storageallowlocalfallback=false` | unavailable 占位 | true | false | HTTP 503，不建记录 |
| S3 失败，显式 `storageallowlocalfallback=true` | local | true | true | 写 local，记录为 `storage_type=local` |
| 后台重连 S3 成功 | s3 | false | false | 后续新录像写 S3 |

切换只影响后续新建的 `RecordJob`。已经开始的录像缓存了启动时的 Storage 实例，因此会在原后端完成，避免单个文件跨后端写入。

## 架构

### Storage Registry

`pkg/storage.Registry` 按类型缓存成功构造的 Storage。它保存配置的浅拷贝，按 key 使用独立锁做 single-flight 构造：失败不缓存，成功后复用。历史读取和主存储初始化/恢复共用同一个 registry。

### 原子主存储与不可变状态快照

`Server` 不再暴露可运行期裸写的 `Storage storage.Storage` 字段，而是通过单个 `atomic.Pointer[storageSnapshot]` 同时保存：

- 当前活动 Storage 引用；
- 当前存储状态快照。

每次状态变化创建新快照并原子替换；读取方通过 `GetStorage()` 和 `GetStorageStatus()` 获取一致值。

### 后台恢复任务

`Server` 持有专用的 `StorageReconnectWork`（嵌入 `github.com/eanfs/gotask.Work`），与 `Records` 队列隔离。该 Work 的 `OnStart` 在加入 Server 之前注册，并只承载一个嵌入 `task.TickTask` 的 `StorageReconnectTask`，因此连接退避不会阻塞录像和上传补偿任务。

`StorageReconnectTask` 每秒轮询一次自管的 `nextAttempt` 截止时间；到期才从 registry 获取期望 S3。它不调用 `sleep`、不启动裸 goroutine，也不把连接错误返回给 gotask。退避序列固定为 5 秒、10 秒、20 秒、40 秒、80 秒、160 秒，之后封顶 5 分钟：

- 瞬态错误保留当前 active backend，继续保持 degraded/fallback 状态，更新脱敏诊断并安排下一个截止时间；
- 永久错误（鉴权、无效配置、未编译 s3 build tag）不再重试，但 readiness 继续为 503/degraded；`NoSuchBucket` 特意保持瞬态，以允许延迟创建 bucket；
- 恢复成功后原子切换 active storage 与状态，清除 degraded/fallback，发出恢复告警；
- 恢复成功或永久错误都通过通用 `task.ErrTaskComplete` 停止任务，不把原始 S3 错误作为任务停止原因传播。

所有 `OnStart` 回调都在对应 `AddTask` 前注册：`StorageReconnectWork.OnStart` 先于 `s.AddTask(s.storageReconnectWork)`，`Records.OnStart` 中的上传补偿调度器先于 `s.AddTask(&s.Records)`，避免 gotask 不重放 `OnStart` 的陷阱。

### 按记录类型解析

`Server.GetStorageForType(storageType)` 负责：

- 空字符串和 `local` 使用 local registry 实例；
- 已配置的 `s3` 使用/构造 S3 registry 实例；
- 未配置类型返回 `storage.ErrStorageTypeNotConfigured`；
- 当前 active storage 是否为同类型不参与判断。

MP4 单文件下载、范围合并、DemuxRange 和 DeleteRecord 都改用这一入口。

### Readiness

新增直接 HTTP handler `GET /api/storage/status`：

```json
{
  "desiredType": "s3",
  "activeType": "local",
  "degraded": true,
  "fallbackActive": true,
  "lastCheckTime": "2026-08-27T09:00:00Z",
  "lastError": "configured storage is temporarily unavailable"
}
```

ready 时返回 200；degraded 时返回 503。状态响应使用固定白名单摘要；MP4 下载与 `DeleteRecord` 的 backend resolver/操作失败也统一通过 `StorageErrorSummary` 和保留 cause 的安全错误包装对外脱敏。响应和日志不得包含 access key、secret key、endpoint userinfo、RTSP 凭据或预签名 URL。

## 验收标准

1. 单元测试稳定复现“构造前两次失败、随后成功”，失败不缓存，成功只构造一次。
2. 未配置对象存储时行为与当前 local 部署一致。
3. 默认禁止 fallback 时，S3 未就绪期间开始录像返回 503，数据库不新增 local 记录。
4. 显式允许 fallback 时，S3 未就绪期间新记录为 local，同时 readiness 为 503/degraded；S3 恢复后 readiness 变 200，后续新记录为 s3。
5. 当前 active storage 为 local 时，已有 `storage_type=s3` 记录仍按 S3 后端下载，不返回 storage type mismatch。
6. 旧 S3 记录、新 S3 记录、fallback local 记录都能按自身类型下载。
7. `go test -race` 覆盖的存储状态并发读写无新增数据竞争。
8. 提供 MinIO 延迟启动的五轮容器测试脚本；本次仅完成 bash、shellcheck、Compose client config 与 Dockerignore 策略静态验证，未执行 Docker build/up、镜像拉取或五轮场景。
9. 测试、HTTP 错误和日志不泄露任何敏感凭据、endpoint userinfo 或预签名参数。

## 最终验证边界

- 最终评审基于 `d1b7abee8ca17dc5867f944a11c4163d718dc2b8...HEAD` 派生实际变更文件；默认、S3、直接 race、受影响 build 与可验证 vet 门禁均使用聚焦包/测试模式，不把全仓遗留问题伪装成本计划失败或成功。
- 容器五轮 harness **未执行**；外部镜像标签、真实 BuildKit 上下文、MinIO/mc/ffmpeg 行为仍需在允许 Docker daemon 的环境中验证。
- 未连接、部署、重启或修改 172.16.12.74 / 172.16.12.183，也未操作其它现场服务器。
- fallback 期间完成的 local 录像不会自动迁移到 S3；OSS/COS 供应商恢复、运行期二次掉线检测，以及 FLV/HLS/Snap 的 `storage_type` 历史缺口仍不在本次范围。
- 仓库中预存在、未由本计划修改的示例对象存储凭据仍需另立安全事项完成轮换/移除；本次验证没有读取、复制或输出其值。
