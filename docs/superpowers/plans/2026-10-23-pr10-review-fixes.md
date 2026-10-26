# PR #10 审查修复实施计划

日期：2026-10-23
基线：`feat/recording-integrity` @ `69e19d27`（内容等同 PR #10 head `91d0a04f`）
来源：`/pr-review-toolkit:review-pr https://github.com/eanfs/monibuca/pull/10` 的审查结论

## 背景

PR #10 交付录制崩溃恢复与上传完整性。审查发现 4 个 Critical 与 27 个 Important。
本计划只修 **Critical 与 Important**，Suggestions 不在范围内（另开）。

问题按信任边界、限额状态机、失败可见性、崩溃一致性、恢复正确性、默认值/类型契约六类归并。

## Global Constraints

以下约束对所有任务生效，违反即验收不通过：

- **TDD**：先写失败测试，再实现；每个任务的测试必须能在修复前失败、修复后通过。
- **task 系统**（`pkg/task/`，fork `github.com/eanfs/gotask`）：
  - `Run()` 内联跑在父 Job 事件循环上，禁止阻塞；阻塞工作放 `Go()`。
  - 禁止裸 `goroutine`，用 `AddTask` / `Go()`。
  - `OnStart` 必须在 `AddTask` **之前**注册（否则永不触发）。
  - `SetRetry(非零, …)` 必须配 `GetTask().SetMaxRetryInterval(...)`，否则指数退避无上限。
  - 除 `Stop()` 外不得直接调用 `task.Task` 的方法；不得调用 `task.Job` 方法（`AddTask` 除外）。
- **日志**：slog 键值对，`t.Info("msg", "key", value)`；不新增 `fmt.Printf`/`log.Printf`。
- **不可变风格**：不就地修改共享对象，返回新值。
- **不降级**：恢复路径绝不把存储类型改写为 local；存储不可用时进入 pending 重试。
- **兼容**：既有公开 API 与配置键不得破坏；新增配置键必须有 `default:` 且 YAML 全小写。
- **门禁**：每个任务结束必须 `gofmt -w <changed-files>`、`go vet <affected-packages>`、
  `go test <affected-packages> -count=1` 全绿；并发相关再加 `-race`。
- 不修 Suggestions（报告中标为 Suggestion 的条目）。

---

## Task 1: 恢复管线信任边界（C1）

**目标**：sidecar 内容一律视为不可信输入；消除"任意文件读取 / 任意对象键覆盖"链路。

**文件**：`plugin/mp4/recovery_index.go`、`plugin/mp4/recovery_api_paths.go`、
`plugin/mp4/recovery_api.go`、`plugin/mp4/pkg/recovery_upload.go`、`plugin/mp4/pkg/recovery_db.go`、
`plugin/mp4/api.go`（如需）

**改动**：

1. `IndexHeader.FilePath` 建立信任校验，新增 `validateIndexHeaderPaths(header) error`：
   - `MediaPath` 必须绝对路径且通过既有 guard（已有）。
   - `FilePath` 为空：通过（保留 `StorageKey` 回退）。
   - **`storageType == local`**：`FilePath` 为绝对路径时必须与 `MediaPath` 同文件或位于其目录子树内，
     否则拒绝；相对路径拒绝任何 `..` 段（含 `\` 分隔），不做目录绑定。
   - **`storageType != local`（s3/cos/oss）**：`FilePath` 是对象键，必须是相对路径且不含 `..` 段；
     绝对路径一律拒绝（对象键不应是绝对路径，同目录绑定对对象存储无意义）。
   - 拒绝时 Error 记录 `filePath`/`mediaPath`/`storageType` 与命中的规则名，便于现场定位。
   - 绑定点在 `plugin/mp4/recovery_index.go` 的 `recoverSidecarFile`，guard 校验之后。
2. 默认扫描目录移除 `os.TempDir()`：`mediaTempDir()` 仅在 `storage.s3.tempdir` 显式配置时返回该目录；
   未配置时返回空，`defaultDirs()` 跳过空目录。
3. `POST /api/recovery/run` 在 `EnableLogin=false` 时**仍要求**本地回环或显式配置开关：
   新增配置 `record.recovery.allowunauthenticated`（`default:"false"`）。
   为 false 时，未启用登录则返回 403 并记录 Warn（说明如何开启）。
4. `recoveryObjectKey` 增加纵深防御：`FilePath` 为绝对路径或含 `..` 时返回空串
   （调用方已有 "missing object path" 错误分支）。不依赖上游是否调用过 `validateIndexHeaderPaths`。
5. `updateRecoveredRecord` 写入 `file_path` 前复用同一校验，不合法则 Warn 并跳过 DB 写入（不阻塞上传）。

**测试**（`plugin/mp4/recovery_api_test.go`、`plugin/mp4/pkg/recovery_upload_test.go` 新增）：
- local + `FilePath=/etc/shadow` 且 `MediaPath=<白名单内>` → 恢复被拒绝，DB 无写入。
- local + `FilePath` 与 `MediaPath` 同目录 → 正常通过。
- **对象存储 + `FilePath=live/test/rec.mp4`、`MediaPath=<tmp>/a.tmp` → 正常通过**（回归，不得被新校验破坏）。
- 对象存储 + `FilePath` 为绝对路径 → 拒绝。
- 任意 storageType + `FilePath` 含 `..` → 拒绝。
- 未配置 `s3.tempdir` 时 `defaultDirs()` 不含 `/tmp`。
- `run` 在 `EnableLogin=false` 且 `allowunauthenticated=false` 时返回 403。
- `recoveryObjectKey` 对绝对路径/`..` 返回空串。

**验收**：上述测试在旧代码上失败、新代码通过；`go test ./plugin/mp4/... -count=1` 全绿。

---

## Task 2: 索引解析硬化（C3）

**目标**：损坏/超大 sidecar 不能导致 OOM 或 panic。

**文件**：`plugin/mp4/pkg/recordindex.go`、`plugin/mp4/pkg/muxer.go`、`plugin/mp4/pkg/box/box.go`（只读参考）

**改动**：

1. `readIndexFrom` 增加记录数上限 `maxIndexRecords = 20_000_000`；超过时返回
   `fmt.Errorf("%w: index exceeds %d records", ErrIndexCorrupt, maxIndexRecords)`
   并把已读部分丢弃（返回 nil error 会让调用方把截断当成功）。
   **与 Task 7 的边界**：超上限 = 不可接受，返回错误（调用方拒绝恢复）；
   Task 7 新增的 `Truncated` 标志 = 结构性损坏但前缀可用，返回部分记录 + 标志。
   两者语义不同，Task 7 不得把超限改回"返回部分记录"。
2. `restoreTrackFromIndex`（`plugin/mp4/pkg/muxer.go`）解析 STSD 前做边界校验：
   - `len(it.STSD) >= 8`
   - 第一个 box 的 `size`（大端 uint32，`size==1` 时读 64 位）必须 `>= 8` 且 `<= len(it.STSD)`
   - 不满足返回 `fmt.Errorf("mp4: restore track %d: invalid stsd bounds", it.ID)`
3. 不改 `box.ReadFrom` 本身（影响面太大）；只在恢复入口做校验。

**测试**（`plugin/mp4/pkg/recordindex_test.go`、`plugin/mp4/pkg/muxer_restore_test.go` 新增）：
- 超过 `maxIndexRecords` 的索引 → `ErrIndexCorrupt`。
- `STSD` size=0 / size=4 / size > len → 返回错误，**不 panic**（用 `recover` 断言无 panic）。
- 合法 STSD 仍能恢复（回归）。

**验收**：`go test ./plugin/mp4/pkg/... -count=1 -race` 全绿。

---

## Task 3: 恢复 API 健壮性（Important 18/19/20/21）

**文件**：`plugin/mp4/recovery_api_paths.go`、`plugin/mp4/recovery_api_state.go`、`plugin/mp4/recovery_api.go`

**改动**：

1. `allowExisting` / `allowOutput` 返回**解析后的 canon 路径**而非 `filepath.Clean(path)`，
   消除检查与使用之间的符号链接 TOCTOU。调用方（`recoverSidecarFile`）用返回值做后续读写。
2. `startRun` 不再在 HTTP handler 内同步 `WaitStarted()`。gotask 的 `Promise` 没有
   context 感知的 `Await`，`startup` 又是私有字段，handler 内无法带超时等待。改为
   task 系统的 watcher 模式（同 `plugin.go` 的 `recordStartWatcher`）：
   - `startRun` 创建 run 后 `AddTask(&recoveryStartWatcher{child, state, run})`，handler 立即返回 202；
   - watcher 的 `Go()` 内调用 `child.WaitStarted()`：失败则把 run 标 failed 并记录原因，
     成功则把 run 标 started（或直接完成，视 Run 的语义）；
   - `GET /status/{id}` 在 watcher 未回填前显示 `running`，之后显示真实状态；
   - 这样既不在 handler 里阻塞，也不在 handler 里直接调用 task 方法（watcher 内部属于任务自身生命周期）。
   - 注意 `OnStart` 必须在 `AddTask` 之前注册（本仓已知陷阱）。
3. `discoverSidecars` / `scanRecoveryEntries` 的计数上限改为**遍历条目总数**上限（默认 10000），
   普通文件也计数；超限提前返回并 Warn。
4. `requireAdmin` 在 `p.Server == nil` 时返回 403（fail-closed）。

**测试**：
- `allowExisting` 返回 canon：构造 `allowed/a -> outside/b` 的符号链接，断言被拒（已有）且
  合法路径返回 EvalSymlinks 后的值。
- `run` 立即返回 202（不等启动结果）；任务启动失败时 `GET /status/{id}` 最终显示 `failed` 并带原因。
- `run` 的目标目录扫描超过上限时提前返回且不 OOM。
- 目录内 10001 个普通文件 → list 提前返回且不 OOM（用 `t.TempDir()` 批量建空文件，控制耗时）。
- `Server == nil` → 403。

**验收**：`go test ./plugin/mp4/... -count=1 -race` 全绿。

---

## Task 4: 限流器名额泄漏（C2 + Important 15/16/17）

**目标**：名额 exactly-once，任何 `AddTask` 拒绝路径都不泄漏。

**文件**：`plugin/mp4/limits.go`、`plugin.go`、`plugin/mp4/api.go`、`plugin/gb28181/downloaddialog.go`（如需）

**改动**：

1. **排队任务自己申请名额**（消除孤儿 waiter）：
   - `GateRecord` 只做 `l.CanQueue()` 廉价预检；不再 `Reserve()`。
   - `pendingAutoRecordTask.Go()` 内 `l.Acquire(ctx)`，失败即 `Refused` 并记 Error。
   - 这样 `AddTask` 被拒时没有任何 waiter 被遗留。
2. `GateRecord` 与 `recordOnPublish` 检查 `p.AddTask(...)` 返回值：
   - 若任务未启动（`GetState()` 仍为 INIT / promise 已 reject），记 Error 并（如适用）释放名额。
   - 同一处理也覆盖 `plugin/mp4/recovery_api_state.go` 的 `recoveryStartWatcher`：AddTask 被拒时
     把对应 run 标为 failed（否则 run 永久停在 `running`，Task 3 已暴露该边界）。
3. 排队路径的 `pub.Using(job)` 竞态：
   - `RecordWithDispose` 增加参数或在 `pendingAutoRecordTask.Go()` 中判断
     `pub.GetTask().IsStopped()`；已停止则不启动并释放名额。
   - 在 `plugin.go` 的 `Plugin.RecordWithDispose` 注释中说明：`Using` 必须在 publisher 所属 Job 上调用，
     排队路径通过 `pub.GetTask().Call(...)` 调度（若 gotask 提供 `Call`；否则加 `sync` 保护的注册函数）。
4. `MaxConcurrentRecordings` 覆盖面：`plugin/gb28181/downloaddialog.go:477` 与
   `plugin/mp4/api.go` 事件录像入口改走同一 gate（或至少在日志/文档中明确排除）。
   实现选择：GB28181 下载录像改为 `mp4Plugin.RecordWithDispose` 并先 `Acquire`；事件录像同样。
5. 挂死录制（`recoder.go` 的 `SetRetry(-1, time.Second)`）：**本任务不实现租约回收**
   （已裁决）。改为在 `limits.go` 的 `RecordLimiter` 注释中记录该已知限制：
   "启动成功但子任务持续失败的录制会一直持有名额；配置 max=N 后 N 路坏流可让自动录像停摆，
   需运维介入"。同时在 `docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md`
   的排障章节加一条对应说明。
6. `RecordGateResult` 零值改造（原 Task 9 第 3 条并入本任务，因为本任务要重写 GateRecord 路径）：
   新增 `RecordGateUnset` 作为 iota 0，`RecordGateAdmit` 后移；`recordOnPublish` 对未知值
   fail-closed（记 Error、不启动录制）。该接口是本 PR 新增，改零值无兼容成本。

**测试**（`plugin/mp4/limits_test.go` 新增）：
- 模拟 `owner` 已停止 → `AddTask` 拒绝 → 断言 `limiter` 的 `waiters` 为空、`active` 未变。
- 连续 10 次拒绝后 `Acquire` 仍能拿到名额（不泄漏）。
- `pendingAutoRecordTask` 在 publisher 已停止时不启动且名额归还。
- GB28181/事件录像入口经 gate（若纳入）。

**验收**：`go test ./plugin/mp4/... ./ -count=1 -race` 全绿。

---

## Task 5: 上传登记确认后才删 sidecar（C4 + Important 11/12/13/14）

**文件**：`upload_task.go`、`upload_recover.go`、`plugin/mp4/pkg/record.go`、
`plugin/mp4/pkg/recovery_upload.go`、`upload_retry.go`、`plugin/mp4/recovery_index.go`

**改动**：

1. `SaveFailedUpload` 返回 `error`；所有调用方处理：
   - `upload_recover.go`：Create 失败返回错误（调用方保留文件与 sidecar）。
   - `record.go` 两处：仅在**登记确认**后调用 `removeIndexAfterPendingRegistration`。
   - **登记失败时不得删除媒体文件**：`record.go` 现有 `if rerr != nil { os.Remove(tempPath); return }`
     会在 pending 满/DB 写失败时删掉唯一副本。改为保留文件（原地）并记 Error，
     sidecar 一并保留，让下次扫描能重试。
   - 同理 `t.db == nil` 分支的 `os.Remove(tempPath)` 只在**明确无法补传**时执行，
     并记 Warn 说明数据将丢失（无 DB 部署的已知取舍，保留但必须可见）。
2. 新增 `ConfirmPendingRegistration(db, localPath, objectKey) (bool, error)`：
   按 `local_path` + `object_key` 查询 `upload_tasks`，命中才返回 true。
   `recovery_upload.go:248` 已有等价逻辑，抽取复用。
3. `recoveryUploadRegistered` 查询失败改为返回 `(bool, error)`；错误时按"未确定"处理
   （不删 sidecar、不重复上传、记 Error）。
4. `recovery_upload.go:145-159` 已登记分支：`updateRecoveredRecord` 失败时**保留 sidecar**
   （返回错误而非 Warn 后删除）。
5. `upload_retry.go:45` pending 水位扫描错误记 Warn（去重，避免刷屏）。
6. `recovery_index.go:402-409` 启动扫描逐条记录 `stats.Errors`（Error 级，最多 20 条 + 总数）。

**测试**：
- `SaveFailedUpload` 在 DB 不可用时返回 error（用已关闭的 `*gorm.DB`）。
- 登记失败时 sidecar 保留（`record.go` 路径）。
- `recoveryUploadRegistered` 查询失败 → 不删 sidecar。
- 已登记分支 DB 回写失败 → sidecar 保留。
- pending 扫描错误产生 Warn（用不可读目录模拟）。

**验收**：`go test ./plugin/mp4/... ./pkg/storage/... ./ -count=1` 全绿。

---

## Task 6: 崩溃一致性窗口（Important 1/2/3/4）

**文件**：`plugin/mp4/pkg/record.go`、`pkg/storage/finalize.go`、`plugin/mp4/recovery_index.go`

**改动**：

1. **INSERT_RANGE 窗口**（`record.go` 的 `runInsertRangeFastPath`，约 :487-560）：
   - 新增扩展记录 `indexExtOffsetShift`（type=3，复用现有 `IndexWriter.writeExtension` 机制），
     payload 为 `{preLen int64, insertLen int64}`。
   - **写入顺序**：在 `storage.InsertRange` **之前**写 SHIFT 记录并 `Flush()`（该函数已做
     `buf.Flush()+f.Sync()`），再执行 `InsertRange`；`rewriteIndexRecords` 成功后追加一条
     `indexExtOffsetShiftDone`（type=4）。
   - **恢复判定**（必须确定性，不能猜）：
     | sidecar 状态 | 文件大小 | 结论 |
     |---|---|---|
     | 最后一条是 SHIFT | `size == preLen` | InsertRange 未生效 → 索引偏移**有效，不平移** |
     | 最后一条是 SHIFT | `size >= preLen + insertLen` | InsertRange 已生效、重写未完成 → 偏移 **+insertLen** |
     | 最后一条是 SHIFT | 其它 | 状态不确定 → **拒绝恢复**，保留 sidecar 并 Error |
     | 有 SHIFT_DONE | 任意 | 索引已按新偏移重写 → 直接用 |
     | 无 SHIFT | 任意 | 旧行为 |
   - 判定逻辑放在 `RestoreFromIndex` 之前（`recovery_index.go` 读取索引后），把平移后的记录交给 muxer。
   - 拒绝恢复时不 `markFailed`（保留 sidecar，人工可用 CLI 处理）。
2. **FinalizeFromTemp RENAME 前置**（`record.go:291-302`）：
   - 在调用 `finalize.FinalizeFromTemp(tempPath)` **之前**写 RENAME 记录（新路径已知）。
   - 保留旧顺序的回滚：finalize 失败时写一条 RENAME 回旧路径或删除该记录（扩展记录可追加，恢复以最后一条为准）。
3. **`hasTopLevelMoov` 加强**（`recovery_index.go:1018-1053`）：
   - 除 box 头外，校验 `moov` 的 `size` 落在文件长度内，且内含 `mvhd`；
   - 不满足视为未收尾，走重建路径。
4. **活跃登记时机**（`record.go:804-805`）：
   - `UnregisterActiveIndex` 从 `commitIndex` 移到 trailer 完成后（`writeTrailerTask.Run` 末尾的 defer）。
   - 若 trailer 任务失败，仍注销（defer 保证）。

**测试**：
- 构造 sidecar + 文件模拟 INSERT_RANGE 后状态（偏移已移、索引未移）→ 恢复结果正确或明确失败，不产出错位 MP4。
- finalize 失败后 sidecar 的空 RENAME 记录不会导致恢复找不到媒体。
- `hasTopLevelMoov` 对"moov 头存在但 size 超出文件"返回 false。
- 活跃登记在 trailer 完成前保持（用回调断言）。

**验收**：`go test ./plugin/mp4/... ./plugin/mp4/pkg/... -count=1 -race` 全绿。

---

## Task 7: 恢复正确性与 legacy 扫描（Important 5/6/7/8）

**文件**：`plugin/mp4/pkg/muxer.go`、`plugin/mp4/pkg/recordindex.go`、
`plugin/mp4/recovery_legacy.go`、`plugin/mp4/recovery_index.go`

**改动**：

1. **轨道静默丢弃**（`muxer.go:283-285`）：`RestoreFromIndex` 返回 `RestoreReport{DroppedTrackIDs []uint16}`；
   调用方 `recovery_index.go` 记录 Warn（`outcome=recovered, droppedTracks=...`），
   且 `updateRecoveredRecord` 在音轨丢失时不写 `AudioCodec`。
2. **索引截断信号**（`recordindex.go:388-405`）：`IndexFile` 增加 `Truncated bool` 与 `DroppedAt int64`；
   全零帧/TrackID==0/payload 超限时置位。`RecoveryItem.Truncated` 合并 `ErrIndexTruncated` 与该标志，
   日志与 API 输出 `truncated=true`。
3. **legacy NAL 校验**（`recovery_legacy.go:400-409`）：
   - `legacyMaxNALSize` 从 8MB 降到 1MB（仍覆盖 4K 关键帧）；
   - NAL type 收紧到白名单（H.264: 1/5/6/7/8 等 slice/SEI/参数集；H.265: VCL 0-31 与 VPS/SPS/PPS 32-34），不再接受任意 1..23；
   - 链式校验（`pos+4+n` 处也是合法长度字段）**若可实现则做**；受 `bufio.Reader` 只能向前 Peek 的限制，可用 `s` 的文件句柄 `ReadAt` 做一次前瞻（读 8 字节即可）。实现困难时在报告说明并只保留前两项；
   - 连续跳过超过 4MB 未命中 NAL 时，记 Warn 并在结果中标记 `HolesSkipped`。
4. **`ErrNoUsableSamples` 可重试**（`recovery_index.go:772-776`）：
   - 仅当 `info.Size() > 0` 且索引头合法时才落 `.failed`；
   - `size == 0` 按可重试处理（保留 sidecar，下次再试）。
5. **STSD 结构性校验**（Task 2 遗留，`plugin/mp4/pkg/box/stsd.go`）：
   - `Unmarshal` 校验解析出的 entry 数是否等于 `entry_count`；不等视为结构性损坏，返回错误；
   - `entry_count == 0` 视为损坏（空 stsd 无法恢复任何轨道）；由恢复入口映射为可重试/永久失败之一，
     并在 `RestoreFromIndex` 的 `RestoreReport` 中体现。

**测试**：
- 声明 video+audio 但音频记录全越界 → 返回 `DroppedTrackIDs=[audio]`，日志有 Warn。
- 索引中段全零帧 → `IndexFile.Truncated=true`，恢复结果 `truncated=true`。
- legacy：构造音频空洞 + 真实 NAL 交织，断言不吞掉后续 NAL（用已知长度的合成数据）。
- `size==0` → 不落 `.failed`，保留 sidecar。

**验收**：`go test ./plugin/mp4/... ./plugin/mp4/pkg/... -count=1` 全绿。

---

## Task 8: 重试语义与 MySQL 兼容（Important 9/10）

**文件**：`plugin/mp4/recovery_index.go`、`plugin/mp4/pkg/recovery_db.go`

**改动**：

1. **周期重扫**：`IndexRecoveryTask` 增加 `rerunInterval`（`default:"10m"`，`0` 禁用）。
   - `Start()` 在 `RunOnStart` 后注册一个 `TickTask` 子任务，每 `rerunInterval` 调一次 `RunOnce`
     （仅处理 `.recovering` 与未清理 sidecar；无残留时零开销）。
   - 或改为：日志文案改为 "retry on next startup or POST /api/recovery/run" 并保留手动触发。
   - **选择**：实现周期重扫（兑现"自动恢复"承诺），间隔可配置。
2. **MySQL 零时间**（`recovery_db.go:121/151-155`）：
   - 所有 `end_time = ?` 绑定 `time.Time{}` 改为 `end_time IS NULL`；
   - 若需兼容历史零值行，追加 `OR end_time < '1970-01-01'`（用字符串字面量，方言无关）。

**测试**：
- 周期重扫：注入短 `rerunInterval`（100ms），放入 `.recovering` 文件，断言自动重试（用 `t.TempDir()`）。
- `end_time IS NULL` 谓词在 sqlite 上命中（现有测试回归）；新增一个断言 SQL 不含 `0001-01-01` 的测试
  （用 gorm `DryRun` 取 SQL）。

**验收**：`go test ./plugin/mp4/... -count=1 -race` 全绿。

---

## Task 9: 默认值与类型契约（Important 22/23/24/25/26/27）

**文件**：`plugin/logrotate/index.go`、`plugin/mp4/pkg/index_config.go`、`plugin.go`、
`plugin/mp4/pkg/recovery_upload.go`

**改动**：

1. **logrotate**：
   - `Start()` 校验并夹紧：`Size==0` → 默认值 + Warn；`MaxFiles==0` → 默认值 + Warn；
     `Formatter==""` → 返回错误（fail fast）。
   - 新增启动时一次性清理：删除 `Path` 下**前缀不匹配但时间戳可解析**的旧 `*.log`
     （仅当 `FilePrefix != ""`），或至少 Warn 列出待手工清理的文件。
   - 在 `desc` 中标注"磁盘上限 = Size × MaxFiles"。
2. **IndexConfig 零值**：`Enable *bool`（nil = true）或 `SetIndexConfig` 对 `!Enable` 记 Warn 说明会关闭恢复。
   选择后者（改动小、无 API 破坏）。
3. **GateRecord 契约**：返回值改为 `RecordAdmission{Result; Release func()}`，
   `Release` 契约写明"必须幂等"；`limits.go` 已用 `sync.Once`，保留。
   （若改动面过大，至少补文档注释 + 第三方实现指引。）
4. **RecoveryUpload**：新增 `NewRecoveryUpload(...)` 构造校验（storageType 非空、output 绝对路径、
   sidecar 绝对路径）；local 分支保留 `os.Stat(OutputPath)` 作为纵深防御。

**测试**：
- logrotate `Size=0`/`MaxFiles=0` → 使用默认值并 Warn；`Formatter=""` → Start 返回错误。
- 旧日志（前缀不匹配）被清理或列出。
- `IndexConfig{FlushInterval: x}` 关闭索引时产生 Warn。
- `NewRecoveryUpload` 对空 storageType / 相对路径返回错误。
- local 分支 OutputPath 不存在 → 不删 sidecar。

**验收**：`go test ./plugin/logrotate/... ./plugin/mp4/... ./ -count=1` 全绿。

---

## 完成后

- 全部任务完成后跑整支审查（whole-branch review）。
- 不提交 PR/不推送，除非用户明确要求。
