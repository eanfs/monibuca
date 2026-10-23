# 录制完整性与异常恢复 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 progressive MP4 录制在进程被 SIGKILL/OOM/崩溃后，重启时自动把残留 tmp 恢复成可播 MP4 并上传到 S3，同时补齐录制/上传/恢复的可观测性与崩溃根因。

**Architecture:** 录制时向同目录的 `<media>.m7sidx` 追加轻量帧索引；正常停止只写 Commit 不删索引，等上传成功后才删。启动时恢复任务扫描索引与裸 tmp：有索引的按索引重建 moov，无索引的历史文件走 NALU 反向重建；产物写新文件后原子 rename，再按文件自身的 storageType 上传 S3。上传失败进现有 pending 重试。

**Tech Stack:** Go 1.24、`github.com/eanfs/gotask` 任务系统、`m7s.live/v5/plugin/mp4/pkg/box`、MinIO/S3、`ffprobe`（验收）。

**Spec:** `docs/superpowers/specs/2026-10-22-recording-integrity-design.md`

## Global Constraints

- 只支持 progressive MP4；不得复用或扩展到 fMP4（`fragment`）路径。
- 不改变 `StartRecord` 的 `(streamPath, filePath, fileName)` 去重语义，也不改变“同一 streamPath 可并发多录制”。
- 不实现续录。
- 禁止裸 goroutine；异步一律用 `task.Task/Work`，且 `OnStart` 必须在 `AddTask` 之前注册。
- sidecar：每帧固定 23 字节，1 秒 flush 一次，文件扩展名 `.m7sidx`，与媒体文件同目录。
- 正常停止只写 Commit 记录，**不删除** sidecar；只有上传成功或成功登记 pending 后才删除。
- 恢复必须幂等、可重复执行；一律“写新文件 + 原子 rename”，不原地修改。
- 恢复产物上传必须用 `Server.GetStorageForType(storageType)`，**不允许**降级为 local。
- 日志默认：单文件 20 MB、保留 30 天、最多 750 个文件。
- 改动后至少运行 `gofmt -w <files>` 与受影响包 `go test -count=1`；涉及并发改动用 `-race`。

## Review Focus

1. **sidecar 尾部被截断或损坏**：崩溃可能留下半条记录。恢复必须读到第一条坏记录就停止，而不是 panic 或读越界。
2. **索引记录越过媒体实际长度**：S3 文件写入有 1 MiB 用户态缓冲，索引可能指向 EOF 之后。恢复必须以文件实际长度为准截断，不能延长文件。
3. **恢复过程中再次被杀**：`.recovering` 状态必须能在下次启动重试并最终完成，不能永久卡死。
4. **S3 未就绪 / 凭证错误**：恢复产物不得写到 local，必须进 pending 并持续重试，且日志可查。
5. **无索引 tmp 的音频缺失**：反向重建的产物可能只有视频轨、时间戳为估算值，但仍必须是 `ffprobe` 可读、可播的文件。

---

## File Structure

| 文件 | 职责 |
|---|---|
| `plugin/mp4/pkg/recordindex.go`（新） | sidecar 格式：header JSON、23 字节记录、Commit/Rename、读取与校验 |
| `plugin/mp4/pkg/recordindex_test.go`（新） | 格式往返、损坏截断、越界停止 |
| `plugin/mp4/pkg/muxer.go`（改） | `RestoreFromIndex`、`WriteMoovOnly`、恢复时的 mdat 回填 |
| `plugin/mp4/pkg/record.go`（改） | 录制时写索引、Commit、RENAME、成功后删除 |
| `plugin/mp4/recovery_index.go`（新） | 启动恢复任务：扫描、定稿、原子 rename、幂等、并发上限 |
| `plugin/mp4/recovery_legacy.go`（新） | 无索引 tmp 的 NALU 反向重建 |
| `plugin/mp4/recovery_api.go`（新） | `/mp4/api/recovery/*` HTTP 接口、目录白名单、鉴权 |
| `plugin/mp4/limits.go`（新） | 录制并发软保护：有界等待队列 |
| `pkg/storage/s3.go`（改） | `tempdir` 配置生效，索引与媒体同目录 |
| `plugin/logrotate/index.go`（改） | 默认 20 MB / 30 天 / 750 文件 |
| `plugin/gb28181/device.go`（改/验证） | 插件级 `channels` 锁初始化 |
| `test/recovery_integration_test.go`（新） | kill -9 场景集成测试（可用 `-short` 跳过） |
| `docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md`（新） | 上线步骤与 S3 前置检查 |

---

### Task 0: 现场样本可行性验证（spike，先做）

**Files:**
- Create: `testdata/recovery/README.md`（记录样本来源与结论，样本本身不入库）
- Create: `/tmp/recovery-probe/main.go`（临时探针，不提交）

**Interfaces:**
- Consumes: 无
- Produces: 结论“B 档能否产出可播 MP4”，以及反向重建需要的最小解析逻辑；供 Task 7 参考。

- [ ] **Step 1: 从现场取 2 个样本**

Run:
```bash
ssh root@<FIELD_HOST> 'ls -S <DEPLOY_DIR>/monibuca/tmp/s3writer_*.tmp | head -2'
scp root@<FIELD_HOST>:/path/to/sample1.tmp /tmp/sample1.tmp
```
Expected: 拿到 2 个 200 MB–700 MB 的 tmp。

- [ ] **Step 2: 用 ffprobe 记录基线**

Run: `ffprobe -v error -show_entries format=duration /tmp/sample1.tmp`
Expected: 报 `moov atom not found`（确认是半成品）。

- [ ] **Step 3: 写最小探针：扫描 H.264 start code，统计帧边界**

探针只做一件事：统计 `00 00 01` / `00 00 00 01` NALU 数量、首个 IDR、是否有重复 SPS/PPS。不要生成 mp4。

- [ ] **Step 4: 生成一个可播 mp4 并验证**

用探针输出的帧边界构造最小 stsz/stco 表，套用 `plugin/mp4/pkg/box` 现成 box 写入 `/tmp/sample1.mp4`，Run: `ffprobe -v error -show_entries stream=codec_name,width,height /tmp/sample1.mp4` 且 `ffplay` 能播放。

- [ ] **Step 5: 记录结论**

在 `testdata/recovery/README.md` 写明：NALU 扫描是否可行、时间戳估算方式、音频是否能恢复、样本不可入库的原因。若不可行，Task 7 改为“只提供诊断报告 + 人工 ffmpeg 流程”。

---

### Task 1: sidecar 格式与读写

**Files:**
- Create: `plugin/mp4/pkg/recordindex.go`
- Test: `plugin/mp4/pkg/recordindex_test.go`

**Interfaces:**
- Consumes: 无
- Produces:
  - `type IndexHeader struct { Version int; StreamPath, MediaPath, TargetName, StorageKey, FilePath, StorageType string; StartTime time.Time; Tracks []IndexTrack }`
  - `type IndexTrack struct { ID uint16; FourCC string; Width, Height, SampleRate, Channels int; STSD []byte }`
  - `type IndexRecord struct { Flags uint8; TrackID uint16; Size uint32; Offset uint64; DTS, Duration uint32 }`
  - `func CreateIndex(path string, h IndexHeader) (*IndexWriter, error)`
  - `func (w *IndexWriter) Append(r IndexRecord) error`
  - `func (w *IndexWriter) Flush() error`
  - `func (w *IndexWriter) Commit(totalDuration uint32, samples uint64) error`
  - `func (w *IndexWriter) Rename(mediaPath string) error`
  - `func (w *IndexWriter) Close() error`
  - `func RemoveIndex(path string) error`
  - `type IndexFile struct { Header IndexHeader; Records []IndexRecord; Committed bool; MediaPath string }`
  - `func ReadIndex(path string) (*IndexFile, error)`

- [ ] **Step 1: 写失败测试——往返读写**

```go
func TestIndexWriteReadRoundTrip(t *testing.T) {
    dir := t.TempDir()
    p := filepath.Join(dir, "a.tmp.m7sidx")
    w, err := CreateIndex(p, IndexHeader{Version: 1, StreamPath: "live/a", MediaPath: "/tmp/a.tmp", StorageType: "s3", Tracks: []IndexTrack{{ID: 1, FourCC: "avc1", Width: 1920, Height: 1080}}})
    if err != nil { t.Fatal(err) }
    recs := []IndexRecord{{Flags: 1, TrackID: 1, Size: 100, Offset: 48, DTS: 0, Duration: 40}, {TrackID: 1, Size: 200, Offset: 148, DTS: 40, Duration: 40}}
    for _, r := range recs { if err := w.Append(r); err != nil { t.Fatal(err) } }
    if err := w.Commit(80, 2); err != nil { t.Fatal(err) }
    _ = w.Close()
    f, err := ReadIndex(p)
    if err != nil { t.Fatal(err) }
    if !f.Committed || len(f.Records) != 2 || f.Records[1].Offset != 148 { t.Fatalf("got %+v", f) }
}
```

- [ ] **Step 2: 运行，确认失败**

Run: `go test ./plugin/mp4/pkg -run TestIndexWriteReadRoundTrip -count=1`
Expected: FAIL，`undefined: CreateIndex`。

- [ ] **Step 3: 实现格式（header + 23B 记录 + Commit + Rename）**

Header：`[magic 7B "M7SIDX1"][headerLen uint32 BE][headerJSON]`；Record：`flags(1) trackID(2) size(4) offset(8) dts(4) duration(4)`；Commit/Rename 用 `flags=0xFF` 的扩展记录。写入用 `bufio.NewWriterSize(f, 4096)`，`Append` 后按 1 秒节流 `Flush`。

- [ ] **Step 4: 写失败测试——损坏尾部与越界**

覆盖两个用例：截断到半条记录时 `ReadIndex` 返回已读记录且不报错；记录 `Offset+Size > fileSize` 时由调用方停止（`ReadIndex` 只负责格式，不校验媒体长度）。

- [ ] **Step 5: 运行全部测试并通过**

Run: `go test ./plugin/mp4/pkg -run 'TestIndex' -count=1 -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
gofmt -w plugin/mp4/pkg/recordindex.go plugin/mp4/pkg/recordindex_test.go
git add plugin/mp4/pkg/recordindex.go plugin/mp4/pkg/recordindex_test.go
git commit -m "feat(mp4): 录像伴随索引 sidecar 格式与读写"
```

---

### Task 2: S3 临时目录可配置，索引与媒体同目录

**Files:**
- Modify: `pkg/storage/s3.go`（`S3StorageConfig` 与 `CreateTemp` 调用点，约 40-60 行与 519 行）
- Test: `pkg/storage/s3_tempdir_test.go`（新）

**Interfaces:**
- Consumes: 配置系统
- Produces: `S3StorageConfig.TempDir string`（yaml `tempdir`）；`NewS3Writer` 创建的临时文件位于该目录，默认 `/tmp`。

- [ ] **Step 1: 写失败测试**

```go
func TestS3CreateTempUsesConfiguredDir(t *testing.T) {
    dir := t.TempDir()
    cfg := S3StorageConfig{TempDir: dir}
    f, err := os.CreateTemp(effectiveTempDir(&cfg), "s3writer_*.tmp")
    if err != nil { t.Fatal(err) }
    defer f.Close()
    if filepath.Dir(f.Name()) != dir { t.Fatalf("temp file dir = %s, want %s", filepath.Dir(f.Name()), dir) }
}
```

- [ ] **Step 2: 运行确认失败**，`go test ./pkg/storage -run TestS3CreateTempUsesConfiguredDir` → FAIL。

- [ ] **Step 3: 实现**

新增字段与 `effectiveTempDir(cfg *S3StorageConfig) string`（空值返回 `os.TempDir()`）；把 `s3.go:519` 的 `os.CreateTemp("", ...)` 改为使用该目录。

- [ ] **Step 4: 运行通过并跑该包测试**

Run: `go test ./pkg/storage -count=1` → PASS。

- [ ] **Step 5: 提交**，消息 `feat(storage): S3 临时目录可配置`。

---

### Task 3: 录制时写 sidecar（含 Commit 与提交式删除）

**Files:**
- Modify: `plugin/mp4/pkg/record.go`（`Recorder` 结构与 `WriteSample` 调用点、`Dispose`、`handoffToUploadQueue`、`finalizeUploadTask.Go`）
- Test: `plugin/mp4/pkg/recordindex_record_test.go`（新）

**Interfaces:**
- Consumes: Task 1 的 `IndexWriter`/`IndexRecord`；Task 2 的临时目录。
- Produces: 录制结束后同目录存在 `.m7sidx`；上传成功后该文件被删除。

- [ ] **Step 1: 写失败测试——录制两个 sample 后索引有 2 条记录**

用一个内存 `storage.File` 桩（或 `local` 存储写临时目录），调用 `Recorder` 的写样本路径，断言索引文件存在且 `ReadIndex` 得到 2 条、`Committed==true`。

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现**

`Recorder` 增加 `index *IndexWriter`；在 sample 写入存储成功后调用 `Append`；`Dispose` 调 `Commit` 后只 `Close` 不删除；`finalizeUploadTask.Go` 上传成功分支调用 `RemoveIndex`；失败分支保留。

- [ ] **Step 4: 写失败测试——finalize 换路径时追加 RENAME**

构造 `FinalizeFromTemp` 场景，断言 `ReadIndex(path).MediaPath` 等于新路径。

- [ ] **Step 5: 实现 RENAME 并跑测试通过**

Run: `go test ./plugin/mp4/pkg -run 'TestIndex' -count=1`。

- [ ] **Step 6: 提交**。

---

### Task 4: A 档——从索引重建 moov

**Files:**
- Modify: `plugin/mp4/pkg/muxer.go`
- Test: `plugin/mp4/pkg/muxer_restore_test.go`（新）

**Interfaces:**
- Consumes: Task 1 的 `IndexHeader`/`IndexRecord`。
- Produces:
  - `func (m *Muxer) RestoreFromIndex(h IndexHeader, records []IndexRecord) error`
  - `func (m *Muxer) WriteMoovOnly(w io.Writer) error`

- [ ] **Step 1: 写失败测试——恢复后 mp4 可被 box 解析且 sample 数与索引一致**

```go
func TestRestoreFromIndexBuildsPlayableMoov(t *testing.T) {
    // 造一个 [ftyp][mdat(占位)] + 2 个 sample 的媒体文件，配 2 条 IndexRecord
    // RestoreFromIndex 后 WriteMoovOnly 到新文件，再用 box 解析，断言 stsz.Count==2
}
```

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现**

从 header 的 `Tracks` 建 `Track`（用 `STSD` 直接注入 stsd，避免重新解析码流）；按记录逐条构造 `box.Sample` 并 `AddSampleEntry`；`CurrentOffset` 设为媒体实际长度。

- [ ] **Step 4: 写失败测试——越界记录被截断**

记录 `Offset+Size > fileSize` 时，`RestoreFromIndex` 只接受越界前的记录并返回 `ErrIndexTruncated`。

- [ ] **Step 5: 跑测试通过**：`go test ./plugin/mp4/pkg -run 'TestRestore|TestIndex' -count=1`。

- [ ] **Step 6: 提交**。

---

### Task 5: 启动恢复任务（扫描/定稿/原子 rename/幂等/并发）

**Files:**
- Create: `plugin/mp4/recovery_index.go`
- Modify: `plugin/mp4/index.go`（任务注册，与 `RecordRecoveryTask` 合并）
- Test: `plugin/mp4/recovery_index_test.go`（新）

**Interfaces:**
- Consumes: Task 4 的 `RestoreFromIndex`；Task 1 的 `ReadIndex`；现有 `RecordRecoveryTask`。
- Produces: `type IndexRecoveryTask struct { task.Work; ... }`，暴露 `RunOnce(dirs []string) (RecoveryStats, error)` 供 API 调用。

- [ ] **Step 1: 写失败测试——造 sidecar+媒体，跑 RunOnce，断言新文件可解析、旧文件被 rename、sidecar 删除**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现扫描与定稿**

扫描 `record tempdir`、`pending_uploads`、配置 `dirs`；对每个 `*.m7sidx` 读取 → 截断 → 写新文件 → `os.Rename` → 删 sidecar → 提交上传队列。先跑现有 `RecordRecoveryTask` 补 DB，再跑索引定稿。并发上限默认 2（`recovery.concurrency`）。

- [ ] **Step 4: 写失败测试——恢复中再杀（幂等）**

模拟：把 sidecar 改成 `.recovering` 后中断，再次 `RunOnce` 能继续完成。

- [ ] **Step 5: 实现 `.recovering` 重试并跑测试通过**。

- [ ] **Step 6: 提交**。

---

### Task 6: 恢复产物自动上传 S3（不降级 local）

**Files:**
- Modify: `plugin/mp4/recovery_index.go`、`plugin/mp4/pkg/record.go`（复用上传队列）
- Test: `plugin/mp4/recovery_upload_test.go`（新）

**Interfaces:**
- Consumes: `Server.GetStorageForType(storageType)`（已存在）；`uploadQueueTask`。
- Produces: 恢复完成后调用上传队列；S3 未就绪时文件保留并登记 pending。

- [ ] **Step 1: 写失败测试——用 fake storage registry，断言上传走 `s3` 而非 `local`**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现**

恢复产物构造 `finalizeUploadTask` 等价的入队参数，使用 `GetStorageForType(header.StorageType)`；解析失败时调用 `upload_manager` 的 pending 登记并返回可重试错误，**不**改用 `GetStorage()`。

- [ ] **Step 4: 写失败测试——S3 解析失败时本地文件不被删除**

- [ ] **Step 5: 实现并跑测试通过**。

- [ ] **Step 6: 提交**。

---

### Task 7: B 档——无索引 tmp 反向重建 + CLI

**Files:**
- Create: `plugin/mp4/recovery_legacy.go`
- Create: `cmd/m7s-recover/main.go`（或 `example/recover/main.go`，沿用仓库 CLI 习惯）
- Test: `plugin/mp4/recovery_legacy_test.go`

**Interfaces:**
- Consumes: Task 0 的结论；`plugin/mp4/pkg/box`。
- Produces: `func RebuildFromMDAT(mediaPath string, opts LegacyOptions) (LegacyResult, error)`；CLI 参数 `-dir`、`-dry-run`、`-concurrency`。

- [ ] **Step 1: 写失败测试——用 Task 0 的样本帧边界数据（或构造小样本）断言产物可被 box 解析、有视频轨**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现 NALU 扫描与 stbl 生成**

只支持 H.264/H.265 Annex-B；时间戳按固定帧率估算（默认 25 fps，可配置）；音频轨缺失时在结果中标记 `AudioRecovered=false`。

- [ ] **Step 4: 实现 CLI（dry-run 只输出报告）**

- [ ] **Step 5: 跑测试通过**，并用现场样本手动验证一次 `ffprobe`。

- [ ] **Step 6: 提交**。

---

### Task 8: 可观测性日志（录制起止、上传失败、恢复）

**Files:**
- Modify: `plugin/mp4/pkg/record.go`、`plugin/mp4/recovery_index.go`、`pkg/storage/upload_manager.go`
- Test: `plugin/mp4/observability_test.go`（新，用 slog 测试 handler）

**Interfaces:**
- Consumes: 现有 `task.Task` 日志方法。
- Produces: 固定字段名，供日志检索：`streamPath`、`file`、`durationMs`、`size`、`samples`、`retryCount`、`errorClass`、`nextRetryAt`、`scanned`、`recovered`、`failed`。

- [ ] **Step 1: 写失败测试——录制开始/结束各产生一条含指定字段的日志**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现录制开始/结束/索引异常日志**

- [ ] **Step 4: 写失败测试——上传失败日志含 errorClass 与 nextRetryAt**

- [ ] **Step 5: 实现上传/恢复日志并跑测试通过**。

- [ ] **Step 6: 提交**。

---

### Task 9: 恢复 API（list/run/status）

**Files:**
- Create: `plugin/mp4/recovery_api.go`
- Modify: `plugin/mp4/index.go`（注册 `RegisterHandler`）
- Test: `plugin/mp4/recovery_api_test.go`

**Interfaces:**
- Consumes: Task 5 的 `RunOnce` 与统计结构。
- Produces: `GET /mp4/api/recovery/list`、`POST /mp4/api/recovery/run`、`GET /mp4/api/recovery/status/{id}`。

- [ ] **Step 1: 写失败测试——`httptest` 调用 list 返回 JSON 数组**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现三个 handler + 目录白名单 + 管理员鉴权**

白名单来自配置 `recovery.dirs`；`run` 拒绝 mtime 小于 `recovery.minage`（默认 5m）的文件和活跃录制对应文件。注意：**该 5m 阈值只作用于手动 run**；启动自动扫描只跑一次，使用短阈值 `max(5s, 2×index.flushinterval)`，否则崩溃后秒级重启的残留（age 仅数秒）会被全部跳过且无第二次自动扫描（2026-10-22 裁决）。

- [ ] **Step 4: 写失败测试——白名单外目录返回 403**

- [ ] **Step 5: 实现并跑测试通过**。

- [ ] **Step 6: 提交**。

---

### Task 10: 日志轮转默认值

**Files:**
- Modify: `plugin/logrotate/index.go:18-23`
- Test: `plugin/logrotate/defaults_test.go`（新）

**Interfaces:**
- Consumes: 无
- Produces: `Size=20971520`、`Days=30`、`MaxFiles=750` 的默认值。

- [ ] **Step 1: 写失败测试——反射读取 struct tag 默认值断言三个数字**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 修改默认 tag**（20 MB / 30 天 / 750）。

- [ ] **Step 4: 验证 rotoslog 在“按小时 + 20 MB”下的实际切分**

在本机写一个临时程序，快速写入 25 MB 日志，断言产生两个文件且每个不超过 20 MB；若不满足，记录到 runbook 并调整 `formatter`。

- [ ] **Step 5: 跑测试通过并提交**。

---

### Task 11: GB28181 插件级 channels 锁

**Files:**
- Modify: `plugin/gb28181/device.go`（`ensureCollectionMutex`）
- Test: `plugin/gb28181/collection_race_test.go`（新）

**Interfaces:**
- Consumes: 现有 `ensureCollectionMutex`
- Produces: 插件级 `plugin.channels` 与设备级 `d.channels` 都有非 nil 锁。

- [ ] **Step 1: 写失败测试——并发调用 `addOrUpdateChannel` 与读取 plugin.channels，`-race` 下不得报竞争**

- [ ] **Step 2: 运行确认失败（或确认当前已通过，则记录结论）**

- [ ] **Step 3: 扩展 `ensureCollectionMutex` 覆盖插件级集合**

- [ ] **Step 4: 跑 `go test -race ./plugin/gb28181 -run TestConcurrentChannelUpdate -count=3` 通过**。

- [ ] **Step 5: 提交**。

---

### Task 12: 录制并发软保护（有界等待队列）

**Files:**
- Create: `plugin/mp4/limits.go`
- Modify: `plugin/mp4/api.go`（`StartRecord` 超限返回 503）
- Test: `plugin/mp4/limits_test.go`

**Interfaces:**
- Consumes: 配置 `limits.maxconcurrentrecordings`、`limits.maxmemorymb`
- Produces: `type RecordLimiter struct{...}`，方法 `Acquire(ctx) error`、`Release()`；队列容量 = 上限 × 4，排队超时 30s。

- [ ] **Step 1: 写失败测试——上限 1、并发 3 时第 2 个排队、第 3 个超时返回错误**

- [ ] **Step 2: 运行确认失败**。

- [ ] **Step 3: 实现 limiter 并接入 `StartRecord`**

超限时 API 返回 503；onpub 录制记录 Error 并保留参数。

- [ ] **Step 4: 写失败测试——Release 后排队的请求能成功**

- [ ] **Step 5: 实现并跑测试通过**。

- [ ] **Step 6: 提交**。

---

### Task 13: 集成验收与上线手册

**Files:**
- Create: `test/recovery_integration_test.go`
- Create: `docs/superpowers/runbooks/2026-10-22-recording-recovery-deploy.md`

**Interfaces:**
- Consumes: 前序全部任务
- Produces: 可重复执行的验收流程；上线步骤含 S3 endpoint 修正与回滚。

- [ ] **Step 1: 写集成测试（`-short` 跳过）**

用 `ffmpeg` 生成测试流推入，启动录制，`kill -9` 进程，重启，断言自动恢复 + 文件可 `ffprobe` + fake S3 收到对象。

- [ ] **Step 2: 运行 `go test ./test -run TestRecoveryAfterKill -count=1` 通过**。

- [ ] **Step 3: 写上线手册**

内容：备份 tmp/db → 修正 S3 endpoint（`127.0.0.1:9000` 或 `<INTERNAL_S3_ENDPOINT>`）→ 提升 `mem_limit` 并分批 `pull_on_start` → 换镜像 → 观察恢复日志与 `/mp4/api/recovery/list` → 用 `mc stat` 抽查对象 → 回滚步骤。

- [ ] **Step 4: 跑全量受影响包测试**

Run: `go test ./plugin/mp4/... ./pkg/storage/... ./plugin/gb28181/... -count=1`
Expected: PASS。

- [ ] **Step 5: 提交**。

---

## Self-Review

- **Spec coverage**：目标 1（恢复+上传）→ Task 3-7；目标 2（可观测性）→ Task 8-10；目标 3（根因）→ Task 11-12；非目标（不做续录/fMP4）→ 计划未引入；上线前置 → Task 13。sidecar 提交式删除 → Task 3；`.recovering` 幂等 → Task 5；不降级 local → Task 6；日志 20 MB/30 天 → Task 10。
- **Review Focus 覆盖**：损坏尾部 → Task 1 Step 4；索引越界 → Task 4 Step 4；恢复中再杀 → Task 5 Step 4；S3 未就绪 → Task 6 Step 4；无索引音频缺失 → Task 7 Step 3。
- **类型一致**：`IndexHeader/IndexRecord/IndexWriter/ReadIndex` 在 Task 1 定义，Task 3/4/5/6 只引用；`RunOnce` 在 Task 5 定义，Task 9 引用；`RebuildFromMDAT` 在 Task 7 定义，CLI 引用。
- **比例**：计划约 500 行，spec 约 260 行；代码块仅测试断言与签名，无完整实现体。
