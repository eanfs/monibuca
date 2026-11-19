# WebRTC 2K@24 升级与性能优化实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把 WebRTC 插件升级到 pion v4.2.26 并启用 GCC 拥塞控制，使 2K（2560×1440）@24fps 在弱网下稳定播放，同时验证 RTSP 拉流与 MP4 录制（RTSP/WebRTC 源）不回归。

**Architecture:** 升级依赖后在 `CreateAPI` 的拦截器链最前面插入 GCC（`cc` + `gcc`），把默认注册的 NACK/RTCP/TWCC 保留在其后；订阅侧用可配置的 PLI 节流器替代固定 2s 请求，并输出码率/丢包指标。录制与拉流不改代码，只补兼容性验证脚本与验收数据。

**Tech Stack:** Go 1.26、pion/webrtc v4、pion/interceptor、ffmpeg、tc netem、Chrome getStats。

**Spec:** `docs/superpowers/specs/2026-11-18-webrtc-2k-upgrade-design.md`

## Global Constraints

- 依赖版本：`github.com/pion/webrtc/v4 v4.2.26`、`github.com/pion/interceptor v0.1.51`、`github.com/pion/rtp v1.10.6`、`github.com/pion/rtcp v1.2.20`。
- 不升级到 `v5.0.0-rc`；不引入服务端转码；不做 simulcast/SVC；不改 batchv2 信令。
- 2K 规格：2560×1440@24fps；H264 8–12 Mbps，H265 5–8 Mbps。
- GCC 默认：`enable=true`、`initialbitrate=2000000`、`minbitrate=300000`、`maxbitrate=20000000`。
- PLI 节流：最小间隔 500ms，无关键帧兜底 5s。
- 1080p 帧率保底：正常网络 ≥ 29fps；1%/3% 丢包下平均 ≥ 24fps（前提可用带宽 ≥ 2.5 Mbps）。
- 点导入仅允许 `. "m7s.live/v5/pkg"`（staticcheck 白名单）；webrtc 包现有 `. "github.com/pion/webrtc/v4"` 保留。
- 日志使用结构化 kv（`Info("msg", "key", value)`），不新增裸 goroutine。
- 每个任务结束前：`gofmt -w` 改动文件、`go test <affected package> -count=1` 通过。

## Review Focus

1. **2K 弱网时目标码率是否真的下降**：GCC 在高码率（8–12 Mbps）下的收敛；若只降不升或震荡，2K 画质会崩。归 Task 7 的弱网验收测试。
2. **WebRTC 推流的 Opus 音频录进 MP4 后能否播放**：录制器支持 Opus，但 Opus-in-MP4 浏览器支持有限；结论必须是实测记录，不是假设。归 Task 6。
3. **RTSP 断流重连后 WebRTC 订阅者是否自动恢复**：拉流代理重连成功但订阅者未恢复是常见断点。归 Task 5。
4. **`cc.enable=false` 回滚后是否与升级前行为一致**：注册顺序/统计埋点不能改变无 cc 时的行为。归 Task 2 的单测与 Task 9 的回滚验证。
5. **H265 客户端不支持时的 DataChannel 降级路径不退化**：升级可能改变协商细节。归 Task 9 的回归清单。

## File Structure

| 文件 | 职责 | 任务 |
|---|---|---|
| `plugin/webrtc/pkg/util.go` | 拦截器注册（GCC 在前）、`CreateAPI` 签名 | 2 |
| `plugin/webrtc/index.go` | `WebRTCPlugin` 配置（`CC` 字段）、`CreatePC` 传参 | 2 |
| `plugin/webrtc/pkg/metrics.go`（新建） | 发送指标结构与日志格式化 | 3 |
| `plugin/webrtc/pkg/pli.go`（新建） | PLI 节流器 | 4 |
| `plugin/webrtc/pkg/connection.go` | 指标埋点、PLI 按需化、清理空转 RTCP | 3、4 |
| `plugin/webrtc/default.yaml` | `cc` 配置示例/默认值 | 2、9 |
| `example/webrtc-2k-perf/`（新建） | 2K 推流、播放采集、RTSP 拉流、录制校验脚本与报告 | 0、5、6、7、8 |
| `plugin/webrtc/README_CN.md` | 配置说明 | 9 |

---

## Task 0: 基线测量脚本与数据

**Files:**
- Create: `example/webrtc-2k-perf/README.md`
- Create: `example/webrtc-2k-perf/push-2k.sh`
- Create: `example/webrtc-2k-perf/play-collect.js`
- Create: `example/webrtc-2k-perf/report-baseline.md`

**Interfaces:**
- Consumes: 现有 `/webrtc/play/{streamPath}` 与 `/webrtc/test/subscribe` 页面。
- Produces: 可复现的基线命令与一组升级前指标，供 Task 7 对比。

- [ ] **Step 1: 写 2K 推流脚本**

`push-2k.sh` 用 ffmpeg 产出 2560×1440@24 H264：

```bash
ffmpeg -re -f lavfi -i testsrc2=size=2560x1440:rate=24 \
  -c:v libx264 -preset veryfast -b:v 10M -maxrate 12M -bufsize 20M \
  -g 48 -pix_fmt yuv420p -f flv "rtmp://127.0.0.1/live/2k"
```

- [ ] **Step 2: 写播放采集脚本**

`play-collect.js` 在浏览器控制台执行 `pc.getStats()`，每 5s 打印
`framesDecoded`、`framesDropped`、`jitterBufferDelay`、`nackCount`、`currentRoundTripTime`。

- [ ] **Step 3: 运行基线并记录**

Run: `cd example/default && go run -tags sqlite main.go`，另开终端跑 `push-2k.sh`，Chrome 播放 10 分钟。
Expected: 报告 `report-baseline.md` 记录平均帧率、丢帧、NACK/PLI 次数、CPU。

- [ ] **Step 4: 提交**

```bash
git add example/webrtc-2k-perf
git commit -m "test(webrtc): 新增 2K@24 基线测量脚本与数据"
```

---

## Task 1: 依赖升级到 pion v4.2.26

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify（仅在编译报错时）: `plugin/webrtc/pkg/*.go`、`plugin/webrtc/*.go`

**Interfaces:**
- Produces: 后续任务使用的 pion v4.2.26 API；`CreateAPI` 在本任务暂不改签名。

- [ ] **Step 1: 升级四个模块**

```bash
go get github.com/pion/webrtc/v4@v4.2.26 github.com/pion/interceptor@v0.1.51 \
       github.com/pion/rtp@v1.10.6 github.com/pion/rtcp@v1.2.20
go mod tidy
```

- [ ] **Step 2: 编译并修复 API 变化**

Run: `go build ./...`
Expected: 若失败，错误只涉及 pion API 改名/签名；按错误修 `plugin/webrtc`，不改业务语义。

- [ ] **Step 3: 跑现有测试**

Run: `go test ./plugin/webrtc/... -count=1`
Expected: PASS。

- [ ] **Step 4: 提交**

```bash
git add go.mod go.sum plugin/webrtc
git commit -m "chore(deps): 升级 pion/webrtc 到 v4.2.26"
```

---

## Task 2: 启用 GCC 拥塞控制

**Files:**
- Modify: `plugin/webrtc/pkg/util.go:53-66`
- Modify: `plugin/webrtc/index.go:40-60`（`WebRTCPlugin` 字段）、`index.go:126-140`（`CreatePC`）
- Modify: `plugin/webrtc/pkg/cloudflare.go:81`、`plugin/webrtc/pkg/client.go:27`（新签名传空配置）
- Modify: `plugin/webrtc/default.yaml`
- Test: `plugin/webrtc/pkg/util_test.go`（新建）

**Interfaces:**
- Consumes: pion v4.2.26（Task 1）。
- Produces:
  - `type CCConfig struct { Enable bool; InitialBitrate, MinBitrate, MaxBitrate int }`
  - `func createInterceptorRegistry(m *MediaEngine, ccConfig CCConfig, onEstimator func(cc.BandwidthEstimator)) (*interceptor.Registry, error)`
  - `func CreateAPI(ssd *sdp.SessionDescription, s SettingEngine, ccConfig CCConfig, onEstimator func(cc.BandwidthEstimator)) (*API, error)`
  - `WebRTCPlugin.CC CCConfig`（yaml `cc`），默认值 `enable=true`、`initialbitrate=2000000`、`minbitrate=300000`、`maxbitrate=20000000`

- [ ] **Step 1: 写失败测试**

`plugin/webrtc/pkg/util_test.go`：

```go
func TestCreateInterceptorRegistryCCEnabled(t *testing.T) {
    m := &MediaEngine{}
    if err := registerLegacyCodecs(m); err != nil { t.Fatal(err) }

    withCC, err := createInterceptorRegistry(m, CCConfig{Enable: true}, nil)
    if err != nil { t.Fatal(err) }
    withoutCC, err := createInterceptorRegistry(m, CCConfig{Enable: false}, nil)
    if err != nil { t.Fatal(err) }

    a, err := withCC.Build("test")
    if err != nil { t.Fatal(err) }
    b, err := withoutCC.Build("test")
    if err != nil { t.Fatal(err) }
    if len(a) != len(b)+1 {
        t.Fatalf("cc enabled should add exactly one interceptor: got %d vs %d", len(a), len(b))
    }
}

func TestCreateInterceptorRegistryCCDefaults(t *testing.T) {
    m := &MediaEngine{}
    if err := registerLegacyCodecs(m); err != nil { t.Fatal(err) }
    // 启用了也不应因为 0 值 panic
    if _, err := createInterceptorRegistry(m, CCConfig{Enable: true, InitialBitrate: 0, MinBitrate: 0, MaxBitrate: 0}, nil); err != nil {
        t.Fatal(err)
    }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./plugin/webrtc/pkg -run 'TestCreateInterceptorRegistryCC' -v`
Expected: FAIL，`undefined: createInterceptorRegistry`。

- [ ] **Step 3: 实现 CCConfig 与注册函数**

`util.go` 新增：`cc.NewInterceptor`（factory 返回 `gcc.NewSendSideBWE`，非零值才传对应 Option），
随后调用 `RegisterDefaultInterceptors(m, i)`；`onEstimator != nil` 时用
`ccInterceptor.OnNewPeerConnection` 回调。`CreateAPI` 改为接收 `ccConfig` 与回调。

`index.go`：`WebRTCPlugin` 增加 `CC CCConfig` 字段（`desc` 标注），`CreatePC` 调
`CreateAPI(ssd, p.s, p.CC, p.onBandwidthEstimate)`；`onBandwidthEstimate` 本任务先空实现（Task 3 接入）。

`cloudflare.go`/`client.go`：`CreateAPI(nil, SettingEngine{}, CCConfig{}, nil)`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./plugin/webrtc/pkg -run 'TestCreateInterceptorRegistryCC' -v`
Expected: PASS。

- [ ] **Step 5: 补默认配置**

`default.yaml` 增加：

```yaml
cc:
  enable: true
  initialbitrate: 2000000
  minbitrate: 300000
  maxbitrate: 20000000
```

- [ ] **Step 6: 编译并提交**

Run: `go build ./...` → PASS。

```bash
git add plugin/webrtc
git commit -m "feat(webrtc): 启用 GCC 拥塞控制（可配置）"
```

---

## Task 3: 发送端可观测性

**Files:**
- Create: `plugin/webrtc/pkg/metrics.go`
- Test: `plugin/webrtc/pkg/metrics_test.go`
- Modify: `plugin/webrtc/pkg/connection.go`（埋点与周期日志）

**Interfaces:**
- Consumes: Task 2 的 `cc.BandwidthEstimator` 回调。
- Produces:
  - `type StreamMetrics struct { PacketsSent, BytesSent, PacketsLost, PLIRequests atomic.Uint64; TargetBitrate, SendBitrate atomic.Int64 }`
  - `func (m *StreamMetrics) Snapshot() MetricsSnapshot`
  - `type MetricsSnapshot struct { PacketsSent, BytesSent, PacketsLost, PLIRequests uint64; TargetBitrate, SendBitrate int64 }`
  - `func (s MetricsSnapshot) LogLine() string`

- [ ] **Step 1: 写失败测试**

`metrics_test.go`：累加 `PacketsSent` 与设置 `TargetBitrate` 后，`Snapshot()` 返回对应值；
`LogLine()` 包含 `target_bitrate=`、`packets_sent=` 两个键。

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./plugin/webrtc/pkg -run TestStreamMetrics -v`
Expected: FAIL，`undefined: StreamMetrics`。

- [ ] **Step 3: 实现 metrics.go 并埋点**

- `MultipleConnection` 增加 `metrics *StreamMetrics`（`Start` 时初始化）。
- 视频/音频 `WriteRTP` 成功后 `metrics.PacketsSent.Add(1)` 且 `metrics.BytesSent.Add(uint64(len(p.Payload)))`（两处闭包）。
- RTCP 读循环里把 `*rtcp.ReceiverReport` 各报告块的 `TotalLost` 累加到 `PacketsLost`（累计值，不做差分）。
- 每 10s（`time.Since(lastLog)`，在视频循环内）：
  - `SendBitrate = (BytesSent - prevBytes) * 8 / elapsedSeconds`，写入 `SendBitrate` 并更新 `prevBytes`；
  - 输出 `IO.Debug("webrtc stats", "metrics", snap.LogLine())`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./plugin/webrtc/pkg -run TestStreamMetrics -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add plugin/webrtc/pkg/metrics.go plugin/webrtc/pkg/metrics_test.go plugin/webrtc/pkg/connection.go
git commit -m "feat(webrtc): 增加发送端码率与丢包可观测性"
```

---

## Task 4: PLI 按需转发 + 清理空转 RTCP

**Files:**
- Create: `plugin/webrtc/pkg/pli.go`
- Test: `plugin/webrtc/pkg/pli_test.go`
- Modify: `plugin/webrtc/pkg/connection.go:195-201`（Receive 侧固定 PLI）
- Modify: `plugin/webrtc/pkg/connection.go:313-330`、`:542-560`（空转 RTCP 循环）

**Interfaces:**
- Produces:
  - `type PLIThrottler struct { MinInterval, Fallback time.Duration; lastSent time.Time }`
  - `func NewPLIThrottler(minInterval, fallback time.Duration) *PLIThrottler`
  - `func (p *PLIThrottler) Allow(now, lastKeyFrame time.Time) bool`

- [ ] **Step 1: 写失败测试**

`pli_test.go`：

```go
func TestPLIThrottlerWindow(t *testing.T) {
    now := time.Unix(0, 0)
    p := NewPLIThrottler(500*time.Millisecond, 5*time.Second)
    if !p.Allow(now, now) { t.Fatal("first request must pass") }
    if p.Allow(now.Add(100*time.Millisecond), now) { t.Fatal("inside window must be blocked") }
    if !p.Allow(now.Add(600*time.Millisecond), now) { t.Fatal("after window must pass") }
}

func TestPLIThrottlerFallback(t *testing.T) {
    now := time.Unix(0, 0)
    p := NewPLIThrottler(time.Hour, 5*time.Second)
    if !p.Allow(now, now) { t.Fatal("first request must pass") }
    if p.Allow(now.Add(time.Second), now.Add(-10*time.Second)) != true {
        t.Fatal("fallback must fire when no keyframe for > fallback")
    }
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./plugin/webrtc/pkg -run TestPLIThrottler -v`
Expected: FAIL，`undefined: NewPLIThrottler`。

- [ ] **Step 3: 实现 PLIThrottler**

`Allow` 逻辑：`now.Sub(lastSent) >= MinInterval` → 通过并更新 `lastSent`；
否则若 `Fallback > 0 && now.Sub(lastKeyFrame) >= Fallback` → 通过并更新 `lastSent`；其他返回 false。

- [ ] **Step 4: 接入 Receive 侧**

- `Receive()` 中删除固定 `time.Since(lastPLISent) > IO.PLI` 的无条件 PLI。
- `MultipleConnection` 增加 `pliThrottler *PLIThrottler`；视频闭包里用 `frame.IDR`（`pkg/avframe.go:42`）
  为 true 时更新 `lastKeyFrame = time.Now()`。
- 订阅者 PLI（`videoSender.Read` 读到的 `*rtcp.PictureLossIndication`）与严重丢包路径调用
  `pliThrottler.Allow(time.Now(), lastKeyFrame)`，true 时才向 Publisher 发 PLI。

- [ ] **Step 5: 处理空转 RTCP 循环**

先直接移除两处 `videoSender.Read` 循环外的空 switch；若实机验证发现发送阻塞（有 RTCP 必须消费），
保留读取但把 PLI 分支接到 Step 4 的节流器，并在代码注释与计划末尾记录原因。

- [ ] **Step 6: 运行测试与编译**

Run: `go test ./plugin/webrtc/pkg -run TestPLIThrottler -v` → PASS；`go build ./...` → PASS。

- [ ] **Step 7: 提交**

```bash
git add plugin/webrtc/pkg/pli.go plugin/webrtc/pkg/pli_test.go plugin/webrtc/pkg/connection.go
git commit -m "perf(webrtc): PLI 按需转发并清理空转 RTCP 循环"
```

---

## Task 5: RTSP 拉流兼容性验证

**Files:**
- Create: `example/webrtc-2k-perf/rtsp-pull.sh`
- Create: `example/webrtc-2k-perf/report-rtsp-pull.md`

**Interfaces:**
- Consumes: 现有拉流代理 API（`/api/proxy/pull/*`）与 Task 0 的 2K 推流脚本。
- Produces: A9/A10 的实测数据。

- [ ] **Step 1: 写拉流脚本**

`rtsp-pull.sh`：用 ffmpeg 起 RTSP 服务（或复用现场摄像头），调用拉流代理 API 拉 2K 流，
等待状态为 Online 后断言。

- [ ] **Step 2: 单路拉流测试**

Run: `./rtsp-pull.sh start`，WebRTC 播放 10 分钟。
Expected: 播放帧率 ≥ 23.5fps；`/api/proxy/pull/list` 状态 Online。

- [ ] **Step 3: 10 路并发**

Run: 脚本循环起 10 路（不同 streamPath），检查 CPU/内存/连接。
Expected: 无错误日志风暴，无 goroutine 泄漏（进程 RSS 稳定）。

- [ ] **Step 4: 断流重连**

Run: 杀源 5s 后重启，观察拉流代理与 WebRTC 观众。
Expected: ≤ 5s 重新出流，观众恢复播放（A10）。

- [ ] **Step 5: 记录并提交**

写入 `report-rtsp-pull.md`，然后：

```bash
git add example/webrtc-2k-perf
git commit -m "test(webrtc): RTSP 拉流兼容性验证"
```

---

## Task 6: 录制兼容性验证（RTSP 源 + WebRTC 流）

**Files:**
- Create: `example/webrtc-2k-perf/record-check.sh`
- Create: `example/webrtc-2k-perf/report-record.md`

**Interfaces:**
- Consumes: `/mp4/api/start|stop/<streamPath>`、`ffprobe`、Task 5 的 RTSP 源。
- Produces: A11/A12/A13 的实测数据与 Opus-in-MP4 结论。

- [ ] **Step 1: 写录制校验脚本**

`record-check.sh`：调用 `/mp4/api/start`（`{"duration":"600s","fragment":"0"}`）→ 等待 → `/mp4/api/stop`
→ `ffprobe -v error -show_entries stream=codec_type,codec_name,width,height,avg_frame_rate -show_entries format=duration`。

- [ ] **Step 2: RTSP 源录制**

Run: 2K RTSP 源录制 10 分钟。
Expected: ffprobe 分辨率 2560×1440、帧率 ≈24、时长 ≈600s、有音轨（若源有音频）。

- [ ] **Step 3: WebRTC 推流录制**

Run: WHIP 推 H264 + Opus 音频，录制 5 分钟。
Expected: ffprobe 视频正常；音频 codec 为 opus；记录 VLC/Chrome 播放结论（Chrome 不支持则写入已知限制）。

- [ ] **Step 4: 分片录制**

Run: `fragment=1m` 录 3 分钟。
Expected: 3 个分片，每片可播、时长 ≈60s（A13）。

- [ ] **Step 5: 停止源后的收尾**

Run: 录制中停止推流/拉流源。
Expected: 文件收到 moov，无零字节文件。

- [ ] **Step 6: 记录并提交**

```bash
git add example/webrtc-2k-perf
git commit -m "test(mp4): WebRTC/RTSP 源录制兼容性验证"
```

---

## Task 7: 2K@24 功能与弱网验收

**Files:**
- Create: `example/webrtc-2k-perf/report-after.md`

**Interfaces:**
- Consumes: Task 2/3/4 的实现与 Task 0 的脚本。
- Produces: A1–A3、A6–A8 的实测数据。

- [ ] **Step 1: 单观众 30 分钟**

Run: `push-2k.sh` + Chrome 播放 30 分钟。
Expected: 帧率 ≥ 23.5fps，无连续 >1s 花屏，延迟 P95 ≤ 1.2s（A1）。

- [ ] **Step 2: 弱网 1%**

Run: `tc qdisc add dev lo root netem loss 1% delay 50ms`，播放 10 分钟。
Expected: 无永久卡死，恢复 ≤ 3s，平均帧率 ≥ 22fps（A2）。

- [ ] **Step 3: 弱网 3%**

Run: `tc qdisc change dev lo root netem loss 3% delay 150ms`，播放 10 分钟。
Expected: 恢复 ≤ 5s（A3）。

- [ ] **Step 4: 1080p 保底验证**

Run: ffmpeg 推 1920×1080@30（`-b:v 4M -maxrate 5M`），Chrome 播放 10 分钟。
Expected: 正常网络平均帧率 ≥ 29fps；切 1%/3% 丢包后平均帧率 ≥ 24fps，且无持续低于 20fps 的区间（A1b/A1c）。

- [ ] **Step 5: 回归与回滚**

- H264/H265/AV1、AAC→Opus、WHIP、TCP 模式、batchv2（A6）。
- `cc.enable=false` 重启，重复 A1 关键项，行为与基线一致（A8）。

- [ ] **Step 6: 记录并提交**

```bash
git add example/webrtc-2k-perf/report-after.md
git commit -m "test(webrtc): 2K@24 升级后验收报告"
```

---

## Task 8: 观众数压测

**Files:**
- Create: `example/webrtc-2k-perf/report-load.md`

**Interfaces:**
- Consumes: Task 7 的 2K 源。

- [ ] **Step 1: 10 观众（标准模式）**

Run: 10 个浏览器/脚本观众同看一路 2K，记录 CPU、出向带宽、帧率。
Expected: CPU（16 核基准）≤ 50%（A4）。

- [ ] **Step 2: 50 观众（batchv2）**

Run: batchv2 单 PC 多流 50 观众。
Expected: 无连接崩溃、无错误风暴（A5）。

- [ ] **Step 3: 记录并提交**

```bash
git add example/webrtc-2k-perf/report-load.md
git commit -m "test(webrtc): 2K 多观众压测报告"
```

---

## Task 9: 回归、文档与 PR

**Files:**
- Modify: `plugin/webrtc/README_CN.md`
- Modify: `plugin/webrtc/default.yaml`（如 Task 2 未补齐）
- Modify: `docs/superpowers/specs/2026-11-18-webrtc-2k-upgrade-design.md`（状态改为已实现）
- Modify: `docs/superpowers/plans/2026-11-18-webrtc-2k-upgrade.md`（勾选完成项）

**Interfaces:**
- Consumes: Task 0–8 的全部产出。

- [ ] **Step 1: 全量测试**

Run:
```bash
gofmt -w $(git diff --name-only -- '*.go')
go vet ./plugin/webrtc/...
go test ./plugin/webrtc/... -count=1
go test ./test -count=1
```
Expected: 全绿（既有失败需先确认基线一致）。

- [ ] **Step 2: 更新文档**

README_CN 增加 `cc` 配置说明与回滚方式；设计文档第 5 节逐项勾选验收表。

- [ ] **Step 3: 提交并开 PR**

```bash
git add -A
git commit -m "docs(webrtc): 2K@24 升级说明与验收结论"
```

PR 描述包含：版本表、2K@24 前后对比、RTSP/录制结论、`cc.enable=false` 回滚方式。

---

## 执行记录（2026-11-18）

> 本节是计划完成情况的记录；上方各 Task 的步骤复选框保持原样，以本表为准。

| Task | 提交（范围） | 状态 | 交付 / 待办 |
|---|---|---|---|
| 0 基线测量脚本与数据 | `ef4842ae` | DONE | `example/webrtc-2k-perf/`：`push-2k.sh`、`play-collect.js`、`report-baseline.md`；基线 24.00fps / 0 丢帧（pre-upgrade pion v4.1.4，ultrafast） |
| 1 依赖升级 | `678ca0dd` | DONE | pion/webrtc v4.2.26、interceptor v0.1.51、rtp v1.10.6、rtcp v1.2.20（含 sdp/testify 传递升级）；`go build ./...` 通过 |
| 2 GCC 拥塞控制 | `96be596a`、`241517a0` | DONE | `CCConfig` + `cc.enable` 开关；`default.yaml` 默认 2M/300k/20M；单测钉住注册顺序与非零默认值 |
| 3 发送端可观测性 | `ebc1503c` | DONE | `StreamMetrics`、每连接 estimator、每 10s `webrtc stats`（target/send/packets/lost/rtt/jitter/pli）；`metrics_test.go` |
| 4 PLI 按需转发 + 清理空转 RTCP | `5bec3f37`、`4df526c5`、`6448b9bf` | DONE | 按 Publisher 的 `PLIThrottler`（500ms 硬窗口 + 5s 无 IDR 调用方触发）；观众 PLI/严重丢包转发；旧固定 2s PLI 删除；两处 RTCP 读循环保留并集中到 `handleViewerRTCP`（删除会丢观众 PLI），空 switch 已消除；`pli_test.go` |
| 5 RTSP 拉流兼容性 | `724263e3`、`170fe160`、`f59bb3ac` | DONE_WITH_CONCERNS | 单路 2K 默认 GCC 5 次 60s 运行：4 次复跑 **22.71–23.65fps**（首跑为冷启动）、稳态 24.0、重连 0.38s；TWCC header extension 修复（默认 GCC 出包阻断）；**10 路并发 / 10 分钟 / 真实摄像头 PENDING** |
| 6 录制兼容性 | `4bb6a03c`、`53e06d6b`、`db205192` | DONE_WITH_CONCERNS | A11 **PASS（短测）**、A13 PASS（3 片 60.300/59.957/54.832s）；**A12 录制端 Opus 阻断**（progressive 拒绝 `*codec.OPUScTx`、fMP4 panic）；10 分钟长录/真实摄像头/弱网/10 路并发 PENDING |
| 7 2K@24 功能与弱网验收 | `fa63c113` | DONE_WITH_CONCERNS | 本机短测：A1 120s 平均 20.70fps（t≥80s 23.88）、A1b 30.00fps/60s、A8 24.00fps/0 丢帧、A7 target/send 通过（rtt/jitter/pli 字段已补齐）；**A1 30min、A1c/A2/A3 弱网、A6 AAC→Opus、P95 延迟 PENDING** |
| 8 观众数压测 | `f24f93b8` | DONE_WITH_CONCERNS | 10 观众：服务端 ≈0.50 核、出向 101 Mbps、订阅者恒 10、NACK 0；**A4 16 核口径 / A5 50 观众 PENDING**；发现既有 `/api/metrics` 视频-only 流 panic |
| 9 回归、文档与 PR 准备 | 本次提交 `docs(webrtc): 2K@24 升级说明与验收结论` | DONE | `README_CN.md` 增补 cc/PLI/可观测性/2K 起播/WHIP Opus/已知限制；设计文档 §5 验收结论与 §6/§7 风险决定；本执行记录。**不推送、不开 PR（由控制器决定）** |

**验收结果汇总（A1–A13）**：PASS = A11、A13；MEASURED（本机短测）= A8、A9、A10；PARTIAL = A1b、A4、A6、A7；PENDING = A1（30min）、A1c、A2、A3、A5；BLOCKED = A12（录制端 Opus）。逐项证据见 `example/webrtc-2k-perf/report-{baseline,rtsp-pull,record,after,load}.md`。

**已知阻断 / 边界（必须与验收结论一起读）**：

- **Ruling 10（A12 产品阻断，本期不改录制器）**：Monibuca MP4 录制器不能写 Opus 音轨——progressive 报 `unsupported audio codec for mp4 record: *codec.OPUSCtx` 并每秒重试（只得到 1s 纯视频碎片），fMP4 在 `CreateOpusSpecificBox → ParseExtranData` panic；DB 列表误报 `audioCodec:"opus"`。WHIP H264+Opus 推流本身通过；Opus-in-MP4 播放能力仅用 ffmpeg 合成样本验证（ffplay/VLC/Chrome 可播），修复方向见设计文档 §6。
- **Ruling 11（既有缺陷，本期不改代码）**：`/api/metrics` 对视频-only 流（`AudioTrack == nil`）在 `prometheus.go:82` 解引用 `stream.AudioTrack.AVTrack.BPS` 触发 nil pointer panic，Prometheus 抓取会杀死整个进程；崩溃样本 `/tmp/webrtc-2k-load/fatal/2026-11-18 22:38:10.log`。该缺陷先于本次升级存在，建议单独任务加 nil 判空 + 单测，压测期间改用 `/api/stream/list`。

## Self-Review

- **Spec coverage**：G1→Task 1；G2→Task 2；G3（含 1080p 保底 A1b/A1c）→Task 7；G4→Task 7；G5→Task 3；G6→Task 7/9；G7→Task 5/6；4.6 的 RTSP/录制/WebRTC 流录制→Task 5/6；验收 A1–A13→Task 5–8/9。
- **Step scan**：代码步骤给到签名/断言；测试步骤给到失败预期；验证任务给到命令与通过条件。
- **Type consistency**：`CCConfig`、`createInterceptorRegistry`、`StreamMetrics`、`PLIThrottler` 在 Task 2/3/4 内自洽，后续任务只引用其 Interfaces。
- **Review Focus**：5 条均落到 Task 2/5/6/7/9 的测试或验证步骤。
- **Proportion**：计划未展开实现体，只保留签名、断言与命令。
