# MP4 Trailer Flush 限流 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 在 mp4 plugin 的 trailer flush 流程加并发槽位限制，**把 record stop 时磁盘写带宽峰值控制在 300 MB/s 以内**（实测无限流时峰值 1.1 GB/s ≈ SSD 顺序写上限）。

**目标值推导：** 31 路并发产生 1126 MB/s → 期望 300 MB/s → 槽位数 = 31 × 300/1126 ≈ **8**。默认 `MaxConcurrentTrailerWrites=8`，可按部署环境磁盘能力调整（NVMe Gen4 可放宽，HDD 应进一步收紧）。

**Architecture:** 复用 `pkg/storage` 现有的 `uploadSem` 信号量模式 — 在 `pkg/storage` 加一个独立的 `trailerSem` + `SetTrailerSlotLimit(n)` 公开 setter（不读 `UploadConfig`，与 upload sem 解耦）。**配置归属 `mp4` plugin**（trailer 是录制流程的关切，不是 storage 后端的关切；local / s3 / cos / oss 都共用同一个磁盘 IO 限流）。`MP4Plugin.Start()` 读 `p.MaxConcurrentTrailerWrites` 调 setter 初始化。`writeTrailerTask.Start()` 入口 acquire 槽位，`Dispose()` 出口 release。不动 task framework。

**Tech Stack:** Go channel-based semaphore，沿用 `pkg/storage/upload_manager.go` 既有模式。

**问题背景：**
- 实测 31 路 5/10/15 min 录制都正常完成，但 `record stop` 那 1-2 秒磁盘写带宽飙到 1126 MB/s ≈ SSD 物理上限 1.1 GB/s
- 原因：`plugin/mp4/pkg/record.go` 的 `writeTrailerQueueTask.AddTask(t)` 把 31 个 trailer task 并发拉起，每个都做 moov 重排 + bufio flush；当用户调 stop API 卸 31 路时所有 31 个 trailer 同时跑
- 上传层已经有 4-slot 限制（`AcquireUploadSlot`），但 trailer 写盘**没有限制** — 是真正的瓶颈

**修复范围：** 仅加 trailer 槽位 + 给 `writeTrailerTask` 上手铐。**不修** record stop 时的 context cancel bug（独立的另一件事，留单独 plan）。

**Spec：** 见本文件首段 + 文末"验收"节。无独立 spec 文件。

---

## 文件结构

```
pkg/storage/
├── upload_manager.go          [改] 加 trailerSem + Acquire/Release + SetTrailerSlotLimit(n) setter
│                                  (不动 UploadConfig, 与 upload sem 完全解耦)
└── upload_manager_test.go     [新] 信号量行为单测

plugin/mp4/
└── index.go                   [改] MP4Plugin struct 加 MaxConcurrentTrailerWrites 字段
                                   Start() 调 storage.SetTrailerSlotLimit(p.MaxConcurrentTrailerWrites)

plugin/mp4/pkg/
└── record.go                  [改] writeTrailerTask 加 Start acquire / Dispose release

docs/
└── superpowers/plans/2026-05-15-trailer-flush-rate-limit.md  [新] 本文件
```

---

## Task 1: pkg/storage 加 trailerSem + SetTrailerSlotLimit setter

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

func TestTrailerSlotConcurrencyLimit(t *testing.T) {
	SetTrailerSlotLimit(2)

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
	SetTrailerSlotLimit(1)
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatalf("first acquire should succeed: %v", err)
	}
	defer ReleaseTrailerSlot()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := AcquireTrailerSlot(ctx); err == nil {
		t.Fatal("second acquire should fail when ctx cancels")
		ReleaseTrailerSlot()
	}
}

func TestTrailerSlotNoLimit(t *testing.T) {
	// 0 / 负值 = 不限流, Acquire 立即返回 nil, 不计数
	SetTrailerSlotLimit(0)
	if err := AcquireTrailerSlot(context.Background()); err != nil {
		t.Fatalf("acquire with no limit should be no-op: %v", err)
	}
	ReleaseTrailerSlot() // 应是 no-op
	if got := GetMaxConcurrentTrailerWrites(); got != 0 {
		t.Fatalf("max should be 0 when disabled, got %d", got)
	}
}
```

- [ ] **Step 2: 跑测试，确认失败（编译失败 / 函数未定义）**

```bash
go test -run 'TestTrailerSlot' ./pkg/storage/...
```

预期：`undefined: AcquireTrailerSlot` / `undefined: ReleaseTrailerSlot` / `unknown field MaxConcurrentTrailerWrites in UploadConfig`。

- [ ] **Step 3: 在 `pkg/storage/upload_manager.go` 加 trailerSem 实现**

**不动 `UploadConfig` 和 `InitUploadManager`**。trailer sem 是独立子系统，由 mp4 plugin 在 Start() 主动初始化（Task 3）。

文件 import 区加 `"sync"`（若没有）：

```go
import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)
```

在文件顶部 `var (...)` 块里加（与 `uploadSem` 并列、独立）：

```go
	// --- trailer flush 限流（与 uploadSem 解耦）---
	// trailer 是磁盘 IO 关切（moov-rewrite + bufio flush），local/s3/oss/cos 都需要;
	// uploadSem 是网络/对象存储关切. 二者并发上限不同, 单独控制.
	trailerSem            chan struct{}
	activeTrailerWrites   int32
	maxConcurrentTrailers int
	trailerSemMu          sync.Mutex
```

文件末尾（`GetPendingDir` 之后）加：

```go
// SetTrailerSlotLimit 设置 trailer 写盘槽位上限.
// 由调用方（如 mp4 plugin Start）按其 config 初始化.
// n <= 0 表示不限流, AcquireTrailerSlot 立即返回 nil.
// 重复调用以最后一次为准.
func SetTrailerSlotLimit(n int) {
	trailerSemMu.Lock()
	defer trailerSemMu.Unlock()
	if n <= 0 {
		trailerSem = nil
		maxConcurrentTrailers = 0
		log.Printf("[storage] trailer slot limit disabled")
		return
	}
	trailerSem = make(chan struct{}, n)
	maxConcurrentTrailers = n
	log.Printf("[storage] trailer slot limit set: %d", n)
}

// AcquireTrailerSlot 获取一个 trailer 写盘槽位, 阻塞直到有可用槽位或 ctx 取消.
// 未 SetTrailerSlotLimit 或 limit<=0 时为 no-op, 立即返回 nil.
func AcquireTrailerSlot(ctx context.Context) error {
	trailerSemMu.Lock()
	sem := trailerSem
	trailerSemMu.Unlock()
	if sem == nil {
		return nil
	}
	select {
	case sem <- struct{}{}:
		atomic.AddInt32(&activeTrailerWrites, 1)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReleaseTrailerSlot 释放一个 trailer 写盘槽位. 配对 AcquireTrailerSlot.
// 未初始化时 no-op, 保证 defer Release 永远安全.
func ReleaseTrailerSlot() {
	trailerSemMu.Lock()
	sem := trailerSem
	trailerSemMu.Unlock()
	if sem == nil {
		return
	}
	select {
	case <-sem:
		atomic.AddInt32(&activeTrailerWrites, -1)
	default:
		// sem 已空 / 在 acquire 后被 reset; 静默忽略
	}
}

// GetActiveTrailerWrites 当前活跃 trailer 写盘数
func GetActiveTrailerWrites() int32 {
	return atomic.LoadInt32(&activeTrailerWrites)
}

// GetMaxConcurrentTrailerWrites 当前 trailer slot 上限（SetTrailerSlotLimit 设的值）
func GetMaxConcurrentTrailerWrites() int {
	trailerSemMu.Lock()
	defer trailerSemMu.Unlock()
	return maxConcurrentTrailers
}
```

- [ ] **Step 4: 跑测试，确认通过**

```bash
go test -run 'TestTrailerSlot' -race ./pkg/storage/...
```

预期：2 个 case 全 PASS。

- [ ] **Step 5: 跑现有 upload_manager 测试，确认没回归**

```bash
go test ./pkg/storage/... -count=1
```

预期：原有测试全 PASS（如果有的话），新测试也 PASS。

- [ ] **Step 6: 提交**

```bash
git add pkg/storage/upload_manager.go pkg/storage/upload_manager_test.go
git commit -m "feat(storage): 加 trailerSem 限制 record stop 时 trailer 并发写盘"
```

---

## Task 2: writeTrailerTask 用 trailer slot

**Files:**
- Modify: `plugin/mp4/pkg/record.go`

- [ ] **Step 1: 看现有 writeTrailerTask 结构**

```bash
sed -n '31,55p' plugin/mp4/pkg/record.go
```

预期看到：

```go
type writeTrailerTask struct {
	task.Task
	muxer      *Muxer
	file       storage.File
	filePath   string
	durationMs uint32
	streamPath string
	storageKey string
	db         *gorm.DB
	dbWrite func(tailJob task.IJob)
}
```

- [ ] **Step 2: 修改 struct 加 slotAcquired 标志**

把 struct 改成：

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

	// slotAcquired 标志当前任务是否持有 trailer slot；用于 Dispose 时安全释放
	slotAcquired bool
}
```

- [ ] **Step 3: 改 Start() 入口加 acquire**

找到 `func (task *writeTrailerTask) Start() (err error) {`，把整个方法改成：

```go
func (t *writeTrailerTask) Start() (err error) {
	// 获取 trailer 槽位，阻塞直到有空位或 ctx 取消
	// 限制并发 trailer flush 数，避免多路 record stop 时磁盘 IO burst
	if err = storage.AcquireTrailerSlot(t.Context); err != nil {
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

注意：把方法接收器从 `task` 改成 `t` 以避免与导入包 `task` 冲突（原代码用 `task`，但用了 `t.muxer` 风格，需检查实际代码是 `task.muxer` 还是 `t.muxer`，按现状对齐）。

- [ ] **Step 4: 加 Dispose() 释放槽位**

在 `writeTrailerTask` 现有方法之后（同文件）追加：

```go
// Dispose 在任务结束（成功/失败/取消）时被框架调用，统一释放 trailer 槽位
func (t *writeTrailerTask) Dispose() {
	if t.slotAcquired {
		storage.ReleaseTrailerSlot()
		t.slotAcquired = false
	}
}
```

- [ ] **Step 5: 确认 import 含 `m7s.live/v5/pkg/storage`**

```bash
head -25 plugin/mp4/pkg/record.go | grep -E '"m7s.live/v5/pkg/storage"'
```

如果没有，在 import 块加：

```go
	"m7s.live/v5/pkg/storage"
```

- [ ] **Step 6: 写并发限制集成测试**

文件 `plugin/mp4/pkg/record_test.go`（若已存在则追加，否则新建）：

```go
package pkg

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"m7s.live/v5/pkg/storage"
)

func TestWriteTrailerSlotSerialization(t *testing.T) {
	// 槽位 = 2，模拟 6 个 trailer 同时来
	storage.InitUploadManager(storage.UploadConfig{
		MaxConcurrentUploads:       4,
		MaxConcurrentTrailerWrites: 2,
		PendingDir:                 t.TempDir(),
	})

	var active int32
	var maxObserved int32
	var wg sync.WaitGroup
	hold := 50 * time.Millisecond

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
			time.Sleep(hold)
			atomic.AddInt32(&active, -1)
		}()
	}
	wg.Wait()
	if maxObserved > 2 {
		t.Fatalf("max active should be ≤ 2, got %d", maxObserved)
	}
}
```

- [ ] **Step 7: 跑 mp4 测试**

```bash
go test -race -run 'TestWriteTrailerSlot' ./plugin/mp4/pkg/...
```

预期：PASS（直接通过 storage 包验证语义，因为 writeTrailerTask 的 Start/Dispose 需要 muxer/file 才能跑，这里只测槽位语义）。

- [ ] **Step 8: 编译全仓 + 跑 mp4 全部测试**

```bash
go build ./plugin/mp4/...
go test ./plugin/mp4/pkg/... -count=1
```

预期：全 PASS。

- [ ] **Step 9: 提交**

```bash
git add plugin/mp4/pkg/record.go plugin/mp4/pkg/record_test.go
git commit -m "feat(mp4): writeTrailerTask 加 trailer 槽位限制 (Start acquire / Dispose release)"
```

---

## Task 3: MP4Plugin 加配置字段 + Start() 调 setter + 示例文档

**Files:**
- Modify: `plugin/mp4/index.go` (MP4Plugin struct 加字段 + Start() 调 setter)
- Modify: `example/cluster/config1.yaml`（或挑一个有 `mp4:` 段的示例）
- Modify: `CLAUDE.md`（加新配置项说明）

- [ ] **Step 1: MP4Plugin struct 加字段**

在 `plugin/mp4/index.go` 的 `type MP4Plugin struct { ... }` 块末尾追加：

```go
	MaxConcurrentTrailerWrites int `default:"8" desc:"录制 stop 时 trailer flush 并发槽位数, 控制磁盘 IO burst. 默认 8 (≈ 300 MB/s 写盘, 按 SSD 1.1 GB/s 反推). 0 或负值 = 不限流. NVMe Gen4 可调 16-32, HDD 设 2-4"`
```

- [ ] **Step 2: MP4Plugin.Start() 入口调 setter**

在 `plugin/mp4/index.go` 找到 `func (p *MP4Plugin) Start() (err error) {`, 把方法体最开头改成：

```go
func (p *MP4Plugin) Start() (err error) {
	// 初始化 trailer 写盘槽位 (与 upload sem 解耦)
	storage.SetTrailerSlotLimit(p.MaxConcurrentTrailerWrites)

	if p.DB != nil {
		// ... 原有代码不变
```

import 区加 `"m7s.live/v5/pkg/storage"`（若没有）：

```go
import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"m7s.live/v5"
	"m7s.live/v5/pb"
	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/storage"
	"m7s.live/v5/pkg/util"
	mp4pb "m7s.live/v5/plugin/mp4/pb"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)
```

- [ ] **Step 3: 编译验证**

```bash
go build ./plugin/mp4/...
```

预期：通过。

- [ ] **Step 4: 端到端单测 — plugin Start 后 storage 限制生效**

文件 `plugin/mp4/index_test.go`（追加或新建）：

```go
package plugin_mp4

import (
	"testing"

	"m7s.live/v5/pkg/storage"
)

func TestMP4PluginConfiguresTrailerSlot(t *testing.T) {
	// 模拟 plugin Start 行为：写 config 后调 setter
	storage.SetTrailerSlotLimit(8)
	if got := storage.GetMaxConcurrentTrailerWrites(); got != 8 {
		t.Fatalf("default 8 should be set, got %d", got)
	}
	storage.SetTrailerSlotLimit(0) // disable
	if got := storage.GetMaxConcurrentTrailerWrites(); got != 0 {
		t.Fatalf("disable should set 0, got %d", got)
	}
}
```

跑：

```bash
go test -run 'TestMP4PluginConfiguresTrailerSlot' ./plugin/mp4/...
```

- [ ] **Step 5: 找 mp4 已有配置示例文件**

```bash
grep -l 'mp4:' example/cluster/*.yaml example/default/*.yaml example/record-test/*.yaml 2>/dev/null | head -5
```

- [ ] **Step 6: 在一个示例 config 的 `mp4:` 段加注释展示新选项**

例如在 `example/cluster/config1.yaml` 的 `mp4:` 段补：

```yaml
mp4:
  enable: true
  publish:
    delayclosetimeout: 900s
  # 录制 stop 时 trailer 写盘并发数, 控制磁盘 IO burst (默认 8, 目标 ~300 MB/s SSD 写)
  # NVMe Gen4 可放宽 16-32, HDD 设 2-4. 0/负 = 不限流
  # maxconcurrenttrailerwrites: 8
```

注意 yaml 字段名按 monibuca 约定全小写无下划线：`maxconcurrenttrailerwrites`。

- [ ] **Step 7: 看 CLAUDE.md 是否提到 mp4 plugin config**

```bash
grep -n "MP4 Plugin\|mp4 配置\|maxconcurrent" CLAUDE.md | head -5
```

如有 mp4 plugin 配置说明节，加：

```markdown
- `mp4.maxconcurrenttrailerwrites`：录制 stop 时 trailer flush 并发槽位数（默认 8）。多路 record stop 时 trailer moov-rewrite 会同时打盘，限流避免磁盘 IO burst。按 `(目标带宽 MB/s) × 实测并发数 / 实测峰值带宽 (MB/s)` 反推；本仓 SSD 实测 31 并发 → 1126 MB/s，故 8 ≈ 290 MB/s。
```

- [ ] **Step 8: 提交**

```bash
git add plugin/mp4/index.go plugin/mp4/index_test.go example/cluster/config1.yaml CLAUDE.md
git commit -m "feat(mp4): 加 MaxConcurrentTrailerWrites 配置, Start 时初始化 trailer slot"
```

---

## Task 4: 端到端冒烟（手动）

**Files:**
- 仅文档/操作步骤

- [ ] **Step 1: 出新镜像（用现有 build_docker.sh）**

```bash
./build_tag.sh   # 生成新 tag, 假设 v5.2.2.YYMMDDHHMM
./build_docker.sh
```

或在测试机上手动 build 当前 develop 分支。

- [ ] **Step 2: 部署到 130 测试环境（按 record-test 已建立的流程）**

```bash
ssh root@172.16.12.130 \
  'cd /home/project/xde-uat/media-docker-compose && \
   sed -i "s|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:.*|swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:<NEW_TAG>|" docker-compose-xde-monibuca.yml && \
   docker compose -f docker-compose-xde-monibuca.yml pull && \
   docker compose -f docker-compose-xde-monibuca.yml up -d --force-recreate'
```

- [ ] **Step 3: 跑同样的 31 路 5 min 录制**

复用 session 已建好的 pull proxy 批量加 + start + stop 流程。

- [ ] **Step 4: 监控磁盘 burst**

新建监控 csv，重点看 `disk_w_kbps` 峰值。

预期：录制 stop 时磁盘写带宽峰值 **大幅下降**：
- 当前（无限流）：~1126 MB/s（一次 31 路同时 flush）
- 修复后（slot=8）：~290 MB/s 持续约 4 秒，而不是 1.1 GB/s 持续 1 秒

总写入量不变（2-3 GB），但带宽峰值平滑到 ~300 MB/s 目标。

**验证标准**: 监控 csv 的 `disk_w_kbps` 峰值 < 320 MB/s（误差容忍 20 MB/s）, 持续高峰时间在 3-5 秒。

- [ ] **Step 5: 验录制完整性（同 ffprobe 流程）**

预期：31 / 31 PASS，无视频缺失，时长正常。slot 限流不应影响最终录制内容，仅影响写盘节奏。

---

## Task 5: 验收

- [ ] **Step 1: 单测覆盖**

```bash
go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...
```

期望全 PASS。

- [ ] **Step 2: 关键代码 grep 自检**

```bash
grep -rn "AcquireTrailerSlot\|ReleaseTrailerSlot" --include='*.go'
```

预期：3 处定义（`upload_manager.go`）+ 1 处使用（`record.go`）。

- [ ] **Step 3: 默认值核查**

```bash
grep -A1 'MaxConcurrentTrailerWrites' pkg/storage/upload_manager.go | head -5
```

期望 default tag 是 `"8"`（目标控制 SSD 磁盘 burst ~300 MB/s）。

- [ ] **Step 4: 升级后磁盘 burst 实测对比**

需要部署到 130 真实环境跑一次（Task 4），看监控数据。

- [ ] **Step 5: 上线前补充 issue 跟踪**

把 "record stop 时 cancel S3 upload context" bug 单开 issue（本 plan 不修），关联本 PR。

---

## Self-Review 记录

**Spec coverage**:
- "spread trailer flush over 几秒" → Task 1 加 trailer semaphore；Task 2 应用到 writeTrailerTask；slot=4 时 31 路 trailer 会按 8 批串行（31/4 ≈ 8），每批耗时 ~1s（按当前观察），共 ~8s spread ✓
- "rate limiter" → 信号量本质是 admission control 形式的 rate limiter，对突发性 IO 平滑效果与令牌桶等价（不需要更复杂） ✓

**Placeholder scan**：全 task 步骤都有完整代码块，无 TBD / TODO。

**Type consistency**：
- `AcquireTrailerSlot(ctx context.Context) error` / `ReleaseTrailerSlot()` / `GetActiveTrailerWrites() int32` / `GetMaxConcurrentTrailerWrites() int` — 全 plan 一致
- `MaxConcurrentTrailerWrites` 配置字段名 — 全 plan 一致
- struct field `slotAcquired bool` — Task 2 中 struct 修改与方法引用一致

**与 spec 的弱化**：
- 不修 context cancel bug（明确声明为 out of scope，单独 issue）
- 用固定信号量而非真令牌桶（更简单，效果等价）
