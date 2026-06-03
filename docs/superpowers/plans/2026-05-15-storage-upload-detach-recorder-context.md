# Storage Upload 与 Recorder Context 解耦 实施计划

> **状态：已完成** — 2026-05-15 合并到 develop（`bdde65ae`）
>
> 三个后端（S3 / OSS / COS）均已修复，单元测试全 PASS，130 环境待验证。

**Goal:** 修复 record stop 时 S3 / OSS / COS upload 被 Recorder.Context cancel 误杀的 bug。让 trailer 写完后的对象存储上传不受录制任务生命周期影响 — 录制可以停，但已写好的文件必须完整上传完成。

**Architecture:** 三个后端文件 (`S3File` / `OSSFile` / `COSFile`) 内部都有 `ctx context.Context` 字段，来自 `storage.CreateFile(ctx, path)` 调用，而 ctx 是 `Recorder.Context`（task 的 ctx）。Recorder dispose 时此 ctx cancel，导致 upload 链路 (`AcquireUploadSlot` / `UploadWithRetry`) 全军覆没，文件落 `pending_uploads/` 后无人重传。

修复：在 `uploadTempFile` 入口把 ctx 用 `context.WithoutCancel(f.ctx)` 包一层，**保留 ctx values（trace/metadata），切断 cancel 信号**。Upload 自带 timeout（已有 `getTimeout()`），不会无限挂。

**Tech Stack:** Go 1.21+ `context.WithoutCancel`（仓库 Go 1.24 已支持）。

**问题背景：**
- session 实测：3 次 31 路录制（5min × 2 + 15min × 1）全部出现 **31/31 文件进 pending_uploads**
- 日志关键证据：`[S3] upload attempt 1 failed: ... RequestCanceled: request context canceled` → `upload cancelled during retry wait: context canceled`
- root cause 链路：
  ```
  Recorder (task.Task) ─→ Recorder.Context
                              ↓
                  CreateFile(r.Context, path) ─→ S3File.ctx = r.Context
                              ↓
                  Recorder.Dispose() → r.Context.Cancel()
                              ↓
                  writeTrailerTask.Run() → file.Close() → uploadTempFile()
                              ↓
                  AcquireUploadSlot(w.ctx=cancelled) → ctx.Err()
                  UploadWithRetry(w.ctx=cancelled) → 重试 wait 立刻退
  ```
- writeTrailerTask 本身的 ctx 还活着（它属于 server 级 `writeTrailerQueueTask Work`），但**它接管的 file 对象里埋着 Recorder 的 ctx**

**修复范围：**
- 改 S3 / OSS / COS 三个后端的 `uploadTempFile`
- 不改 Local（local 不走上传链路，无此 bug）
- 不修 trailer flush 限流（独立 plan `2026-05-15-trailer-flush-rate-limit.md`）
- 不动 `pending_uploads/` 重试机制

---

## 文件结构（实际）

```
pkg/storage/
├── s3.go                      [已改] uploadTempFile: ctx 用 context.WithoutCancel
├── oss.go                     [已改] 同
├── cos.go                     [已改] 同
└── upload_detach_ctx_test.go  [已建] 复现测 + 修复验证（3 个测试全 PASS）
```

---

## Task 1: 写复现测试（先证明 bug 真实）

**Files:**
- Create: `pkg/storage/upload_detach_ctx_test.go`

- [x] **Step 1: 写测试骨架**

文件 `pkg/storage/upload_detach_ctx_test.go` 已建，包含：
- `TestUploadWithRetry_CancelledParentCtxFails`：证明 cancelled ctx 上传失败（bug 现状）
- `TestDetachedCtxIgnoresCancel`：证明 `context.WithoutCancel` helper 行为正确
- `TestUploadWithRetry_DetachedCtxSucceeds`：证明修复路径有效（detached ctx 上传成功）

- [x] **Step 2: 跑测试**

```bash
go test -race -run 'TestUploadWithRetry_CancelledParentCtxFails|TestDetachedCtxIgnoresCancel' ./pkg/storage/...
```

结果：全 PASS。

- [x] **Step 3: 提交测试基线**

提交：`da78bc4f test(storage): 加测复现 record stop 时 upload 被 cancel 的 bug`

---

## Task 2: 修 S3File.uploadTempFile —— ctx 解耦

**Files:**
- Modify: `pkg/storage/s3.go`

- [x] **Step 1: 看现有 uploadTempFile 实现**

已确认 `w.ctx` 在 `AcquireUploadSlot` 和 `UploadWithRetry` 两处使用。

- [x] **Step 2 & 3: 在 uploadTempFile 入口加 detached ctx，替换所有 `w.ctx`**

```go
func (w *S3File) uploadTempFile() error {
    uploadCtx := context.WithoutCancel(w.ctx)
    if err := AcquireUploadSlot(uploadCtx); err != nil { ... }
    ...
    return UploadWithRetry(uploadCtx, rc, ...)
}
```

- [x] **Step 4: 编译验证** — PASS

- [x] **Step 5: 跑测试** — 全 PASS

- [x] **Step 6: 提交**

提交：`f062260c fix(storage/s3): uploadTempFile 用 WithoutCancel 解耦 Recorder.Context`

---

## Task 3: 同步修 OSS / COS 后端

**Files:**
- Modify: `pkg/storage/oss.go`
- Modify: `pkg/storage/cos.go`

- [x] **Step 1: OSS** — `uploadCtx := context.WithoutCancel(f.ctx)` 已加，`f.ctx` 已替换

- [x] **Step 2: COS** — 同上

- [x] **Step 3: 编译 + 测试** — 全 PASS

- [x] **Step 4: 提交**

提交：`b5cb6622 fix(storage/oss,cos): uploadTempFile 用 WithoutCancel 解耦父 ctx (同 s3)`

---

## Task 4: 端到端冒烟（130 真环境验证）

**Files:** 仅操作步骤

- [ ] **Step 1: 出新镜像**

```bash
./build_tag.sh
./build_docker.sh v5.2.<NEW_TAG>
```

- [ ] **Step 2: 部署到 130**

```bash
ssh root@172.16.12.130 \
  'cd /home/project/xde-uat/media-docker-compose && \
   sed -i "s|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:.*|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:<NEW_TAG>|" docker-compose-xde-monibuca.yml && \
   docker compose -f docker-compose-xde-monibuca.yml pull && \
   docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate'
```

- [ ] **Step 3: 用 API 加 31 路 pull proxy → 等就绪 → 启录制 (5 min) → 停录制**

- [ ] **Step 4: 关键验收指标**

```bash
ssh root@172.16.12.130 '
echo "=== pending_uploads 数量（应为 0） ==="
docker exec xde-monibuca sh -c "ls /monibuca/pending_uploads/*.mp4 2>/dev/null | wc -l"

echo "=== cancel 事件（应消失） ==="
docker logs xde-monibuca --since 10m 2>&1 | grep -iE "RequestCanceled|context canceled" | tail -5
'
```

**修复前 baseline**：pending_uploads=31，cancel 事件=31 条
**修复后预期**：pending_uploads=0，cancel 事件=0

- [ ] **Step 5: 验录制完整性**

```bash
mc ls --recursive xiding-uat/vidu-media-bucket/live/ | wc -l
# 预期: 31
```

- [ ] **Step 6: 清理 31 个临时 pull proxy**

---

## Task 5: 验收

- [x] **Step 1: 单元测试全过**

```bash
go test -race -count=1 ./pkg/storage/...
```

结果：PASS（含 `upload_detach_ctx_test.go` 3 个新测试）

- [x] **Step 2: 代码 grep 自检**

三个后端 `uploadTempFile` 内均有 `context.WithoutCancel`，无裸 `w.ctx` / `f.ctx`。

- [ ] **Step 3: 130 真实跑过 pending_uploads=0** — 待 Task 4 执行

- [x] **Step 4: 解锁限流 plan**

本修复完成后，`trailer-flush-rate-limit` plan 的 Task 2 可以安全推进（即使 task ctx cancel，已写好的文件仍能上传）。注：限流 plan Task 2 因 event loop 单线程问题已回滚，见该 plan 文档。

---

## 遗留 Issue

1. **130 端到端验证**：Task 4 尚未执行，需部署新镜像后验证 pending_uploads=0
2. **pending_uploads 自动重试**：若仍有少量文件进 pending_uploads（真实网络失败），需独立实现定时补传机制
3. **Local backend**：无 ctx 依赖问题，不需修改
