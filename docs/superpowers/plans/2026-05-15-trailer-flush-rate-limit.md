# MP4 Trailer Flush 限流 实施计划

> **状态：已完成（部分回滚）· 磁盘 burst 已被后续 plan 取代** — 2026-05-15 合并到 develop（`be9915a1`）
>
> Task 1（storage API）已合并并保留。Task 2（mp4 plugin 调用）经三次迭代后回滚，原因见下方「执行结果」节。
>
> ⚠️ **本 plan 遗留的「磁盘 burst 未解决」（Task 6 / 遗留 Issue 1）已由 `2026-05-16-trailer-rewrite-io-reduction.md` 解决并合并 develop（`eb828a11`）**：
> 后续 plan 用「消除 trailer 回拷（每文件写盘 2×→1×）+ `TrailerWriteRateMBps` 写盘限速器」两个正确杠杆，替代了本 plan 失败的并发信号量方案。本文件 Task 6 与「遗留 Issue 1」仅作历史记录，不再跟进。

**Goal:** 在录制插件的 trailer flush 流程加并发槽位限制，把 record stop 时磁盘写带宽峰值控制在 300 MB/s 以内（实测无限流时峰值 1.1 GB/s ≈ SSD 顺序写上限）。

**目标值推导（原始）：** 31 路并发产生 1126 MB/s → 期望 300 MB/s → 槽位数 = 31 × 300/1126 ≈ **8**。默认 `MaxConcurrentTrailerWrites=8`。

**Architecture:** 在 `pkg/storage` 加一个 `trailerSem` 信号量，配置字段加在 `UploadConfig` 里（与 `MaxConcurrentUploads` 并列），通过 `InitUploadManager` 一次性初始化。**配置归 storage**，因为 trailer 是磁盘 IO 共享关切：mp4/flv/任何录制 plugin 写盘都过 storage 层。

**Tech Stack:** Go channel-based semaphore，沿用 `pkg/storage/upload_manager.go` 既有 `uploadSem` 模式。

---

## ⚠️ 根因分析修正（执行后发现）

**原始假设（错误）：**
> `writeTrailerQueueTask.AddTask(t)` 把 31 个 trailer task 并发拉起，每个都做 moov 重排 + bufio flush

**实际情况：**
`writeTrailerQueueTask` 是全局单例 `task.Work`，其 event loop 是**单线程**的（`event_loop.go:67` 只启动一个 goroutine）。`task.start()` 在 event loop goroutine 中同步调用 `Run()`（`task.go:387`），`Run()` 阻塞期间 event loop 无法处理下一个 task。因此 31 个 trailer task 是**严格串行**的，不存在并发 burst。

**1126 MB/s 的真正来源：**
单个 trailer 的 `bufio.Flush()` 瞬间打满 SSD 顺序写速度（~1 GB/s）。这是单个 ~300 MB 文件在 <0.3 秒内写完的物理特性，不是多路并发叠加。

**信号量方案的局限：**
由于 event loop 单线程，`activeTrailerWrites` 永远不超过 1，信号量永远不会触发限流。在 `Run()` 内 acquire slot 时若 slot 满会阻塞 event loop，理论上存在死锁风险（实际因 slot 永远不满而不触发）。

**真正需要的方案（未实现，见 Task 6）：**
限速 Writer（rate-limited io.Writer），控制单个 trailer 的写盘速率，而非限制并发数。

---

## 执行结果

### Task 1: pkg/storage 加 trailerSem API ✅ 已完成

提交：`c88be04b feat(storage): 加 trailerSem 限制 record stop 时 trailer 并发写盘`

**实际文件：**
- `pkg/storage/upload_manager.go`：已加 `trailerSem`、`MaxConcurrentTrailerWrites` 字段、`AcquireTrailerSlot / ReleaseTrailerSlot / GetActiveTrailerWrites / GetMaxConcurrentTrailerWrites`
- `pkg/storage/trailer_slot_test.go`：4 个测试全 PASS（文件名与计划略有差异，内容等价）

**保留理由：** API 完整可用，未来若 trailer queue 改为 worker-pool 并发模式，可直接激活。`UploadConfig.MaxConcurrentTrailerWrites` 字段标注为 `[预留]`。

### Task 2: writeTrailerTask 用 trailer slot ❌ 回滚

三次迭代历史：

| 提交 | 内容 | 结果 |
|------|------|------|
| `6ea556dc` | `Start()` acquire / `Dispose()` release | 问题：slot 覆盖整个 task 生命周期含 S3 upload，MinIO 503 时 upload 重试 21 分钟占着 slot，后续 trailer 全部串行等待 |
| `c46c7255` | slot 移到 `Run()` 内，磁盘 IO 阶段持有，`t.file.Close()` 前显式 release | 实测 131 路 5min disk peak: 1126 MB/s → 211 MB/s（-81%）；但数据可信度存疑（见下） |
| `98972ab9` | 回滚 mp4 改动 | 调研确认 event loop 单线程，slot 无效，回滚 |

**`c46c7255` 的 211 MB/s 数据说明：**
由于 event loop 单线程，`AcquireTrailerSlot` 在 `Run()` 内永远立即返回（slot 永远不满），对磁盘 IO 速率没有实际影响。211 MB/s 的数据可能来自测试环境差异或测量方法，不应作为验收基准。

### Task 3: 配置示例文档 ✅ 已完成

提交：`90a8a95b docs(record-test): 加 maxconcurrenttrailerwrites 配置示例注释`

`example/record-test/config.yaml` 已加注释，措辞已更新为 `[预留]`，说明当前不影响实际行为。

### Task 4 & 5: 端到端验收 ⏭️ 跳过

Task 2 回滚后，磁盘 burst 问题未解决，端到端验收无意义。

---

## 文件结构（实际）

```
pkg/storage/
├── upload_manager.go          [已改] UploadConfig 加 MaxConcurrentTrailerWrites [预留]字段
│                                     trailerSem 全局变量 + InitUploadManager 初始化
│                                     AcquireTrailerSlot / ReleaseTrailerSlot / Getter
└── trailer_slot_test.go       [已建] 4 个信号量行为单测（全 PASS）

plugin/mp4/pkg/
└── record.go                  [未改] 回滚到 v5.1.6 baseline，无 slot 调用

example/record-test/config.yaml  [已改] 加 maxconcurrenttrailerwrites [预留] 注释
```

---

## Task 6: 真正解决磁盘 burst（待实现）

**问题：** 单个 trailer 的 `bufio.Flush()` 瞬间打满 SSD（~1 GB/s），是物理写盘速率问题，不是并发问题。

**可行方案：**

### 方案 A：限速 Writer（推荐）

在 `writeTrailerTask.Run()` 的 `bufio.NewWriterSize(temp, 1<<20)` 外包一个 rate-limited writer：

```go
import "golang.org/x/time/rate"

// 目标：单个 trailer 写盘速率 ≤ 100 MB/s
// 31 路串行时总峰值 ≤ 100 MB/s（远低于 SSD 上限）
limiter := rate.NewLimiter(rate.Limit(100*1024*1024), 1*1024*1024) // 100 MB/s, 1 MB burst
bw := bufio.NewWriterSize(&rateLimitedWriter{w: temp, limiter: limiter}, 1<<20)
```

**代价：** 单个 trailer 写盘时间从 ~0.3s 延长到 ~3s（300 MB ÷ 100 MB/s）。31 路串行总时间 ~93s。

**适用场景：** 对磁盘 burst 敏感（共享存储、HDD、RAID 阵列）。

### 方案 B：改 writeTrailerQueueTask 为 worker pool

把 `task.Work` 的 event loop 改为多 goroutine 并发处理，这样 `trailerSem` 才有意义。

**代价：** 需要改 gotask 使用方式或引入自定义 worker pool，改动较大。

**适用场景：** 希望多路 trailer 并发写盘（利用多核 + SSD 并发 IO），同时用 sem 控制总带宽。

### 方案 C：接受现状

31 路串行 trailer，每路 ~0.3s，总时间 ~10s，期间磁盘 IO 是脉冲式的（每次 ~1 GB/s 持续 0.3s，间隔 ~0s）。对 NVMe SSD 无实质伤害，只是监控数字难看。

**适用场景：** 磁盘性能充裕，无共享 IO 竞争。

---

## 遗留 Issue

1. **磁盘 burst 未解决**：需要方案 A 或 B（见 Task 6）
2. **flv plugin 评估**：flv 是否有同样的写盘 burst 问题，需独立 verify
3. **record stop cancel bug**：已由 `2026-05-15-storage-upload-detach-recorder-context.md` 修复
