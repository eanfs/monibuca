# 录像上传可靠性 P1/P2 修复 实施计划

> **For agentic workers:** 用 superpowers:executing-plans 逐 Phase 执行。Step 用 checkbox(`- [ ]`)跟踪。

**Goal:** 修复录像上传链路的 4 个 P1/P2 级可靠性缺陷,目标「不丢录像、可观测、可人工介入」。来源:2026-05-22 上传可靠性 code review。P0(`pending_uploads` / `m7s.db` 不持久化)已通过 130 docker-compose 加 bind mount 解决,不在本 plan 范围。

## 背景

录像上传链路:录制 → 写本地暂存 → record stop 触发上传。两层重试:
1. `pkg/storage/retry.go` `UploadWithRetry` —— 即时重试(默认 4 次尝试,指数退避 5s→40s)
2. `upload_retry.go` `UploadRetryScheduler`(`task.TickTask`,每 5min)—— 定时补传,失败任务持久化在 SQLite `upload_tasks` 表(`UploadTask` 模型),`MaxRetries=10`

## 待修 4 个问题

- **P1-a — Uploading 状态崩溃卡死**:`retryUpload` 先 `MarkUploading`(status→Uploading=1)再上传;若其间进程崩溃,任务永停 Uploading,而 `QueryPendingUploads` 只查 Failed(3)→ 永不再补传。
- **P1-b — 上传成功但 DB 入库失败 → 孤儿文件**:`recoder.go` `WriteTailDeferred` 返回的闭包写 `record_streams` 失败时只 `Warn` 不返回 error → MinIO 有文件、DB 无索引。
- **P2-a — 补传次数耗尽彻底放弃**:`retry_count` 达 `MaxRetries(10)` 后 `QueryPendingUploads` 不再匹配,无告警、无人工介入接口。
- **P2-b — pending 堆积撑爆本地盘**:MinIO 长期故障时 pending 文件持续堆积,无磁盘水位保护,盘满导致新录制写入失败。

## 实现前调研结论(已确认)

1. **AutoMigrate**:`server.go:334` 已 `AutoMigrate(&db.User{}, &PullProxyConfig{}, &PushProxyConfig{}, &StreamAliasDB{}, &AlarmInfo{}, &UploadTask{})` —— `UploadTask`/`AlarmInfo` 已纳管。给 `UploadTask` 加列自动生效;新表(`record_stream_recovery`)需加进该列表。
2. **UploadConfig 注入**:`server.go:302` `InitUploadManager(storage.UploadConfig{MaxConcurrentUploads:4, ...})` 为**硬编码**,`ServerConfig`(`server.go:60`)无 `Upload` 字段 —— Phase 2 需给 `ServerConfig` 加 `Upload storage.UploadConfig` 字段并改 `InitUploadManager` 读它。
3. **admin API**:`plugin.go:73` `IRegisterHandler{ RegisterHandler() map[string]http.HandlerFunc }`,plugin 实现即注册 HTTP 路由。mp4 plugin 当前走 grpc-gateway、未实现 `IRegisterHandler` —— Phase 3 的 upload admin API 挂载点见 Task 3.3。
4. **告警**:`AlarmInfo`(`alarm.go`,表 `alarm_info`)已 AutoMigrate;`pkg/config/types.go` 有 `AlarmStorageException`/`AlarmDiskSpaceFull` 常量。告警最小实现 = 直接 `db.Create(&AlarmInfo{})`;webhook 分散绑定,推送为可选。

## 文件结构

```
[改] upload_task.go            UploadTask 加 UploadStartedAt;MarkUploading 原子抢占;
                               新增 ReclaimStaleUploading / QueryExhaustedUploads / ResetUploadForRetry
[改] upload_retry.go           UploadRetryScheduler.Start();Tick 内回收;retryUpload 适配 CAS
[改] recoder.go                WriteTailDeferred 闭包返回 error
[改] plugin/mp4/pkg/record.go  writeTrailerTask.dbWrite 类型改 func(task.IJob) error;捕获错误补偿
[改] server.go                 ServerConfig 加 Upload 字段;InitUploadManager 读它;新表入 AutoMigrate
[改] pkg/storage/upload_manager.go  UploadConfig 加水位字段;MoveToPendingDir 水位检查;
                                    GetPendingDirUsage;ErrPendingDirFull
[新] pkg/storage/statfs_unix.go / statfs_windows.go   分平台 GetDiskFreeBytes
[新] upload_alarm.go           raiseUploadAlarm 告警 helper(去重 + 写 alarm_info)
[新] record_recovery.go        孤儿 RecordStream 持久化补偿(record_stream_recovery 表 + 重试)
[新] upload_admin.go           admin API:列耗尽任务 / 重置重传
[新] upload_task_test.go / pkg/storage/upload_manager_test.go   单测
docs/superpowers/plans/2026-05-22-upload-reliability-p1p2.md   本文件
```

---

# Phase 1 — P1-a(Uploading 卡死)+ P1-b(孤儿文件)

数据一致性硬伤,优先。两者独立,合在一个 Phase 验证。

## Task 1.1: UploadTask 加 UploadStartedAt 字段

- [ ] `upload_task.go` `UploadTask` struct 加 `UploadStartedAt time.Time`(gorm tag 含 `index`,`desc:"本次进入 Uploading 状态的时间"`)。
- 用途:P1-a 判断 Uploading 任务卡了多久。`UpdatedAt` 不可靠(任何 Update 都刷新)。
- 风险:Low。`UploadTask` 已在 `server.go:334` AutoMigrate,加 nullable 列对既有行安全。

## Task 1.2: MarkUploading 改原子抢占

- [ ] `MarkUploading` 改条件更新:`WHERE id=? AND status=?(Failed)`,`Updates` 同时写 `status=Uploading` + `upload_started_at=now`。签名改 `MarkUploading(db, taskID) (claimed bool)`,返回 `RowsAffected>0`。
- 用途:`Tick` 对每条记录 `go retryUpload`,5min tick 可能与上轮重叠 → 同文件并发上传。条件更新让只有第一个抢到的继续。
- 依赖:Task 1.1。风险:Medium —— 改签名,需同步 Task 1.4 调用点。

## Task 1.3: 新增 ReclaimStaleUploading

- [ ] `upload_task.go` 新增 `ReclaimStaleUploading(db *gorm.DB, staleThreshold time.Duration) (int64, error)`:`WHERE status=Uploading AND upload_started_at <= now-staleThreshold`,`Updates` status→Failed、`next_retry_at=now`,**不增 retry_count**(崩溃不算失败尝试)。
- `staleThreshold` 取值须 > 单文件最长上传耗时(`retryUpload` 里 `WithTimeout(30min)`)→ 建议 35min,做成包级常量。
- 依赖:Task 1.1。风险:Medium —— 阈值过小会误回收正在传的任务;但 Task 1.2 的条件 `MarkUploading` 兜底,且对象存储覆盖写幂等,最坏只是浪费一次带宽。

## Task 1.4: UploadRetryScheduler.Start() + Tick 回收 + retryUpload 适配

- [ ] `upload_retry.go` 新增 `func (u *UploadRetryScheduler) Start() error`:启动时调一次 `ReclaimStaleUploading`(回收上次崩溃残留),记日志。`TickTask` 允许 override `Start()`。
- [ ] `Tick` 在 `QueryPendingUploads` 之前也调一次 `ReclaimStaleUploading`(覆盖运行期 goroutine panic/超时卡死)。
- [ ] `retryUpload` 改:`if !MarkUploading(...) { return }`(没抢到直接退);调整顺序为**先抢 DB 状态再 `AcquireUploadSlot`**,避免占着 slot 才发现没抢到。
- 依赖:Task 1.2、1.3。风险:Medium。`retryUpload` 当前是裸 goroutine(`go u.retryUpload`),违反项目「不用裸 goroutine」约定 —— 标为已知债,本 Phase 不扩大改动;若 review 要求再改 `u.AddTask` 调度(`TickTask` 可 `AddTask`)。

## Task 1.5: WriteTailDeferred 闭包返回 error

- [ ] `recoder.go` `WriteTailDeferred` 返回类型从 `func(task.IJob)` 改为 `func(task.IJob) error`。两个闭包分支内 `db.Save(&recordStream...)` 失败时收集并返回 error(`record_streams` 是孤儿判定依据,必须计入;`RecordEvent` 的 Save 失败可只 warn)。
- 依赖:无(与 P1-a 独立)。风险:Medium —— 调用方唯一(`plugin/mp4/pkg/record.go` `writeTailer`,已 grep 确认),`writeTrailerTask.dbWrite` 字段类型同步改。

## Task 1.6: writeTrailerTask 捕获 dbWrite error → 孤儿补偿

- [ ] `plugin/mp4/pkg/record.go` `writeTrailerTask.dbWrite` 字段类型改 `func(task.IJob) error`。
- [ ] `Run()` 阶段 3 成功分支 + `runInsertRangeFastPath` 成功分支的 `t.dbWrite(...)` 调用,改为捕获 error:
  - **Phase 1 实现**:内联有限重试(如 3 次 × 1s,不阻塞单线程 trailer queue 太久);仍失败 → `t.Error` 日志 + 写一条 `AlarmStorageException` 告警(注明孤儿 objectKey)。
  - 完整的「持久化补偿队列」挪到 Phase 3 Task 3.4。
- 依赖:Task 1.5、Phase 2 的 `raiseUploadAlarm`(若 Phase 2 先做)或临时内联告警。风险:Medium。

## Task 1.7: Phase 1 验收

- [ ] 新建 `upload_task_test.go`:`ReclaimStaleUploading`(超时回收 + 不回收新任务 + retry_count 不变)、`MarkUploading` 并发抢占(2 goroutine 只 1 个 claimed)、`WriteTailDeferred` 闭包 DB 失败返回 error。
- [ ] `go test ./plugin/mp4/pkg ./test -count=1` 无回归;`go build ./plugin/mp4/... ./pkg/storage/...`(全仓 build 受 crypto 预存在错影响,用子路径)。
- [ ] 集成:`example/record-test` 录流 → 上传中途 `kill -9` → 重启 → 日志见 `reclaimed stale uploading` 且文件最终补传。

---

# Phase 2 — P2-b(pending 盘满保护)

独立,可与 Phase 1 并行。「磁盘写满」拖垮新录制,影响面大于「单文件放弃」,优先于 P2-a。

## Task 2.1: UploadConfig 加水位配置 + 接入 ServerConfig

- [ ] `pkg/storage/upload_manager.go` `UploadConfig` 加字段(沿用现有 `desc`/`default` tag 范式):`PendingMaxSizeMB int`(`default:"0"`=不限)、`PendingMaxFiles int`(`default:"0"`)、`PendingDiskMinFreeMB int`(`default:"0"`)。`InitUploadManager` 把值存入包级变量。
- [ ] `server.go` `ServerConfig` 加 `Upload storage.UploadConfig` 字段;`server.go:302` `InitUploadManager` 改为读 `s.ServerConfig.Upload`(默认全 0,行为与现网一致)。
- 风险:Low-Medium。默认 0 = 向后兼容。

## Task 2.2: pending 目录用量统计 + 分平台磁盘剩余

- [ ] `upload_manager.go` 新增 `GetPendingDirUsage() (totalBytes int64, fileCount int, err error)`(`filepath.WalkDir` 累加)。
- [ ] 新建 `pkg/storage/statfs_unix.go`(`//go:build !windows`)+ `statfs_windows.go`(`//go:build windows`),实现 `GetDiskFreeBytes(path string) (uint64, error)`;Unix 用 `syscall.Statfs`,Windows 返回「不支持」哨兵(降级为只按文件数/总大小限制)。
- 风险:Medium —— 跨平台,分文件解决。

## Task 2.3: MoveToPendingDir 水位检查

- [ ] `upload_manager.go` 新增导出 `ErrPendingDirFull`。`MoveToPendingDir` 开头(`pendingDir==""` 判断后)加水位检查:配置了阈值且已超 → 返回 `ErrPendingDirFull`。
- [ ] `plugin/mp4/pkg/record.go` `recoverToPending`/`recoverFastPathFailure` 识别 `ErrPendingDirFull` → `Error` 日志(明确「本录像无法暂存、可能丢失」)+ 触发 `AlarmDiskSpaceFull` 告警。
- 风险:Medium —— `MoveToPendingDir` 语义变化(原几乎总成功);调用方已 grep 确认仅 record.go 两处。

## Task 2.4: 告警 helper + pending 周期巡检

- [ ] 新建 `upload_alarm.go`:`raiseUploadAlarm(db, alarmType int, streamPath, filePath, desc string)` —— `db.Create(&AlarmInfo{...})`;写入前查最近 N 分钟同类型+同文件的未恢复告警,有则跳过(去抖)。
- [ ] `UploadRetryScheduler.Tick` 增加:调 `GetPendingDirUsage`,超「警戒线」(阈值 80%)→ `raiseUploadAlarm(AlarmDiskSpaceFull, ...)` + `Warn` 日志。复用现有 5min tick,不新增 task。
- 风险:Low。

## Task 2.5: Phase 2 验收

- [ ] `pkg/storage/upload_manager_test.go`:`GetPendingDirUsage`(`t.TempDir()` 造文件断言 count/size)、`MoveToPendingDir`+水位(`PendingMaxFiles=2`,第 3 次返回 `ErrPendingDirFull`)、`GetDiskFreeBytes`(Unix >0 / Windows 不 panic)。
- [ ] 集成:小 tmpfs 当 pending,关 MinIO 让录像堆积 → 达阈值后 `MoveToPendingDir` 返回 `ErrPendingDirFull`、`alarm_info` 出现告警、盘未满、新录制不受影响。
- [ ] `go test ./pkg/storage -count=1`。

---

# Phase 3 — P2-a(补传耗尽告警 + 人工介入)

依赖 Phase 2 的告警设施;并落地 Phase 1 Task 1.6 的完整孤儿补偿。

## Task 3.1: 补传耗尽触发告警

- [ ] `MarkUploadRetryFailed` 返回 `exhausted bool`(`retryCount+1 >= MaxRetries`);`retryUpload` 据此调 `raiseUploadAlarm(AlarmStorageException, ..., "补传次数耗尽,待人工处理")`。
- 依赖:Phase 2 `raiseUploadAlarm`。风险:Low。

## Task 3.2: QueryExhaustedUploads

- [ ] `upload_task.go` 新增 `QueryExhaustedUploads(db, limit)`:`WHERE status=Failed AND retry_count >= max_retries`。`QueryPendingUploads` 保持现状(不改 status 枚举,最小改动)。
- 风险:Low。

## Task 3.3: 人工介入 API

- [ ] `upload_task.go` 新增 `ResetUploadForRetry(db, taskID) error`:`retry_count=0`、`status=Failed`、`next_retry_at=now`。
- [ ] 新建 `upload_admin.go` 暴露 HTTP 接口:`GET /api/upload/exhausted`(列耗尽)、`POST /api/upload/{id}/retry`(重传)、可选 `POST /api/upload/{id}/discard`(确认放弃 + 删文件)。
- **挂载点决策(实现时定)**:mp4 plugin 未实现 `IRegisterHandler`。选项 A — 给 mp4 plugin 加 `RegisterHandler()`;选项 B — 走 server 已有 admin API 体系。需先读 server admin API 路由 + JWT 鉴权代码再定。
- 依赖:Task 3.2。风险:Medium —— 挂载点 + 鉴权需调研。

## Task 3.4: 孤儿 RecordStream 持久化补偿

- [ ] 新建表 `record_stream_recovery`(字段:`RecordStream` JSON 快照、retry_count、next_retry_at、created_at),加进 `server.go:334` AutoMigrate。
- [ ] Task 1.6 内联重试仍失败的孤儿 → 序列化 JSON 存该表。
- [ ] `UploadRetryScheduler.Tick` 增加:扫 `record_stream_recovery`,对每条重试 `db.Save(&RecordStream)`,成功删记录、失败退避。
- 依赖:Task 1.6、Phase 2。风险:Medium —— 新表入 AutoMigrate;`RecordStream` JSON round-trip 须保留 `ID`(`Save` upsert 不重复行)。

## Task 3.5: Phase 3 验收

- [ ] 单测:`QueryExhaustedUploads` / `ResetUploadForRetry` / `RecordStream` JSON round-trip。
- [ ] 集成:任务 `retry_count` 改到上限 → tick → `alarm_info` 出现耗尽告警;`POST /api/upload/{id}/retry` → 任务重新拉起。

---

## 风险与缓解

| 风险 | 等级 | 缓解 |
|---|---|---|
| DB 加列 / 新表 AutoMigrate | Medium | `UploadTask`/`AlarmInfo` 已纳管;新增列 nullable;新表加进 `server.go:334` |
| `retryUpload` 裸 goroutine 违反项目约定 | Medium | Phase 1 标已知债;必要时改 `u.AddTask` 调度 |
| `WriteTailDeferred` 改签名波及 mp4 plugin | Medium | 调用方唯一,改完 `go build ./plugin/mp4/...` 验证 |
| `syscall.Statfs` Windows 不支持 | Medium | 分平台文件 `statfs_unix.go`/`statfs_windows.go` |
| admin API 挂载点 / JWT 鉴权 | Medium | Task 3.3 实现前调研 server admin API |
| `MoveToPendingDir` 语义变化 | Medium | 调用方仅 record.go 两处,失败分支必告警不 panic |
| 全仓 `go build` 受 crypto 预存在错影响 | Low | 用子路径 build(`./plugin/mp4/... ./pkg/storage/...`) |

## 依赖与顺序

```
Phase 1 (P1-a + P1-b) ── 独立,优先
Phase 2 (P2-b)        ── 独立,可与 Phase 1 并行;产出 raiseUploadAlarm
        │
        ▼
Phase 3 (P2-a)        ── 依赖 Phase 2 告警设施 + 承接 Phase 1 Task 1.6
```

实施顺序:Phase 1 → Phase 2 → Phase 3。

## Self-Review

- **Spec 覆盖**:P1-a → Task 1.1-1.4;P1-b → Task 1.5-1.6 + 3.4;P2-b → Task 2.1-2.4;P2-a → Task 3.1-3.3。
- **向后兼容**:Phase 2 水位配置默认全 0(不启用);`MarkUploading` 改签名仅内部调用方;不改 `UploadStatus` 枚举。
- **不改动**:`UploadWithRetry` 即时重试逻辑;`pkg/storage/retry.go` 退避策略;trailer 队列单线程模型。
- **调研已闭环**:4 项实现前调研均已确认(见上文「实现前调研结论」),仅 Task 3.3 admin API 挂载点留待实现时按代码定。
