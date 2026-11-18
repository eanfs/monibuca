# WebRTC 2K@24 稳定播放：升级与优化实施计划

日期：2026-11-18
设计：[2026-11-18-webrtc-2k-upgrade-design.md](../specs/2026-11-18-webrtc-2k-upgrade-design.md)
基线：`origin/main`（4d726631）

## 任务总览

| # | 任务 | 依赖 | 修改范围 |
|---|---|---|---|
| 0 | 升级前基线测量 | — | `example/webrtc-2k-perf/`（新增，只读脚本） |
| 1 | 依赖升级到 pion v4.2.26 | 0 | `go.mod`/`go.sum`、必要的适配代码 |
| 2 | 启用 GCC 拥塞控制 | 1 | `plugin/webrtc/pkg/util.go`、`plugin/webrtc/index.go`、`default.yaml` |
| 3 | 可观测性（码率/丢包/PLI） | 2 | `plugin/webrtc/pkg/connection.go`、`index.go` |
| 4 | PLI 按需转发 + 清理空转 RTCP | 2 | `plugin/webrtc/pkg/connection.go` |
| 5 | 2K@24 功能与弱网验收 | 2,3,4 | 测试脚本与报告 |
| 6 | RTSP 拉流兼容性 | 1 | 测试脚本与报告 |
| 7 | 录制兼容性（RTSP 源 + WebRTC 流） | 1 | 测试脚本与报告、可能的录制器/文档微调 |
| 8 | 观众数压测 | 5 | 测试脚本与报告 |
| 9 | 回归、文档与 PR | 1-8 | README、default.yaml、PR |

---

## Task 0：升级前基线测量

**目标**：在升级前记录 2K@24 的实际表现，作为对比基线。

**新增**：

- `example/webrtc-2k-perf/README.md`：测试拓扑、命令、指标定义。
- `example/webrtc-2k-perf/push-2k.sh`：ffmpeg 推 2560×1440@24 H264 到 RTMP。
  - 目标码率 8–12 Mbps（可参数化 `-b:v 10M -maxrate 12M -bufsize 20M`）。
- `example/webrtc-2k-perf/play-test.html`（或复用插件自带页面）+ 一段浏览器控制台脚本，采集
  `getStats()`：framesDecoded / framesDropped / jitterBufferDelay / nackCount / currentRoundTripTime。
- `example/webrtc-2k-perf/report-baseline.md`：记录升级前数据。

**步骤**：

1. `cd example/default && go run -tags sqlite main.go`（或按现有启动方式）。
2. 运行 `push-2k.sh`，确认服务端收到 2560×1440@24。
3. Chrome 打开 `/webrtc/test/subscribe` 播放，采集 10 分钟指标。
4. 记录：平均帧率、丢帧、NACK/PLI 次数、延迟估计、CPU。

**验收**：报告文件存在且包含可复现命令与一组基线数字。

---

## Task 1：依赖升级到 pion v4.2.26

**目标**：`go.mod` 四个模块升级，编译与既有测试通过。

**修改**：

- `go.mod` / `go.sum`：
  - `github.com/pion/webrtc/v4` → `v4.2.26`
  - `github.com/pion/interceptor` → `v0.1.51`
  - `github.com/pion/rtp` → `v1.10.6`
  - `github.com/pion/rtcp` → `v1.2.20`
- 如 API 变化：适配 `plugin/webrtc/pkg/connection.go` / `util.go` / `index.go`。

**步骤**：

```bash
go get github.com/pion/webrtc/v4@v4.2.26 github.com/pion/interceptor@v0.1.51 \
       github.com/pion/rtp@v1.10.6 github.com/pion/rtcp@v1.2.20
go mod tidy
go build ./...
go test ./plugin/webrtc/... -count=1
```

**验收**：

- `go build ./...` 通过；
- `go test ./plugin/webrtc/... -count=1` 通过；
- 点导入触发的 API 变化全部修复，无行为降级。

**提交**：`chore(deps): 升级 pion/webrtc 到 v4.2.26`

---

## Task 2：启用 GCC 拥塞控制

**目标**：在 `CreateAPI` 中按顺序注册 GCC 与默认拦截器，新增配置开关。

**修改**：

- [plugin/webrtc/pkg/util.go](plugin/webrtc/pkg/util.go)：
  - `CreateAPI` 增加参数（cc 配置）或从包级配置读取。
  - 注册顺序：`cc.NewInterceptor(...)` → `RegisterDefaultInterceptors(m, i)`。
  - 使用 `gcc.NewSendSideBWE(gcc.SendSideBWEInitialBitrate(...), gcc.SendSideBWEMinBitrate(...), gcc.SendSideBWEMaxBitrate(...))`。
  - 通过 `OnNewPeerConnection` 保存 estimator（交给 Task 3）。
- [plugin/webrtc/index.go](plugin/webrtc/index.go)：`WebRTCPlugin` 增加
  `CC struct { Enable bool; InitialBitrate, MinBitrate, MaxBitrate int }`（YAML 小写）。
- `plugin/webrtc/default.yaml`：新增 `cc` 段默认值。

**步骤**：

1. 先写单测（断言 enable=true 时拦截器数量/顺序包含 cc；enable=false 时与旧行为一致）。
2. 实现注册与配置解析。
3. `gofmt` + 单测通过。
4. 手动起服务，`pc.getStats()` 确认 `targetBitrate` 出现。

**验收**：

- 单测通过；
- 关闭开关后行为与升级前一致（回滚路径 A8）；
- 日志能看到目标码率随网络变化。

**提交**：`feat(webrtc): 启用 GCC 发送端拥塞控制（可配置）`

---

## Task 3：可观测性（码率/丢包/PLI）

**目标**：每订阅者周期输出发送指标，作为验收数据源。

**修改**：

- [plugin/webrtc/pkg/connection.go](plugin/webrtc/pkg/connection.go)：
  - 保存 `BandwidthEstimator`（来自 Task 2 回调）。
  - 新增周期任务（task.TickTask 或现有任务内定时），每 10s 输出：
    `target_bitrate`、`send_bitrate`（可由 stats 或计数器估算）、`packets_sent`、
    `packets_lost`（来自 RTCP RR/NACK）、`rtt`、`jitter`、`pli_requests`。
- 统计计数用 atomic，避免锁。

**步骤**：

1. 定义指标结构。
2. 在 sender/RTCP 路径埋点。
3. 周期日志（Debug 或 Info，按配置）。
4. 单测：计数器与汇总函数。

**验收**：日志中能在弱网时看到目标码率下降、丢包数上升。

**提交**：`feat(webrtc): 增加发送端码率与丢包可观测性`

---

## Task 4：PLI 按需转发 + 清理空转 RTCP

**目标**：去掉固定 2s PLI，改为按需 + 节流；移除空转 goroutine。

**修改**：

- [plugin/webrtc/pkg/connection.go](plugin/webrtc/pkg/connection.go)：
  - 新增 PLI 节流器：同一 Publisher 500ms 内最多一次；5s 无关键帧兜底一次。
  - 触发点：订阅者 PLI（sender RTCP 读到的 PLI）、NACK 无法恢复的严重丢包。
  - 删除固定 `time.Since(lastPLISent) > IO.PLI` 的无条件请求。
  - 空转 RTCP 循环：先验证直接移除不影响发送；若需读取，则合并到节流器同一处理点。
- 配置 `PLI`（默认 2s）语义调整：改为“兜底/最小间隔”或新增 `PLIInterval`；保持向后兼容。

**步骤**：

1. 写单测：500ms 窗口内多次触发只发一次；5s 兜底触发。
2. 实现节流器。
3. 验证移除空转 goroutine 后发送正常（本地播放 + 弱网）。
4. 运行 Task 0 的 2K 测试脚本，确认 PLI 次数显著下降。

**验收**：

- 2K@24 播放 10 分钟，PLI 次数相比基线下降（目标 > 50%）；
- 弱网恢复不退化（A2/A3）。

**提交**：`perf(webrtc): PLI 按需转发并清理空转 RTCP 循环`

---

## Task 5：2K@24 功能与弱网验收

**目标**：跑完设计文档 A1–A3、A6–A8。

**步骤**：

1. 启动服务，2K 推流。
2. 浏览器单观众播放 30 分钟（A1）。
3. `tc netem`：
   - `tc qdisc add dev lo root netem loss 1% delay 50ms`（A2）
   - 3% + 150ms（A3）
   - 每条跑 10 分钟，记录恢复时间与帧率。
4. 记录到 `example/webrtc-2k-perf/report-after.md`。
5. 回归：H264/H265/AV1、AAC→Opus、WHIP、TCP 模式、batchv2（A6）。
6. 回滚验证：`cc.enable=false` 重启，确认行为与基线一致（A8）。

**验收**：报告包含 A1–A3、A6–A8 的实测数据与结论；未通过项写入问题清单。

**提交**：`test(webrtc): 2K@24 升级后验收报告`

---

## Task 6：RTSP 拉流兼容性

**目标**：升级后 RTSP 拉流（PullProxy）功能不回归，并覆盖 2K 源与重连场景。

**新增/复用**：

- `example/webrtc-2k-perf/rtsp-pull.sh`：起 ffmpeg RTSP 推流（2K@24）或复用现场摄像头。
- 复用现有拉流代理 API（`/api/proxy/pull/*`）配置拉流。

**步骤**：

1. 单路：RTSP 源（2K@24）→ 服务端拉流代理 → WebRTC 播放，确认转协议直通、帧率正常。
2. 并发：10 路 RTSP 拉流同时在线，观察服务端 CPU、内存、拉流状态。
3. 重连：杀掉源 5s 后恢复，确认拉流代理自动重连（`retryinterval`），WebRTC 观众恢复播放。
4. 清理：停止拉流代理，确认无残留任务/连接（日志与 `/api/proxy/pull/list`）。
5. 边界：源不存在时拉流代理不崩溃、错误可读。

**验收**：

- 拉流状态稳定，无重连风暴，无 goroutine/连接泄漏；
- 2K RTSP 源经 WebRTC 播放帧率 ≥ 23.5fps；
- 报告记录重连耗时与资源占用。

**提交**：`test(webrtc): RTSP 拉流兼容性验证`

---

## Task 7：录制兼容性（RTSP 源 + WebRTC 流）

**目标**：升级后 MP4 录制对 RTSP 拉流源与 WebRTC（WHIP）推流源均可用，覆盖 2K 视频与音频轨。

**新增/复用**：

- `example/webrtc-2k-perf/record-check.sh`：调用 `/mp4/api/start`、`/mp4/api/stop`，ffprobe 校验产物。
- 录制器已支持 Opus（`plugin/mp4/pkg/record.go:1162`、`track.go:213`），但需实测 Opus-in-MP4 播放器兼容性。

**步骤**：

1. **RTSP 源录制**：2K@24 拉流 → MP4 录制 10 分钟 → `ffprobe` 校验分辨率、帧率、时长、音轨；播放抽查。
2. **WebRTC 推流录制**：WHIP 推 H264 + Opus 音频 → MP4 录制 5 分钟 → `ffprobe` 校验；记录 Opus-in-MP4 在 VLC/Chrome 的播放结果（若浏览器不支持，记录为已知限制，不改录制器）。
3. **WebRTC 推 H265**（若客户端支持）→ 录制（可选，失败记录原因）。
4. **分片录制**：`fragment=1m` 录 3 分钟，确认分片数量与每片可播。
5. **边界**：录制中停止推流，确认文件正常收尾（moov 写入、无零字节文件）。
6. **与录制完整性联动**（可选）：强杀进程后重启，确认 sidecar 恢复逻辑对 WebRTC 源录制产物生效。

**验收**：

- `ffprobe` 输出符合预期（分辨率/帧率/时长/音轨），无零字节或损坏文件；
- 分片数量与时长正确；
- 报告记录 Opus-in-MP4 的播放器兼容性结论与任何已知限制。

**提交**：`test(mp4): WebRTC/RTSP 源录制兼容性验证`

---

## Task 8：观众数压测

**目标**：A4/A5。

**步骤**：

1. 10 观众（标准模式）同看一路 2K，记录 CPU、出向带宽、帧率。
2. 50 观众（batchv2 单 PC 多流），记录连接数、CPU、错误日志。
3. 如需更高，可到 100 观众，记录拐点。

**验收**：服务端不崩溃、无错误风暴；数据写入报告。

**提交**：`test(webrtc): 2K 多观众压测报告`

---

## Task 9：回归、文档与 PR

**目标**：全量回归、文档更新、PR 提交。

**步骤**：

1. `gofmt -w` 改动文件。
2. `go vet ./plugin/webrtc/...`。
3. `go test ./plugin/webrtc/... ./test -count=1`。
4. 更新 `plugin/webrtc/default.yaml` 与 `README_CN.md`（新增 cc 配置说明）。
5. 更新设计/计划文档状态（已实现）。
6. 提交并开 PR（base=main），PR 描述包含：
   - 升级版本表
   - 2K@24 前后对比数据
   - RTSP 拉流与录制兼容性结论
   - 回滚方式（`cc.enable=false`）

**验收**：

- 测试全绿；
- PR 描述包含验收数据；
- 设计文档第 5 节验收表逐项勾选。

---

## 测试矩阵（汇总）

| 测试 | 命令/方式 | 通过标准 |
|---|---|---|
| 编译 | `go build ./...` | 零错误 |
| 单测 | `go test ./plugin/webrtc/... -count=1` | 全绿 |
| 2K 单观众 | `push-2k.sh` + 浏览器 | A1 |
| 弱网 1% | `tc netem loss 1% delay 50ms` | A2 |
| 弱网 3% | `tc netem loss 3% delay 150ms` | A3 |
| 10 观众 | 标准模式 | A4 |
| 50 观众 | batchv2 | A5 |
| 回归 | 编码/协议清单 | A6 |
| 可观测 | 日志 | A7 |
| 回滚 | `cc.enable=false` | A8 |
| RTSP 拉流单路 | `rtsp-pull.sh`（2K 源） | A9 |
| RTSP 拉流重连 | 杀源 5s 后恢复 | A10 |
| RTSP 源录制 | `record-check.sh` + ffprobe | A11 |
| WebRTC 流录制 | WHIP 推流 + ffprobe | A12 |
| 分片录制 | `fragment=1m` | A13 |

## 风险与应对（执行时）

- 若 Task 1 编译改动过大，拆成“适配 commit + 行为 commit”两次提交，便于审阅。
- 若 GCC 在 8–12 Mbps 下表现不佳，先按 A2/A3 数据调参（initial/max/min），不急于改架构。
- 若 Task 4 发现移除 RTCP 循环会阻塞，保留读取但合并到节流器，记录原因。
