# WebRTC 2K@24 稳定播放：pion 升级与性能设计

日期：2026-11-18
状态：已实现（本机短测；30min/弱网/真机项待验收；A12 录制端 Opus 阻断）
作者：Lead（brainstorming 产出）

## 1. 背景

WebRTC 插件当前使用 `pion/webrtc/v4 v4.1.4`（[go.mod:40](go.mod#L40)），是典型 SFU：
服务端不转码，把 Publisher 的 RTP 包逐包转发给每个订阅者的 `TrackLocalStaticRTP`。

业务目标：**2K（2560×1440）@24fps 稳定播放**。当前实现有三个与高码率/弱网直接相关的问题：

1. **没有发送端拥塞控制**。[plugin/webrtc/pkg/util.go:53-66](plugin/webrtc/pkg/util.go#L53-L66) 只调用了
   `RegisterDefaultInterceptors`，它提供 NACK / RTCP 报告 / TWCC 发送侧扩展，但**不包含 GCC 带宽估计器**。
   2K H264 码率通常 8–12 Mbps，弱网时服务端不会降码率，客户端只能丢包 → 花屏、卡顿、延迟累积。
2. **固定 2 秒 PLI 风暴**。[plugin/webrtc/pkg/connection.go:195-201](plugin/webrtc/pkg/connection.go#L195-L201)
   接收侧无条件每 2s 向 Publisher 发 PLI，与观众是否丢包无关；2K 关键帧约 200–400 KB，频繁关键帧既浪费带宽又放大卡顿。
3. **空转的 RTCP 读循环**。[plugin/webrtc/pkg/connection.go:313-330](plugin/webrtc/pkg/connection.go#L313-L330) 与
   [connection.go:542-559](plugin/webrtc/pkg/connection.go#L542-L559)：每个 sender 一个 goroutine + 1500B buffer 读 RTCP，
   但 PLI 分支是空实现。

同时上游已有 22 个 minor 版本的新修复（v4.1.4 → v4.2.26，2026-10-28），包括竞态修复、TCP mux 4MB 写缓冲、
SCTP RACK（DataChannel 吞吐 +71%）、stats 拦截器等。v5.0.0-rc 是不兼容预发布，本次不跟进。

## 2. 目标 / 非目标 / 反目标

### 目标

- G1：依赖升级到 `pion/webrtc/v4 v4.2.26`、`pion/interceptor v0.1.51`、`pion/rtp v1.10.6`、`pion/rtcp v1.2.20`。
- G2：启用 GCC 发送端拥塞控制（TWCC 反馈 → 目标码率 → pacer），使 2K@24 在弱网下不持续花屏。
- G3：2K@24 稳定播放验收：单观众连续 30 分钟，帧率 ≥ 23.5fps，无连续花屏，端到端延迟 P95 ≤ 1.2s；**1080p 源保底 24–30fps**（正常网络 ≥ 29fps，1%/3% 丢包下平均 ≥ 24fps）。
- G4：弱网可恢复：1% 与 3% 丢包、RTT 50ms/150ms 下，恢复时间 ≤ 3s，不出现永久卡死。
- G5：可观测：目标码率、实际发送码率、丢包、RTT、关键帧请求次数可量化（日志 + stats）。
- G6：升级不破坏现有能力：WHIP 推流、WHEP 播放、batchv2 多流、TCP 模式、H264/H265/AV1 直通、AAC→Opus 降级。
- G7：RTSP 拉流与 MP4 录制（含 RTSP 源与 WebRTC 推流源）在升级后功能不回归。

### 非目标

- 不升级到 `v5.0.0-rc`（不兼容预发布）。
- 不引入服务端转码（现有 AAC→Opus 例外保留）。
- 不做 simulcast / SVC / 分层编码。
- 不改 batchv2 的 WebSocket 信令协议。
- 不做多 UDPMux / SO_REUSEPORT 分片（列为后续可选优化，不在本期）。

### 反目标（即使技术上成立也算失败）

- 为了 2K 牺牲 H264 主路径兼容性（Chrome/Firefox 必须可用）。
- 默认配置下改变既有播放行为语义（除新增拥塞控制外，SDP/码率/编码协商不变）。
- 引入新的裸 goroutine 或绕过 task 生命周期。
- 用调大缓冲/加大延迟的方式“稳定”画面。

## 3. 现状架构（关键事实）

- 框架：`pion/webrtc v4`，纯 Go；ICE 使用共享 UDP/TCP mux（[plugin/webrtc/index.go:76-124](plugin/webrtc/index.go#L76-L124)）。
- 播放模式：
  - `/webrtc/play/{streamPath}`：每观众一个 PeerConnection（`MultipleConnection`，[api.go:78-132](plugin/webrtc/api.go#L78-L132)）。
  - `/webrtc/batchv2`：一个 PeerConnection 承载多路流（`SingleConnection` + `WorkCollection`，[batchv2.go:19-426](plugin/webrtc/batchv2.go#L19-L426)）。
- 转发路径：`m7s.PlayBlock` → `frame.Packets.RangePoint` 逐包 → `TrackLocalStaticRTP.WriteRTP` → 拦截器链 → SRTP → ICE。
- 已有优化：`DisableSRTPReplayProtection(true)`、共享 mux、H265 客户端支持检测。
- 音频：Opus/PCMA/PCMU 直通；MP4A 源转 Opus（[connection.go:332-462](plugin/webrtc/pkg/connection.go#L332-L462)）。
- 视频：H264/H265/AV1 直通；H265 客户端不支持时降级 DataChannel 传 FLV。

## 4. 方案

### 4.1 依赖升级（G1）

| 模块 | 当前 | 目标 |
|---|---|---|
| `github.com/pion/webrtc/v4` | v4.1.4 | **v4.2.26** |
| `github.com/pion/interceptor` | v0.1.40 | **v0.1.51** |
| `github.com/pion/rtp` | v1.8.21 | **v1.10.6** |
| `github.com/pion/rtcp` | v1.2.15 | **v1.2.20** |

步骤：

1. `go get` 四个模块到目标版本，`go mod tidy`。
2. `go build ./...`，点导入（`. "github.com/pion/webrtc/v4"`）会让 API 变化直接暴露。
3. 重点核对在用 API：`CreateAPI`、`RegisterDefaultInterceptors`、`SetICEUDPMux`/`SetICETCPMux`、
   `SetNAT1To1IPs`、`SetEphemeralUDPPortRange`、`SetNetworkTypes`、`DisableSRTPReplayProtection`、
   `TrackLocalStaticRTP`、`GatheringCompletePromise`。
4. 如果 v4.2.x 有破坏性变化，优先改适配层（`plugin/webrtc/pkg`），不改业务语义。

### 4.2 启用 GCC 拥塞控制（G2）

v4.2.26 的 `RegisterDefaultInterceptors` 会注册 TWCC 的 SDP 能力/feedback 处理与发送侧 sender
interceptor（另含 stats、NACK、RTCP report），但**不写 RTP 包的 transport-cc header extension**，
也**不含带宽估计与 pacer**。头扩展写入需要单独调用 `ConfigureTWCCHeaderExtensionSender`
（否则 pacer 丢包，见下方实现修正），带宽估计与 pacer 则需接入 `pion/interceptor/pkg/cc` + `gcc`。

设计：

- 在 [plugin/webrtc/pkg/util.go](plugin/webrtc/pkg/util.go) 的 `CreateAPI` 中改为自定义注册顺序：

```
1. cc.NewInterceptor(gcc.NewSendSideBWE)    // GCC，必须在 NACK 之前
2. ConfigureTWCCHeaderExtensionSender(m, i) // 写入 transport-cc 头扩展（pacer 依赖）
3. RegisterDefaultInterceptors(m, i)        // NACK / RTCP report / TWCC sender / stats
```

- GCC 配置项（新增到 `WebRTCPlugin`，YAML 小写）：

| 配置 | 默认 | 说明 |
|---|---|---|
| `cc.enable` | `true` | 关闭时回退到当前行为 |
| `cc.initialbitrate` | `2000000` | 初始目标码率 2 Mbps，快速爬升到 2K 码率 |
| `cc.minbitrate` | `300000` | 下限，防止完全停发 |
| `cc.maxbitrate` | `20000000` | 上限，覆盖 2K H264 8–12 Mbps + 余量 |

> **实现修正（2026-11-18，`170fe160`）**：只注册 cc + 默认拦截器时，GCC pacer 在 `OnSent`
> 读不到 transport-wide-cc 扩展，会以 `missing transport layer cc header extension` 丢弃全部出站包
> （ICE 已连接但画面完全不出帧）。最终注册顺序为 **cc → TWCC header extension sender
> （`ConfigureTWCCHeaderExtensionSender`）→ `RegisterDefaultInterceptors`**，由
> `plugin/webrtc/pkg/util_test.go` 回归钉住；详见 §6/§7。

- 通过 `ccInterceptor.OnNewPeerConnection` 保存每个 PC 的 `BandwidthEstimator`，供可观测性使用
  （目标码率、变化轨迹）。
- 若 GCC 在某些客户端/网络下收敛过慢或误判，可通过 `cc.enable=false` 单点回退，不需要回滚版本。

### 4.3 2K@24 发送路径调优（G3/G4）

1. **PLI 按需转发**（替代固定 2s）：
   - 接收侧不再无条件发 PLI；把订阅者的 PLI / 严重丢包（NACK 失败）转成对 Publisher 的关键帧请求。
   - 合并与节流：同一 Publisher 500ms 内最多一次 PLI，避免多观众同时请求造成关键帧风暴。
   - 保留一个兜底：长时间无关键帧（> 5s）时才强制请求一次。
2. **移除空转 RTCP goroutine**：
   - 现有 `videoSender.Read(rtcpBuf)` 循环空 switch；改为集中处理或直接移除（pion 内部已有 RTCP 需要消费的路径时，需确认不读是否会阻塞——实现时先验证，若必须读则合并到既有任务中）。
3. **NACK 重传**：使用 v4.2.26 默认 NACK responder，确认 GOGC/缓冲默认值；2K 关键帧较大，验证重传不会造成内存尖峰。
4. **不做**：不调大 pion 发送缓冲来换“稳定”（违反反目标）。

### 4.4 可观测性（G5）

- v4.2.26 默认注册 stats interceptor，可通过 `pc.GetStats()` 读取发送统计。
- 新增周期日志（每 10s，Debug/Info 级别可配）：每个订阅者输出
  `target_bitrate`、`send_bitrate`、`packets_sent`、`packets_lost`、`rtt`、`jitter`、`pli_requests`。
- 不新增对外 API（避免扩大范围）；如后续需要，可在 `/webrtc/api/stats` 单独提案。

### 4.5 测试与验收

1. **单元测试**：
   - `CreateAPI` 在 cc.enable=true/false 下的拦截器注册数量与顺序。
   - PLI 节流器：500ms 窗口内多次请求只发一次；无关键帧超时兜底触发。
   - 配置解析：cc 默认值、0 值处理。
2. **集成测试（本机）**：
   - ffmpeg 推 2560×1440@24 H264（8–12 Mbps 目标）到 RTMP/RTSP，WebRTC 播放。
   - 用浏览器（Chrome）+ 脚本采集：`getStats()` 的 framesDecoded、framesDropped、jitterBufferDelay、nackCount。
3. **弱网测试**：`tc netem` 在回环/网卡上加 1%/3% 丢包、50ms/150ms RTT，连续 10 分钟。
4. **压测**：单路 2K + 10/50 观众（batchv2 单 PC 多流与标准模式各一轮），记录服务端 CPU/内存/出向带宽。
5. **回归**：H264/H265/AV1、AAC→Opus、WHIP 推流、TCP 模式、batchv2 多流。

### 4.6 兼容性测试范围（RTSP 拉流与录制）

升级改动集中在 webrtc 插件的依赖与发送路径，但 WebRTC 播放常与拉流、录制串联，必须覆盖：

1. **RTSP 拉流**：
   - 2K@24 RTSP 源 → 拉流代理 → WebRTC 播放（直通、帧率正常）；
   - 10 路并发拉流，观察 CPU/内存/连接状态；
   - 断流重连：源消失 5s 后恢复，确认拉流代理自动重连、观众恢复。
2. **录制（RTSP 源）**：
   - 2K@24 拉流 → MP4 录制 10 分钟 → `ffprobe` 校验分辨率/帧率/时长/音轨；
   - 分片录制（`fragment=1m`）3 分钟，检查分片数量与每片可播；
   - 录制中停止源，文件正常收尾（moov 写入、无零字节文件）。
3. **录制（WebRTC 流）**：
   - WHIP 推流（H264 + Opus 音频）→ MP4 录制 5 分钟 → `ffprobe` 校验；
   - 录制器已支持 Opus（`plugin/mp4/pkg/record.go:1162`、`track.go:213`），但 **Opus-in-MP4 在浏览器播放器支持有限**：本次记录 VLC/Chrome 的实际播放结果，若 Chrome 不支持则作为已知限制写入文档，**不修改录制器行为**；
   - WebRTC 推 H265 → 录制为可选用例（依赖客户端支持，失败记录原因）。
4. **与录制完整性联动**（可选）：强杀进程后重启，确认 sidecar 恢复逻辑对 WebRTC/RTSP 源录制产物生效。

## 5. 验收标准

| 编号 | 场景 | 通过条件 | 结果 / 状态（2026-11-18） |
|---|---|---|---|
| A1 | 单观众 2K@24 H264，0% 丢包，30 分钟 | 帧率 ≥ 23.5fps，无连续 >1s 花屏，延迟 P95 ≤ 1.2s | **PENDING**：120s 短测平均 20.70fps（277 丢帧），t≥30s 21.67fps，t≥80s **23.88fps**；直连 RTMP 冷启动约 80s（`initialbitrate=2 Mbps` vs 源 ~10 Mbps）。30min 与端到端 P95 未测，不判通过（`report-after.md`） |
| A1b | 1080p@30 源，0% 丢包，10 分钟 | 平均帧率 ≥ 29fps（保底 24fps 不跌破） | **PARTIAL**：60s 平均 **30.00fps**、0 丢帧/0 NACK/0 PLI、jitter 13.9ms；10 分钟长稳 PENDING |
| A1c | 1080p@30 源，1%/3% 丢包，各 10 分钟 | 平均帧率 ≥ 24fps（保底），无持续低于 20fps 的区间 | **PENDING**：本机 macOS 无 `tc netem`；Linux 命令见 `report-after.md` §6 |
| A2 | 单观众 2K@24，1% 丢包，10 分钟 | 无永久卡死；恢复 ≤ 3s；帧率平均 ≥ 22fps | **PENDING**：同上（弱网需 Linux `tc netem`） |
| A3 | 单观众 2K@24，3% 丢包，10 分钟 | 无永久卡死；恢复 ≤ 5s；画面可辨（允许短暂花屏） | **PENDING**：同上 |
| A4 | 10 观众同看一路 2K | 服务端 CPU（16 核基准）≤ 50%；每观众出向 ≈ 码率 | **PARTIAL**：10 观众短测（2×60s）服务端 **≈0.50 核**（12 逻辑核的 4.2%）、出向 **101 Mbps**、订阅者恒 10、NACK 0；16 核真机 ≤50% 与观众分机 PENDING（`report-load.md`） |
| A5 | 50 观众（batchv2） | 无连接崩溃；CPU 可接受；日志无错误风暴 | **PENDING**：仅完成 batchv2 2 路流 sanity（通路可用）；50 观众需真机 + 分布式客户端 |
| A6 | 既有功能回归 | WHIP/WHEP/batchv2/TCP/H265/H264/AV1 全部可用 | **PARTIAL**：H264 2K 播放通过；WHIP H264+Opus 推流通过（Task 6）；AAC→Opus **不可用**（缺 `aac_opus_transcode` 标签与 `3rd/ffmpeg6` 库）；batchv2 仅 2 路流 sanity（通路可用，性能未验证）；H265/AV1/TCP/WHEP **未测**（功能与性能均未验证） |
| A7 | 可观测 | 日志能读出目标码率与实际码率，弱网时目标码率下降 | **PARTIAL**：`global.loglevel: debug` 每 10s 输出 `webrtc stats`（target/send bitrate 等）已实测；弱网 target 下降 PENDING |
| A8 | 回滚 | `cc.enable=false` 后行为与升级前一致 | **MEASURED**：60s **24.00fps / 0 丢帧 / 0 NACK/PLI / jitter 8.5ms**，与升级前基线一致；运行时 `target_bitrate=0`；30min cc-off 长稳未做 |
| A9 | RTSP 拉流单路 2K | 拉流状态稳定，WebRTC 播放帧率 ≥ 23.5fps | **MEASURED（短测）**：默认 GCC 5 次 60s 运行——首跑 17.91fps（start 后立即测量的冷启动，§4.1.4），4 次复跑 **22.71–23.65fps**（最好 23.65fps、1301 帧/55s）；t≥25s 稳态 **24.0fps**（四次复跑 23.83–24.14）、0 包丢失；10 路并发 / 10 分钟 PENDING（`report-rtsp-pull.md` §4.1.3） |
| A10 | RTSP 拉流重连 | 源恢复后 ≤ 5s 重新出流，无重连风暴 | **MEASURED（短测）**：源恢复后 **0.38s** 拉流可用、代理始终 Pulling；观众跨断流连续性 PENDING |
| A11 | RTSP 源录制 | ffprobe 分辨率/帧率/时长/音轨正确，文件可播 | **PASS**：视频 2560×1440 h264 **24.072fps** / **25.132s**；AAC 音轨 48000Hz / 18.049s（`report-record.md`） |
| A12 | WebRTC 流录制 | WHIP H264+Opus → MP4 ffprobe 正常；Opus-in-MP4 播放器兼容性结论已记录 | **BLOCKED（录制端）**：WHIP H264+Opus 推流通过；MP4 录制 Opus 失败——progressive 报 `unsupported audio codec for mp4 record: *codec.OPUSCtx`（每秒 1 个纯视频碎片），fMP4 在 `CreateOpusSpecificBox` panic；播放端（ffplay/VLC/Chrome）对合成 Opus-in-MP4 样本可播，瓶颈在 Monibuca 封装 |
| A13 | 分片录制 | `fragment=1m` 分片数量与时长正确，每片可播 | **PASS**：175s → **3 片**（按时间 **60.300 / 59.957 / 54.832s**），逐片 ffprobe 可读；最后一片为手动停止的残余（合计 175.089s） |

2K 码率假设（写入验收脚本参数，可按实际源调整）：

- H264 2560×1440@24：8–12 Mbps（x264 veryfast，CRF 20–23）
- H265 2560×1440@24：5–8 Mbps

## 6. 风险与回滚

| 风险 | 影响 | 缓解 |
|---|---|---|
| v4.2.x API 不兼容 | 编译失败 | 点导入暴露；适配层修改；不改变业务语义 |
| GCC 在高码率（8–12 Mbps）收敛慢 | 起播延迟/画质波动 | initialbitrate 2 Mbps + 快速爬升；maxbitrate 20 Mbps；实测调参 |
| GCC 误判导致码率过低 | 2K 画质下降 | 可配置 minbitrate；监控目标码率；必要时 cc.enable=false |
| 关键帧加大导致弱网重传成本高 | 恢复变慢 | PLI 节流 + NACK；必要时限制 GOP 长度（编码端） |
| 浏览器 H265 不支持 | 2K H265 无法播放 | 主路径 H264；H265 仅 Safari/自定义客户端 |
| stats interceptor 增加开销 | 少量 CPU | 默认开启，压测确认；必要时关闭 |
| 漏注册 TWCC header extension（或顺序错误） | GCC pacer 丢弃全部出站包：ICE 已连接但视频完全不出帧（实测单次累计 73,327 条 `missing transport layer cc header extension`） | 固定 **cc → TWCC header extension → 默认拦截器** 顺序；`util_test.go` 钉住链上前两个拦截器；排障时 `grep -c "pacer ERROR"` 必须为 0 |
| PLI 无节流/多观众放大 | 关键帧风暴，浪费带宽并放大卡顿 | 按 Publisher 共享 `PLIThrottler`：500ms 硬窗口；5s 无 IDR 是调用方触发条件，不绕过硬窗口；非 WebRTC Publisher 无 PLI 通道时静默跳过 |
| 2K 起播冷启动（默认 initialbitrate 2 Mbps vs 源 ~10 Mbps） | 起播阶段帧率下探、jitter buffer 升高；直连 RTMP 实测约 80s 才回稳态（RTSP 路径约 20–25s） | 生产 2K 可评估 `initialbitrate` 8–10 Mbps（注意弱网权衡）；默认值不变；30 分钟长稳验收摊薄该窗口 |
| A12：MP4 录制不支持 Opus | WHIP H264+Opus 流无法录出带音轨的 MP4；progressive 每秒生成 1s 纯视频碎片，fMP4 panic；DB 元数据误报 `audioCodec=opus` | 本期不改录制器，记录为已知限制；录制端 Opus 支持另开任务（修 `record.go` 音频分支与 `box/dops.go` dOps 构造）；修复前调用方不要把 `audioCodec` 当真 |
| 既有缺陷：`/api/metrics` 在视频-only 流上 panic | `prometheus.go:82` 访问 nil 的 `AudioTrack.AVTrack.BPS`，Prometheus 抓取线程 panic 会杀死整个进程 | Ruling 11：该缺陷先于本次升级存在、不在 A1–A13 范围，本期不修；部署中避免对视频-only 流抓取 `/api/metrics`；建议单独任务加 nil 判空 + 单测 |

回滚策略：

- 代码回滚：revert 本次 PR。
- 配置回滚：`cc.enable=false` 单点关闭拥塞控制（配置在插件启动期读取，需重启进程；也可用 `WEBRTC_CC_ENABLE=false` 环境变量覆盖）。
- 版本回滚：依赖版本单独 revert 到 v4.1.4（v4.2 新特性未使用时可安全回退）。

## 7. 已做的决定（实现者按此执行）

- 升级到 **v4.2.26 稳定版**，不跟 v5.0.0-rc。
- **H264 作为 2K@24 主路径**（浏览器兼容），H265 保留给 Safari/自定义客户端。
- GCC **默认开启**，提供 `cc.enable` 开关与三档码率配置。
- **1080p 保底 24–30fps 作为独立验收档位**：正常网络保持 30fps；弱网（1%/3% 丢包）下平均帧率不得低于 24fps。前提是可用带宽 ≥ 2.5 Mbps（1080p H264 24fps 约需 1.5–2 Mbps）；`cc.minbitrate=300000` 仅作“不中断”保护，不承诺物理带宽不足时的保底。
- 验收以 **本机 + tc netem + Chrome getStats** 为主，真机部署冒烟作为发布前检查。
- PLI 节流窗口 **500ms**，兜底 **5s**。
- 不新增对外 stats API，先日志观测。
- 录制兼容性以“**不回归 + 记录限制**”为目标：若 Opus-in-MP4 在 Chrome 不可播，只记录为已知限制，不为它修改录制器。
- RTSP 拉流测试复用现有拉流代理与 API，不新增拉流功能。
- **GCC 拦截器顺序固定为 cc → TWCC header extension → 默认拦截器**：TWCC header extension 是默认配置下正常出帧的必要条件（缺它 pacer 丢包），任何后续改动不得移除或后置该注册。
- **PLI 节流按 Publisher 共享**：500ms 硬最小间隔；5s 无 IDR 是调用方触发条件，不是绕过 500ms 的旁路。旧 `webrtc.pli` 配置字段保留兼容但已废弃、不再生效。
- **2K 默认 `initialbitrate` 保持 2 Mbps**：生产可评估 8–10 Mbps 缩短起播，但弱网表现需实测；A1 的 30 分钟长稳仍待真机验收。
- **A12 录制端 Opus 阻断记录为已知限制**：本期不改录制器；修复前不宣称 WHIP Opus 可录制，Opus-in-MP4 播放能力仅按合成样本记录。
- **`/api/metrics` 视频-only 流 panic 属升级前既有缺陷（Ruling 11）**：不在本期范围、不修改 `prometheus.go`；仅记录精确位置/复现并建议独立 nil-guard 修复任务。

## 8. 留给实现者的判断

- v4.2.26 的 `stats` 拦截器是否与现有 `io.Copy` 风格冲突（实测）。
- 空转 RTCP 循环能否直接移除（需确认 pion 不读 RTCP 是否会导致发送阻塞）。
- GCC 在 TCP mux 模式下的行为（TCP 不是典型 WebRTC 传输，验收时单独看）。
- 2K 源的 GOP 长度对恢复时间的影响（编码参数，不在服务端范围）。
- WebRTC 推流（Opus 音频）录制出的 MP4 在目标客户端的实际可播性（需实测并记录）。
- RTSP 拉流断流重连时，WebRTC 订阅者是否需要重连（现有语义下应自动恢复，实测确认）。
