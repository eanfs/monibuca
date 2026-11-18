# WebRTC 2K@24 稳定播放：pion 升级与性能设计

日期：2026-11-18
状态：待批准
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
- G3：2K@24 稳定播放验收：单观众连续 30 分钟，帧率 ≥ 23.5fps，无连续花屏，端到端延迟 P95 ≤ 1.2s。
- G4：弱网可恢复：1% 与 3% 丢包、RTT 50ms/150ms 下，恢复时间 ≤ 3s，不出现永久卡死。
- G5：可观测：目标码率、实际发送码率、丢包、RTT、关键帧请求次数可量化（日志 + stats）。
- G6：升级不破坏现有能力：WHIP 推流、WHEP 播放、batchv2 多流、TCP 模式、H264/H265/AV1 直通、AAC→Opus 降级。

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

v4.2.26 的 `RegisterDefaultInterceptors` 已包含 TWCC 发送侧（transport-cc 扩展 + feedback 注册 + sender interceptor），
但**不含带宽估计与 pacer**。需要额外接入 `pion/interceptor/pkg/cc` + `gcc`。

设计：

- 在 [plugin/webrtc/pkg/util.go](plugin/webrtc/pkg/util.go) 的 `CreateAPI` 中改为自定义注册顺序：

```
1. cc.NewInterceptor(gcc.NewSendSideBWE)   // GCC，必须在 NACK 之前
2. RegisterDefaultInterceptors(m, i)        // NACK / RTCP report / TWCC sender / stats
```

- GCC 配置项（新增到 `WebRTCPlugin`，YAML 小写）：

| 配置 | 默认 | 说明 |
|---|---|---|
| `cc.enable` | `true` | 关闭时回退到当前行为 |
| `cc.initialbitrate` | `2000000` | 初始目标码率 2 Mbps，快速爬升到 2K 码率 |
| `cc.minbitrate` | `300000` | 下限，防止完全停发 |
| `cc.maxbitrate` | `20000000` | 上限，覆盖 2K H264 8–12 Mbps + 余量 |

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

## 5. 验收标准

| 编号 | 场景 | 通过条件 |
|---|---|---|
| A1 | 单观众 2K@24 H264，0% 丢包，30 分钟 | 帧率 ≥ 23.5fps，无连续 >1s 花屏，延迟 P95 ≤ 1.2s |
| A2 | 单观众 2K@24，1% 丢包，10 分钟 | 无永久卡死；恢复 ≤ 3s；帧率平均 ≥ 22fps |
| A3 | 单观众 2K@24，3% 丢包，10 分钟 | 无永久卡死；恢复 ≤ 5s；画面可辨（允许短暂花屏） |
| A4 | 10 观众同看一路 2K | 服务端 CPU（16 核基准）≤ 50%；每观众出向 ≈ 码率 |
| A5 | 50 观众（batchv2） | 无连接崩溃；CPU 可接受；日志无错误风暴 |
| A6 | 既有功能回归 | WHIP/WHEP/batchv2/TCP/H265/H264/AV1 全部可用 |
| A7 | 可观测 | 日志能读出目标码率与实际码率，弱网时目标码率下降 |
| A8 | 回滚 | `cc.enable=false` 后行为与升级前一致 |

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

回滚策略：

- 代码回滚：revert 本次 PR。
- 配置回滚：`cc.enable=false` 单点关闭拥塞控制，不重启进程（若实现为启动期配置则需重启）。
- 版本回滚：依赖版本单独 revert 到 v4.1.4（v4.2 新特性未使用时可安全回退）。

## 7. 已做的决定（实现者按此执行）

- 升级到 **v4.2.26 稳定版**，不跟 v5.0.0-rc。
- **H264 作为 2K@24 主路径**（浏览器兼容），H265 保留给 Safari/自定义客户端。
- GCC **默认开启**，提供 `cc.enable` 开关与三档码率配置。
- 验收以 **本机 + tc netem + Chrome getStats** 为主，真机部署冒烟作为发布前检查。
- PLI 节流窗口 **500ms**，兜底 **5s**。
- 不新增对外 stats API，先日志观测。

## 8. 留给实现者的判断

- v4.2.26 的 `stats` 拦截器是否与现有 `io.Copy` 风格冲突（实测）。
- 空转 RTCP 循环能否直接移除（需确认 pion 不读 RTCP 是否会导致发送阻塞）。
- GCC 在 TCP mux 模式下的行为（TCP 不是典型 WebRTC 传输，验收时单独看）。
- 2K 源的 GOP 长度对恢复时间的影响（编码参数，不在服务端范围）。
