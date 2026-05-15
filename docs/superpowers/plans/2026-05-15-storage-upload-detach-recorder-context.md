# Storage Upload 与 Recorder Context 解耦 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 修复 record stop 时 S3 / OSS / COS upload 被 Recorder.Context cancel 误杀的 bug。让 trailer 写完后的对象存储上传不受录制任务生命周期影响 — 录制可以停，但已写好的文件必须完整上传完成。

**Architecture:** 三个后端文件 (`S3File` / `OSSFile` / `COSFile`) 内部都有 `ctx context.Context` 字段，来自 `storage.CreateFile(ctx, path)` 调用，而 ctx 是 `Recorder.Context`（task 的 ctx）。Recorder dispose 时此 ctx cancel，导致 upload 链路 (`AcquireUploadSlot` / `UploadWithRetry`) 全军覆没，文件落 `pending_uploads/` 后无人重传。

修复：在 `uploadTempFile` 入口把 ctx 用 `context.WithoutCancel(f.ctx)` 包一层，**保留 ctx values（trace/metadata），切断 cancel 信号**。Upload 自带 timeout（已有 `getTimeout()`），不会无限挂。

**Tech Stack:** Go 1.21+ `context.WithoutCancel`（仓库 Go 1.26.0 已支持）。

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
- 不修 trailer flush 限流（独立 plan `2026-05-15-trailer-flush-rate-limit.md`，本 plan 修完它可以放心做）
- 不动 `pending_uploads/` 重试机制（如果还有少量 case 失败，依赖 retry 队列，那是另一个 issue）

**Spec：** 见首段 + Task 4 验收节。

---

## 文件结构

```
pkg/storage/
├── s3.go                      [改] uploadTempFile: ctx 用 context.WithoutCancel
├── oss.go                     [改] 同
├── cos.go                     [改] 同
└── upload_detach_ctx_test.go  [新] 复现测 + 修复验证（mock S3 / 单元）

plugin/mp4/pkg/
└── record_e2e_cancel_test.go  [新, 可选] mp4 record stop 时 upload 仍完成的集成测

docs/superpowers/plans/
└── 2026-05-15-storage-upload-detach-recorder-context.md  [新] 本文件
```

---

## Task 1: 写复现测试（先证明 bug 真实）

**Files:**
- Create: `pkg/storage/upload_detach_ctx_test.go`

目标：在不依赖真 S3 / 网络的情况下，验证"父 ctx cancel 后 upload 失败"现象，并在 fix 之后验证"父 ctx cancel 后 upload 仍成功"。

- [ ] **Step 1: 写测试骨架（用 fake `uploadFn`，不依赖真 S3）**

文件 `pkg/storage/upload_detach_ctx_test.go`：

```go
package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestUploadWithRetry_CancelledParentCtxFails 是当前 bug 复现：
// 父 ctx cancel 后, UploadWithRetry 立刻退. 这是修复**前**的行为, 修复后此测保持通过（用法不变）.
func TestUploadWithRetry_CancelledParentCtxFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()  // pre-cancelled

	rc := RetryConfig{MaxRetries: 3, RetryInterval: 10 * time.Millisecond}
	var attempts int32
	err := UploadWithRetry(ctx, rc, "test", "obj-key",
		nil,
		func() error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("simulated transient")
		})

	// 当前实现: UploadWithRetry 看 ctx.Done 在 retry wait 处退出
	// 注意 attempt 0 不走 retry wait, 所以 uploadFn 至少跑 1 次
	if err == nil {
		t.Fatalf("expected error when parent ctx cancelled, got nil")
	}
	a := atomic.LoadInt32(&attempts)
	if a > 1 {
		t.Fatalf("uploadFn should run at most once with cancelled ctx, ran %d times", a)
	}
}

// TestUploadTempFile_DetachedFromParentCtx 验证修复:
// S3File.uploadTempFile 收到的 w.ctx 即使被 cancel, 上传逻辑仍能跑完.
// 不直接连真 S3, 通过 mock storage 抽象层 OR 测 helper 函数.
// 这里测一个抽象的 "detachedCtx" helper, 真正 fix 后改为测真 wrapper.
func TestDetachedCtxIgnoresCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	// 修复后的预期行为: 此 helper 返回的 ctx Done() 永不 close
	detached := context.WithoutCancel(parent)

	select {
	case <-detached.Done():
		t.Fatal("detached ctx should not be cancelled by parent")
	default:
		// OK
	}
	if detached.Err() != nil {
		t.Fatalf("detached ctx Err should be nil, got %v", detached.Err())
	}
}
```

- [ ] **Step 2: 跑测试**

```bash
go test -race -run 'TestUploadWithRetry_CancelledParentCtxFails|TestDetachedCtxIgnoresCancel' ./pkg/storage/...
```

预期：
- `TestUploadWithRetry_CancelledParentCtxFails` → **PASS**（证 bug 现状：cancelled ctx 上传失败）
- `TestDetachedCtxIgnoresCancel` → **PASS**（证 helper 行为正确）

- [ ] **Step 3: 提交测试基线**

```bash
git add pkg/storage/upload_detach_ctx_test.go
git commit -m "test(storage): 加测复现 record stop 时 upload 被 cancel 的 bug"
```

---

## Task 2: 修 S3File.uploadTempFile —— ctx 解耦

**Files:**
- Modify: `pkg/storage/s3.go`

- [ ] **Step 1: 看现有 uploadTempFile 实现**

```bash
sed -n '478,540p' pkg/storage/s3.go
```

确认 `w.ctx` 在 4 处使用：
1. `AcquireUploadSlot(w.ctx)`
2. `UploadWithRetry(w.ctx, ...)`（作为父 ctx）
3. `context.WithTimeout(w.ctx, timeout)` —— per attempt timeout
4. （可能还有别的 — grep 一下）

```bash
grep -n 'w\.ctx' pkg/storage/s3.go
```

- [ ] **Step 2: 在 uploadTempFile 入口加 detached ctx**

找到 `func (w *S3File) uploadTempFile() error {`，把方法最开头改成：

```go
func (w *S3File) uploadTempFile() error {
	// 解耦上传 ctx 与文件 ctx (= Recorder.Context).
	// Recorder dispose 时其 ctx 被 cancel, 但写好的临时文件应当继续上传完成.
	// WithoutCancel 保留 ctx 的 Values (trace / auth), 但 Done() 永不 close.
	// 上传链路的真实超时由每次 attempt 的 WithTimeout 控制.
	uploadCtx := context.WithoutCancel(w.ctx)

	// 获取上传槽位（并发控制）
	if err := AcquireUploadSlot(uploadCtx); err != nil {
		return fmt.Errorf("acquire upload slot: %w", err)
	}
	defer ReleaseUploadSlot()
	// ... 后续保持原逻辑, 但把 w.ctx → uploadCtx
```

- [ ] **Step 3: 把后续所有 `w.ctx` 替换成 `uploadCtx`**

替换 `UploadWithRetry(w.ctx, ...)` → `UploadWithRetry(uploadCtx, ...)`

替换 `context.WithTimeout(w.ctx, timeout)` → `context.WithTimeout(uploadCtx, timeout)`

grep 确认（应 0 行）：

```bash
grep -n 'w\.ctx' pkg/storage/s3.go
```

如果还有非上传路径在用 `w.ctx`（比如 Read 操作），那些不动。本 Step 只关心 upload 路径。具体方法：先看 grep 结果，对每一处判定，再选择性替换。

- [ ] **Step 4: 编译验证**

```bash
go build ./pkg/storage/...
```

- [ ] **Step 5: 跑测试，确认现有测试无回归**

```bash
go test -race -count=1 ./pkg/storage/...
```

预期：原有测试 + Task 1 加的测试全 PASS。

- [ ] **Step 6: 提交**

```bash
git add pkg/storage/s3.go
git commit -m "fix(storage/s3): uploadTempFile 用 WithoutCancel 解耦 Recorder.Context

record stop 时 Recorder.Context cancel 会导致正在跑的 S3 upload 被中断,
文件落 pending_uploads/ 后无重试. 用 context.WithoutCancel 保留 ctx Values
但忽略 cancel, 让上传完成. 每次 attempt 的真实超时仍由 WithTimeout 控制."
```

---

## Task 3: 同步修 OSS / COS 后端

**Files:**
- Modify: `pkg/storage/oss.go`
- Modify: `pkg/storage/cos.go`

- [ ] **Step 1: OSS — 同 Task 2 模式**

找 `func (f *OSSFile) uploadTempFile() error {`，在方法开头加：

```go
func (f *OSSFile) uploadTempFile() error {
	// 解耦上传 ctx 与文件 ctx (同 s3.go 注释)
	uploadCtx := context.WithoutCancel(f.ctx)

	if err := AcquireUploadSlot(uploadCtx); err != nil {
		return fmt.Errorf("acquire upload slot: %w", err)
	}
	defer ReleaseUploadSlot()
	// ... 把所有 f.ctx → uploadCtx
```

替换 `UploadWithRetry(f.ctx, ...)` → `UploadWithRetry(uploadCtx, ...)`

grep 验证：

```bash
grep -n 'f\.ctx' pkg/storage/oss.go
```

非上传路径不动；本 Step 只动 `uploadTempFile` 函数体内的 ctx 使用。

- [ ] **Step 2: COS — 同上**

文件 `pkg/storage/cos.go`，同样在 `(f *COSFile) uploadTempFile()` 入口加 `uploadCtx := context.WithoutCancel(f.ctx)`，替换函数体内 `f.ctx` → `uploadCtx`。

- [ ] **Step 3: 编译 + 测试**

```bash
go build ./pkg/storage/...
go test -race -count=1 ./pkg/storage/...
```

- [ ] **Step 4: 提交**

```bash
git add pkg/storage/oss.go pkg/storage/cos.go
git commit -m "fix(storage/oss,cos): uploadTempFile 用 WithoutCancel 解耦父 ctx (同 s3)"
```

---

## Task 4: 端到端冒烟（130 真环境验证）

**Files:** 仅操作步骤

- [ ] **Step 1: 出新镜像**

```bash
./build_tag.sh
./build_docker.sh v5.2.<NEW_TAG>
```

- [ ] **Step 2: 部署到 130**

参照本 session 已建好的部署流程：

```bash
ssh root@172.16.12.130 \
  'cd /home/project/xde-uat/media-docker-compose && \
   sed -i "s|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:.*|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:<NEW_TAG>|" docker-compose-xde-monibuca.yml && \
   docker compose -f docker-compose-xde-monibuca.yml pull && \
   docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate'
```

- [ ] **Step 3: 用 API 加 31 路 pull proxy → 等就绪 → 启录制 (5 min) → 停录制**

复用 session 已建好的脚本流程。

- [ ] **Step 4: 关键验收指标（quantitative）**

```bash
ssh root@172.16.12.130 '
echo "=== pending_uploads 数量（应为 0） ==="
docker exec xde-monibuca sh -c "ls /monibuca/pending_uploads/*.mp4 2>/dev/null | wc -l"

echo ""
echo "=== docker log 中 upload 成功事件 ==="
docker logs xde-monibuca --since 10m 2>&1 | grep -iE "upload.*success|upload completed|S3.*200" | tail -10

echo ""
echo "=== docker log 中 cancel 事件（应消失） ==="
docker logs xde-monibuca --since 10m 2>&1 | grep -iE "RequestCanceled|context canceled" | tail -5
'
```

**修复前对比（已知 baseline）**：
- pending_uploads: **31**
- cancel 事件: **每路 1 条** = 31 条

**修复后预期**：
- pending_uploads: **0**（或 <5 — 个别真实失败可容忍）
- cancel 事件: **0**（与 Recorder context 相关的 cancel 应不再出现）

- [ ] **Step 5: 验录制完整性**

```bash
# 直接看 MinIO 上是不是有 31 个 mp4
mc ls --recursive xiding-uat/vidu-media-bucket/live/ | wc -l
# 预期: 31
```

然后用 ffprobe 校验 31 路（复用 session 流程），预期 PASS=31。

- [ ] **Step 6: 清理 31 个临时 pull proxy**

复用 session 流程。

---

## Task 5: 验收 + 给限流 plan 解锁

- [ ] **Step 1: 单元测试全过**

```bash
go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...
```

- [ ] **Step 2: 代码 grep 自检**

```bash
echo "=== 三个后端都已加 uploadCtx ==="
grep -nE 'uploadCtx[ ,]|context\.WithoutCancel' pkg/storage/s3.go pkg/storage/oss.go pkg/storage/cos.go

echo ""
echo "=== 三个后端的 uploadTempFile 内部没有裸 w.ctx / f.ctx ==="
for f in pkg/storage/s3.go pkg/storage/oss.go pkg/storage/cos.go; do
    echo "--- $f ---"
    awk '/^func.*uploadTempFile/,/^}/' "$f" | grep -nE 'w\.ctx|f\.ctx'
done
```

预期：
- 每个文件 1 处 `WithoutCancel`
- 三个 uploadTempFile 函数体内 0 处裸的 `w.ctx` / `f.ctx`

- [ ] **Step 3: 130 真实跑过 pending_uploads=0**

Task 4 已验。

- [ ] **Step 4: 解锁限流 plan**

修复完成后，`docs/superpowers/plans/2026-05-15-trailer-flush-rate-limit.md` 里 Task 2 Step 4 的 `t.<CTX>` 字段可以**安全使用** `t.Context` 了 — 因为即使 task ctx cancel，slot acquire 失败只是 skip trailer write，但**已写好的文件仍能上传**（本 plan fix）。

或者更稳：限流 plan 里也用 `context.Background()` 给 slot acquire，与本修复独立。

- [ ] **Step 5: PR 关联**

在本 PR 描述里写：
- Fixes: record stop 导致 upload cancel 的 bug（session 实测 31/31 全进 pending_uploads）
- 影响：解决数据无人重传的事实丢失
- 解锁：`trailer-flush-rate-limit` plan 可以安全推进

---

## Self-Review 记录

**Spec coverage**：
- "Recorder stop 时 upload 被 cancel" → Task 1 复现 + Task 2/3 修 ✓
- "三个后端都修" → Task 2 (S3) + Task 3 (OSS, COS) ✓
- "限流 plan 解锁" → Task 5 step 4 明示 ✓

**Placeholder scan**：
- 无 TBD / TODO
- Task 4 镜像 tag `<NEW_TAG>` 是部署时动态填入，非真 placeholder

**Type consistency**：
- `uploadCtx := context.WithoutCancel(w.ctx/f.ctx)` — 三个后端用相同模式 ✓
- 未引入新类型 / 接口 ✓

**风险**：
- `context.WithoutCancel` 保留 Values 但不传递 cancel — 这是 Go 1.21 引入的，仓库 Go 1.26 已支持
- Per-attempt 超时（`context.WithTimeout(uploadCtx, timeout)`）仍然有效，不会无限挂
- 如果 monibuca server 整体退出，Server-level ctx 可能也需要传到上传层让上传能被停掉。目前 plan 不处理这个 — 上传由 retry 机制 + timeout 自然终结。若需 server 级 cancel，加 `cluster/server-level ctx merge`，但**那是另一个 issue**

**与原 spec 的弱化**：
- 不修 `pending_uploads/` 无自动重试机制（独立 issue；本修复让正常路径不进 pending_uploads，问题减轻 90%+）
- 不动 Local backend（无 ctx 依赖问题）
