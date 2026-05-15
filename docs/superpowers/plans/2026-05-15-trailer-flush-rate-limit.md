# MP4 Trailer Flush 限流 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在录制插件的 trailer flush 流程加并发槽位限制，**把 record stop 时磁盘写带宽峰值控制在 300 MB/s 以内**（实测无限流时峰值 1.1 GB/s ≈ SSD 顺序写上限）。

**目标值推导：** 31 路并发产生 1126 MB/s → 期望 300 MB/s → 槽位数 = 31 × 300/1126 ≈ **8**。默认 `MaxConcurrentTrailerWrites=8`，按硬件可调（NVMe Gen4 放宽 16-32，HDD 收紧 2-4）。

**Architecture:** 在 `pkg/storage` 加一个 `trailerSem` 信号量，配置字段加在 `UploadConfig` 里（与 `MaxConcurrentUploads` 并列），通过 `InitUploadManager` 一次性初始化。**配置归 storage**，因为 trailer 是磁盘 IO 共享关切：mp4/flv/任何录制 plugin 写盘都过 storage 层，配在 mp4 plugin 会造成 flv 也得复制一份。`writeTrailerTask.Start()` 入口 acquire 槽位，`Dispose()` 出口 release。不动 task framework。

**Tech Stack:** Go channel-based semaphore，沿用 `pkg/storage/upload_manager.go` 既有 `uploadSem` 模式。

**问题背景：**
- 实测 31 路 5/10/15 min 录制都正常完成，但 `record stop` 那 1-2 秒磁盘写带宽飙到 1126 MB/s ≈ SSD 物理上限 1.1 GB/s
- 原因：`plugin/mp4/pkg/record.go` 的 `writeTrailerQueueTask.AddTask(t)` 把 31 个 trailer task 并发拉起，每个都做 moov 重排 + bufio flush；当用户调 stop API 卸 31 路时所有 31 个 trailer 同时跑
- 上传层已经有 4-slot 限制（`AcquireUploadSlot`），但 trailer 写盘**没有限制** — 是真正的瓶颈

**修复范围：** 仅加 trailer 槽位 + 给 `writeTrailerTask` 上手铐。
- **不修** record stop 时的 context cancel bug（独立 issue，本 plan Task 4 验证会看到文件仍进 pending_uploads，需 mc cp 救回）
- **不动 flv plugin**：本 plan 只覆盖 mp4 plugin。flv 若有同样问题，可参照 Task 2 模式接同一个 `AcquireTrailerSlot`

**Spec：** 见本文件首段 + Task 5 验收节。无独立 spec 文件。

---

## 文件结构

```
pkg/storage/
├── upload_manager.go          [改] UploadConfig 加 MaxConcurrentTrailerWrites 字段
│                                  加 trailerSem 全局变量 + InitUploadManager 初始化
│                                  加 AcquireTrailerSlot / ReleaseTrailerSlot / Getter
└── upload_manager_test.go     [新] 4 个信号量行为单测

plugin/mp4/pkg/
└── record.go                  [改] writeTrailerTask 加 slotAcquired 字段
                                   Start() 入口 acquire / Dispose() 出口 release

example/<some-config>.yaml     [改] mc 示例添加 maxconcurrenttrailerwrites 注释

CLAUDE.md                       [改] 加该配置项说明（若 CLAUDE.md 已有 storage 配置节）

docs/superpowers/plans/
└── 2026-05-15-trailer-flush-rate-limit.md  [新] 本文件
```

---

## Task 1: pkg/storage 加 trailerSem + UploadConfig 字段

**Files:**
- Modify: `pkg/storage/upload_manager.go`
- Create: `pkg/storage/upload_manager_test.go`

- [ ] **Step 1: 写失败测试**

文件 `pkg/storage/upload_manager_test.go`：

```go
package storage

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 重新初始化以避免测试间状态污染（map shared 全局变量）
func initForTest(t *testing.T, maxTrailer int) {
	t.Helper()
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: maxTrailer,
		PendingDir:                 t.TempDir(),
	})
}

func TestTrailerSlotConcurrencyLimit(t *testing.T) {
	initForTest(t, 2)

	var active int32
	var maxObserved int32
	var wg sync.WaitGroup
	hold := 100 * time.Millisecond

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := AcquireTrailerSlot(context.Background()); err != nil {
				t.Errorf("AcquireTrailerSlot: %v", err)
				return
			}
			defer ReleaseTrailerSlot()
			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxObserved)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxObserved, prev, cur) {
					break
				}
			}
			time.Sleep(hold)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()

	if maxObserved > 2 {
		t.Fatalf("max concurrent trailers should be ≤ 2, got %d", maxObserved)
	}
	if got := GetActiveTrailerWrites(); got != 0 {
		t.Fatalf("active should be 0 after all done, got %d", got)
	}
	if got := GetMaxConcurrentTrailerWrites(); got != 2 {
		t.Fatalf("GetMaxConcurrentTrailerWrites should be 2, got %d", got)
	}
}

func TestTrailerSlotContextCancel(t *testing.T) {
	initForTest(t, 1)
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	defer ReleaseTrailerSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := AcquireTrailerSlot(ctx); err == nil {
		t.Fatal("second acquire should fail when ctx cancels")
	}
}

func TestTrailerSlotDefaultsTo8(t *testing.T) {
	// MaxConcurrentTrailerWrites 留 0 时, 应取 default tag 值 8
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads: 4,
		PendingDir:           t.TempDir(),
	})
	if got := GetMaxConcurrentTrailerWrites(); got != 8 {
		t.Fatalf("zero/unset should default to 8, got %d", got)
	}
}

func TestTrailerSlotIndependentFromUploadSem(t *testing.T) {
	// 验 trailer sem 不影响 upload sem (各占 4 / 2 槽)
	InitUploadManager(UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: 2,
		PendingDir:                 t.TempDir(),
	})
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer ReleaseTrailerSlot()
	// upload 应仍可正常 acquire (sem 独立)
	if err := AcquireUploadSlot(context.Background()); err != nil {
		t.Fatal("upload sem should be independent:", err)
	}
	ReleaseUploadSlot()
	if got := GetActiveTrailerWrites(); got != 1 {
		t.Fatalf("trailer active should be 1, got %d", got)
	}
}
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
go test -run 'TestTrailerSlot' ./pkg/storage/...
```

预期：`undefined: AcquireTrailerSlot` / `unknown field MaxConcurrentTrailerWrites in struct literal of type UploadConfig`。

- [ ] **Step 3: 在 `pkg/storage/upload_manager.go` 加实现**

3a. `import` 区无需变动（`context`、`log`、`sync/atomic` 都已存在）。

3b. 文件顶部 `var (...)` 块，加在现有 `uploadSem` 之后：

```go
	// trailerSem 限制并发 trailer 写盘槽位数，避免多路 record stop 时同时
	// moov-rewrite + bufio flush 把磁盘吃满。与 uploadSem 解耦：
	//   - trailerSem 控磁盘 IO（mp4/flv 等录制 plugin 共用）
	//   - uploadSem 控网络上传（s3/cos/oss 后端）
	trailerSem            chan struct{}
	activeTrailerWrites   int32
	maxConcurrentTrailers int
```

3c. `UploadConfig` 加一个字段：

```go
type UploadConfig struct {
	MaxConcurrentUploads       int    `desc:"最大并发上传数" default:"4"`
	MaxConcurrentTrailerWrites int    `desc:"最大并发 trailer 写盘槽位数, 控制 record stop 时磁盘 IO burst (目标 ~300 MB/s SSD)" default:"8"`
	PendingDir                 string `desc:"上传失败文件暂存目录" default:"pending_uploads"`
}
```

3d. `InitUploadManager` 函数体里，紧跟 `uploadSem` 初始化之后加：

```go
	if cfg.MaxConcurrentTrailerWrites <= 0 {
		cfg.MaxConcurrentTrailerWrites = 8
	}
	maxConcurrentTrailers = cfg.MaxConcurrentTrailerWrites
	trailerSem = make(chan struct{}, cfg.MaxConcurrentTrailerWrites)
```

并把 `log.Printf` 行改成：

```go
	log.Printf("[storage] upload manager initialized: maxConcurrent=%d, maxTrailer=%d, pendingDir=%s",
		maxConcurrent, maxConcurrentTrailers, pendingDir)
```

3e. 文件末尾（`GetPendingDir` 之后）加：

```go
// AcquireTrailerSlot 获取一个 trailer 写盘槽位，阻塞直到有可用槽位或 ctx 取消。
// 配对 ReleaseTrailerSlot；调用方通常在 record stop 流程进入 trailer flush 前 acquire,
// 在 task Dispose / Run 末尾 defer Release。
// trailerSem 未初始化时为 no-op，立即返回 nil（保证测试 / 早期初始化场景安全）。
func AcquireTrailerSlot(ctx context.Context) error {
	if trailerSem == nil {
		return nil
	}
	select {
	case trailerSem <- struct{}{}:
		atomic.AddInt32(&activeTrailerWrites, 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseTrailerSlot 释放一个 trailer 写盘槽位。
// 未初始化时 no-op，保证 defer 安全。
func ReleaseTrailerSlot() {
	if trailerSem == nil {
		return
	}
	<-trailerSem
	atomic.AddInt32(&activeTrailerWrites, -1)
}

// GetActiveTrailerWrites 当前活跃 trailer 写盘数
func GetActiveTrailerWrites() int32 {
	return atomic.LoadInt32(&activeTrailerWrites)
}

// GetMaxConcurrentTrailerWrites 当前 trailer slot 上限
func GetMaxConcurrentTrailerWrites() int {
	return maxConcurrentTrailers
}
```

- [ ] **Step 4: 跑测试，确认 4 个 case 全 PASS**

```bash
go test -run 'TestTrailerSlot' -race ./pkg/storage/...
```

预期：`PASS: TestTrailerSlotConcurrencyLimit / TestTrailerSlotContextCancel / TestTrailerSlotDefaultsTo8 / TestTrailerSlotIndependentFromUploadSem`。

- [ ] **Step 5: 跑现有 storage 测试，确认无回归**

```bash
go test ./pkg/storage/... -count=1
```

预期：原有测试 + 新加测试全 PASS。

- [ ] **Step 6: 提交**

```bash
git add pkg/storage/upload_manager.go pkg/storage/upload_manager_test.go
git commit -m "feat(storage): 加 trailerSem 限制 record stop 时 trailer 并发写盘"
```

---

## Task 2: writeTrailerTask 用 trailer slot

**Files:**
- Modify: `plugin/mp4/pkg/record.go`
- Create: `plugin/mp4/pkg/record_trailer_slot_test.go`

- [ ] **Step 1: 先 verify task framework 的 context 字段名**

monibuca 的 `task.Task` 嵌入字段中 context 的访问方式不确定（`t.Context` / `t.Ctx` / `t.GetContext()`）。验：

```bash
GOTASK_PATH=$(go list -m -f '{{.Dir}}' github.com/langhuihui/gotask)
grep -nE 'Context\s|Ctx\s|func.*Context\(\)' "$GOTASK_PATH/task.go" | head -10
```

或者更稳：从 `plugin/mp4/pkg/record.go` 既有代码里找 — 任何已经访问 ctx 的地方就是答案：

```bash
grep -n 'task.Context\|t.Context\|task.Ctx\|t.Ctx\|GetContext' plugin/mp4/pkg/record.go | head -5
```

后续 Step 3 代码里的 `t.Context` 用实际字段名替换。**Step 1 不验过不要往下走**。

- [ ] **Step 2: 看现有 writeTrailerTask 结构**

```bash
sed -n '25,55p' plugin/mp4/pkg/record.go
```

预期看到 struct 定义、接收器名 `task`（即 `func (task *writeTrailerTask) Start(...)`，与 import 包 `task` 同名遮蔽）。

- [ ] **Step 3: 修改 struct 加 slotAcquired，改接收器为 `t`**

替换原 struct 定义：

```go
type writeTrailerTask struct {
	task.Task
	muxer      *Muxer
	file       storage.File
	filePath   string
	durationMs uint32   // 录像时长（毫秒），用于上传 S3 元数据
	streamPath string   // 关联流路径（用于失败追踪）
	storageKey string   // 存储类型 key（s3/oss/cos/local）
	db         *gorm.DB // 数据库连接（用于保存失败记录）
	dbWrite    func(tailJob task.IJob)

	// slotAcquired 标志当前任务是否持有 trailer slot，用于 Dispose 时安全释放
	slotAcquired bool
}
```

- [ ] **Step 4: 改 Start() 入口加 acquire，改接收器**

把现有 `func (task *writeTrailerTask) Start() (err error) {` 整方法替换成（**注意：Step 1 验过的真实 context 字段名替换 `<CTX>`**）：

```go
func (t *writeTrailerTask) Start() (err error) {
	// 阻塞获取 trailer slot, 限多路并发 stop 时磁盘 IO burst
	if err = storage.AcquireTrailerSlot(t.<CTX>); err != nil {
		t.Warn("acquire trailer slot canceled", "err", err)
		return
	}
	t.slotAcquired = true
	t.Info("write trailer start",
		"active", storage.GetActiveTrailerWrites(),
		"max", storage.GetMaxConcurrentTrailerWrites())
	if err = t.muxer.WriteTrailer(t.file); err != nil {
		t.Error("write trailer", "err", err)
		if t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}
	return
}
```

- [ ] **Step 5: 把 `Run()` 里所有 `task.xxx` 引用改成 `t.xxx`**

原代码 `func (t *writeTrailerTask) Run() (err error)` 接收器已经叫 `t`（行 65），里面用的是 `t.muxer / t.file / t.Info` 等。**对比 Run 与 Start 现状：Run 用 `t`，Start 用 `task`**。Step 4 已统一接收器名为 `t`，所以 Run 那部分不用动。

但 grep 验一下不一致：

```bash
grep -nE 'func \(task \*writeTrailerTask\)' plugin/mp4/pkg/record.go
```

预期：0 行（Step 4 已经替换）。如果还有别的方法接收器叫 `task`（例如 `Dispose`、`OnStart` 等），同步改名。

- [ ] **Step 6: 加 Dispose() 释放槽位**

在 writeTrailerTask 现有方法之后追加：

```go
// Dispose 在任务结束（成功/失败/取消）时被 framework 调用. 统一释放 trailer 槽位.
// slotAcquired guard 防止重复释放 (framework 是否多次调 Dispose 取决于版本).
func (t *writeTrailerTask) Dispose() {
	if t.slotAcquired {
		storage.ReleaseTrailerSlot()
		t.slotAcquired = false
	}
}
```

- [ ] **Step 7: 确认 import 含 `m7s.live/v5/pkg/storage`**

```bash
head -25 plugin/mp4/pkg/record.go | grep '"m7s.live/v5/pkg/storage"'
```

预期已存在（既有代码用了 `storage.File`）。若缺则补。

- [ ] **Step 8: 编译验证**

```bash
go build ./plugin/mp4/...
```

预期：通过。

- [ ] **Step 9: 写并发 sanity 单测**

文件 `plugin/mp4/pkg/record_trailer_slot_test.go`（**注意包名 `package mp4`**，不是 `package pkg`）：

```go
package mp4

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

func TestRecordTrailerSlotSerialization(t *testing.T) {
	storage.InitUploadManager(storage.UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: 2,
		PendingDir:                 t.TempDir(),
	})

	var active int32
	var maxObserved int32
	var wg sync.WaitGroup

	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := storage.AcquireTrailerSlot(context.Background()); err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			defer storage.ReleaseTrailerSlot()
			cur := atomic.AddInt32(&active, 1)
			for {
				prev := atomic.LoadInt32(&maxObserved)
				if cur <= prev || atomic.CompareAndSwapInt32(&maxObserved, prev, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()
	if maxObserved > 2 {
		t.Fatalf("max active should be ≤ 2, got %d", maxObserved)
	}
}
```

- [ ] **Step 10: 跑测试**

```bash
go test -race -run 'TestRecordTrailerSlot' ./plugin/mp4/pkg/...
```

预期：PASS。

- [ ] **Step 11: 跑 mp4 全测试无回归**

```bash
go test ./plugin/mp4/... -count=1
```

预期：原有 + 新加全 PASS。

- [ ] **Step 12: 提交**

```bash
git add plugin/mp4/pkg/record.go plugin/mp4/pkg/record_trailer_slot_test.go
git commit -m "feat(mp4): writeTrailerTask 用 trailer 槽位限并发 (Start acquire / Dispose release)"
```

---

## Task 3: 配置示例文档

**Files:**
- Modify: 1 个有 `global.storage` 段的示例 yaml
- Modify: `CLAUDE.md`（若已有 storage 配置节）

- [ ] **Step 1: 找带 `storage:` 块的示例配置**

```bash
grep -l '^  storage:' example/**/*.yaml 2>/dev/null
# 已知 example/record-test/config.yaml + example/cluster/config{1,2,3}.yaml 有
```

- [ ] **Step 2: 在 `example/record-test/config.yaml` 的 `global.storage:` 块下加注释（不动其它示例）**

找到 `storage:` 这一行，在 `s3:` 子段**之前**插入：

```yaml
  storage:
    # === 并发限流（默认值即可，按硬件可调） ===
    # maxconcurrentuploads: 4         # 并发上传 S3/COS/OSS 槽位 (默认 4)
    # maxconcurrenttrailerwrites: 8   # 并发 trailer 写盘槽位 (默认 8, 目标 ~300 MB/s SSD)
                                       # 多路 record stop 时所有录制插件 (mp4/flv) 共用该槽位
                                       # NVMe Gen4 可调 16-32, HDD 设 2-4. 0/负 = 不限流
    s3:
      region: "us-east-1"
      # ... 原内容
```

- [ ] **Step 3: 看 CLAUDE.md 是否有 storage 配置节**

```bash
grep -n "MaxConcurrentUploads\|storage 配置\|存储配置\|pkg/storage" CLAUDE.md | head -5
```

- [ ] **Step 4: 若有, 在 `MaxConcurrentUploads` 附近补一行**

```markdown
- `MaxConcurrentTrailerWrites`：限制 record stop 时 trailer flush 并发写盘数（默认 8）。多路 stop 时所有录制 plugin (mp4/flv) 共用该槽位，目标控制 SSD 磁盘 burst ~300 MB/s。换硬件按 `(目标带宽 MB/s) × 实测并发数 / 实测峰值带宽` 反推；本仓 SSD 实测 31 并发 → 1126 MB/s，故 8 ≈ 290 MB/s。
```

如 CLAUDE.md 没这一节，跳过 Step 4。

- [ ] **Step 5: 提交**

```bash
git add example/record-test/config.yaml CLAUDE.md
git commit -m "docs(storage): MaxConcurrentTrailerWrites 配置项说明"
```

---

## Task 4: 端到端冒烟（手动跑）

**Files:** 仅操作步骤，无代码改动

- [ ] **Step 1: 出新镜像**

```bash
./build_tag.sh             # 假设产生 v5.2.2.YYMMDDHHMM
./build_docker.sh v5.2.2.<TAG>
```

或在测试机直接 `go build -tags sqlite,s3` 出二进制。

- [ ] **Step 2: 部署到 130 测试环境**

```bash
ssh root@172.16.12.130 \
  'cd /home/project/xde-uat/media-docker-compose && \
   sed -i "s|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:.*|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:<NEW_TAG>|" docker-compose-xde-monibuca.yml && \
   docker login -u <SWR_USER> -p <SWR_PASS> swr.cn-east-3.myhuaweicloud.com && \
   docker compose -f docker-compose-xde-monibuca.yml pull && \
   docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate'
```

Check 日志含 `maxTrailer=8`：

```bash
ssh root@172.16.12.130 'docker logs xde-monibuca 2>&1 | grep maxTrailer'
```

- [ ] **Step 3: 跑 31 路 5min 录制对照（复用 session 流程）**

API 加 31 路 pull proxy → 等就绪 → start 录制 (duration=300) → wait → 停。

- [ ] **Step 4: 监控磁盘 burst**

启动 1/min 监控 (复用 session 现成脚本)，重点看 `disk_w_kbps` 在 stop 时的峰值。

**验收标准（量化）**：
- 修复前实测：peak `disk_w_kbps = 1126144` (1126 MB/s × 1 sec)
- 修复后预期：peak `disk_w_kbps < 320 MB/s` (~290 MB/s 持续 ~4 sec)
- 总写入量不变（与录制 + trailer rewrite 数据量同）

- [ ] **Step 5: 验录制完整性**

注意：**stop cancel context bug 没修，文件仍会进 pending_uploads**。需手动 mc cp。

```bash
ssh root@172.16.12.130 '
  docker exec xde-monibuca ls /monibuca/pending_uploads/*.mp4 | wc -l
  # 预期：31
'
# 然后 mc mirror 救回上传（复用 session 流程）
```

ffprobe 31 路：预期 PASS=31，时长 300±5 sec。**slot 限流不影响最终文件内容，仅影响写盘节奏**。

---

## Task 5: 验收

- [ ] **Step 1: 单测覆盖**

```bash
go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...
```

期望全 PASS。

- [ ] **Step 2: 函数 / 字段 grep 自检**

```bash
echo "=== pkg/storage 定义 ==="
grep -n 'AcquireTrailerSlot\|ReleaseTrailerSlot\|GetActiveTrailerWrites\|GetMaxConcurrentTrailerWrites\|MaxConcurrentTrailerWrites' pkg/storage/upload_manager.go

echo "=== mp4 plugin 使用 ==="
grep -n 'AcquireTrailerSlot\|ReleaseTrailerSlot' plugin/mp4/pkg/record.go
```

预期：
- `upload_manager.go`：5 处（4 函数 + 1 config 字段）
- `record.go`：2 处（Start acquire + Dispose release）

- [ ] **Step 3: 默认值核查**

```bash
grep -A1 'MaxConcurrentTrailerWrites' pkg/storage/upload_manager.go
```

期望看到 `default:"8"` tag。

- [ ] **Step 4: 升级后磁盘 burst 实测对比**

需要 Task 4 真实跑过一次，监控 csv 里 `disk_w_kbps` peak < 320 MB/s。

- [ ] **Step 5: 上线前补充 issue 跟踪**

把 "record stop 时 cancel S3 upload context" bug 单开 issue（本 plan 不修），关联本 PR。

把 flv plugin 是否需要同样限流单开评估 issue（mp4 内部 muxer 与 flv 写盘路径不同，需 verify）。

---

## Self-Review 记录

**Spec coverage**:
- "spread trailer flush over 几秒" → Task 1 加信号量；Task 2 应用到 writeTrailerTask；slot=8 时 31 路 trailer 会按 4 批跑（31/8 ≈ 4 批），每批 ~1 秒（盘饱和），共 ~4 秒 spread ✓
- "控制在 300MB/s" → 信号量 8 × 单 trailer 写盘 ~36 MB/s ≈ 290 MB/s ✓（仍小于 320 MB/s 阈值）
- "配置归 storage" → `UploadConfig.MaxConcurrentTrailerWrites` 在 pkg/storage，mp4/flv 都通过同一槽位 ✓

**Placeholder scan**：
- Task 2 Step 4 有 `<CTX>` 占位 — 这是**有意保留**给 executor 验证后填，已在 Step 1 写明验证命令。非真 placeholder（无法静态填）。
- 其它任务步骤均有完整代码块。

**Type consistency**：
- `AcquireTrailerSlot(ctx context.Context) error` / `ReleaseTrailerSlot()` / `GetActiveTrailerWrites() int32` / `GetMaxConcurrentTrailerWrites() int` — 全 plan 一致
- `MaxConcurrentTrailerWrites int` 配置字段 — Task 1 Step 3c 定义，Task 1 测试、Task 2 测试、Task 3 yaml 引用一致
- `slotAcquired bool` struct 字段 — Task 2 Step 3 定义，Step 4 (Start) / Step 6 (Dispose) 引用一致
- Test 包名：`pkg/storage/upload_manager_test.go` → `package storage`，`plugin/mp4/pkg/record_trailer_slot_test.go` → `package mp4`（与 record.go 同包）

**与原 spec 的弱化**：
- 不修 stop context cancel bug（明确 out of scope，Task 4 / Task 5 step 5 提醒）
- 不动 flv plugin（同 issue 留 Task 5 step 5 跟踪）
- 用固定信号量而非真令牌桶（更简单，效果等价；token bucket 需引入 `golang.org/x/time/rate` 依赖）

**Race condition / 并发安全**：
- `trailerSem` 是 channel，本身线程安全
- `activeTrailerWrites` 用 `atomic.Int32`
- `maxConcurrentTrailers` 仅在 `InitUploadManager` 中赋值，运行时只读 → 不需锁
- `SetTrailerSlotLimit` 已**移除**（避免运行时重置 sem 时已等待 goroutine 永久挂起的风险），改回 init 一次性配置
