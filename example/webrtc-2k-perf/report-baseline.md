# WebRTC 2K@24 升级前基线报告

- 采集日期：2026-11-18（本机时间 UTC+8）
- 被测版本：**pion/webrtc v4.1.4（升级前）**，`pion/interceptor v0.1.40`、`pion/rtp v1.8.21`、`pion/rtcp v1.2.15`
- 用途：作为 Task 7/8（A1–A3、A9）验收的对照基线；Task 1 升级到 pion v4.2.26 后可用同一套脚本复测。

## 1. 环境

| 项 | 值 |
|---|---|
| 主机 | macOS 26.7.3（Build 25G342），Intel Core i7-9750H @ 2.60GHz，12 逻辑核，16 GB RAM |
| 服务端 | 仓库 `example/default`，`go run -tags sqlite main.go`，Go 1.27.4 darwin/amd64 |
| 服务端版本 | 工作树 `dsh-39c7ba14-fa26`，HEAD `ef196cc8`（pion v4.1.4） |
| HTTP / RTMP / WebRTC | `:8080` / `:1935` / `udp:9000-9100` |
| 推流端 | ffmpeg 7.1.1（libx264），`testsrc2` 合成源 |
| 播放端 | Google Chrome 158.0.8127.36（Playwright 1.56.0，`channel: 'chrome'`，headless） |
| 网络 | 全本机回环（127.0.0.1），无 `tc netem` |

说明：非 root 无法绑定 RTSP 默认的 `:554/:322`，服务端用环境变量覆盖为
`RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322` 启动；本次不涉及 RTSP 路径。

启动日志中有一条与 WebRTC 无关的错误：
`S3 unavailable, recording disabled until recovery (errorCategory=unsupported_build)` ——
`example/default` 的配置里写了 S3，但本次按计划只用 `-tags sqlite` 构建，未带 `s3` tag。
不影响播放路径；后续 Task 6 若要在该配置下录制，需要补 `s3` 构建标签或移除 S3 配置。

## 2. 方法

### 2.1 服务端

```bash
cd example/default
RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go
```

服务端在 17:54:04 启动并就绪（`GET /api/stream/list` 返回 200）。

### 2.2 推流

脚本 `push-2k.sh`（默认参数即计划中的命令）：

```bash
cd example/webrtc-2k-perf
./push-2k.sh
# ffmpeg -re -f lavfi -i testsrc2=size=2560x1440:rate=24 \
#   -c:v libx264 -preset veryfast -b:v 10M -maxrate 12M -bufsize 20M \
#   -g 48 -pix_fmt yuv420p -f flv rtmp://127.0.0.1/live/2k
```

运行 1（主基线，按计划默认参数）：`preset=veryfast`，服务端 17:54:12 收到推流，
推流约 2.5 分钟后 SIGINT 停止（ffmpeg 自己的最终统计：4188 帧 / 174.41 s 内容 / 9974.5 kb/s）。
运行中段 ffmpeg 报 `speed≈0.95x`，编码器是本机实时性的瓶颈。

运行 2（补充基线，消除编码器瓶颈）：`PRESET=ultrafast ./push-2k.sh`，服务端 17:57:24
收到推流，约 1.8 分钟后停止（ffmpeg 最终统计：2933 帧 / 122.20 s 内容 / 9987.3 kb/s；
稳定后 `fps=24`、`speed≈0.99–1x`）。

### 2.3 播放与采集

浏览器打开现有测试播放页：

```
http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k
```

采集用 `play-collect.js`（Playwright 以 `addInitScript` 在文档开始时注入，因此能接管
播放页自己创建的 `pc`；脚本把采样打印为单行 JSON，同时汇总为 summary）：

- 运行 1：采样窗口 90 s（17:55:01–17:56:36），5 s 间隔，18 条采样（首条为基线，fps 从第 2 条起）。
- 运行 2：采样窗口 60 s（17:58:05–17:59:07），5 s 间隔，12 条采样。

### 2.4 服务端 CPU

每 5 s 用 `ps -o %cpu= -p <pid>` 采样服务端进程与 ffmpeg 进程（macOS 上该值按单核百分比计，
100% = 1 核；进程 CPU 是衰减平均值，非瞬时值）。

## 3. 结果

### 3.1 运行 1：主基线（`-preset veryfast`，10 Mbps，90 s）

浏览器 summary（原始 JSON）：

```json
{"type":"summary","elapsedSec":94.6,"intervalMs":5000,"samples":18,"errors":0,
 "peerConnections":[{"pc":1,"connectionState":"connected","samples":18,"spanSec":85,
 "decodedFrames":1968,"avgFps":23.15,"sampleFpsMean":23.15,"sampleFpsMin":21.6,
 "framesDropped":71,"nackCount":0,"pliCount":0,"jitterBufferDelayAvg":0.1238,"lastRtt":0.001}]}
```

（summary 里的 `framesDropped=71` 是“首条采样之后”的增量；连接建立以来累计值见下表。）

| 指标 | 值 |
|---|---|
| 实际分辨率 | 2560×1440（`frameWidth/frameHeight`） |
| 连接状态 | `connected` / ICE `connected`，全程无断连 |
| 平均帧率 | **23.15 fps**（1968 解码帧 / 85 s 采样跨度），单条采样范围 21.6–24.2 fps |
| 丢帧 | 累计 **95** 帧（首 5 s 24 帧，之后 71 帧），约占已交付帧（2090）的 4.5% |
| NACK | **1**（仅出现在连接建立后的首条采样，之后为 0） |
| PLI | **14**（全部出现在首条采样，之后为 0） |
| jitterBufferDelay | 累计 241.5 s；平均 **123.8 ms**（首条 269 ms，稳态 109–119 ms） |
| packetsLost | RTCP 口径累计 **190**（首条采样即 190，之后不变） |
| jitter | 0.011–0.075 s |
| currentRoundTripTime | 0–0.005 s（回环，约 1 ms） |
| 接收码率 | 首末采样间 `bytesReceived` 增量 106,043,965 B / 85.0 s ≈ **9.98 Mbps** |
| 服务端进程 CPU | 平均 **5.2%**（单核百分比），峰值 8.2%；RSS 平均 102 MB（83–119 MB） |
| ffmpeg 编码 CPU | 平均 **151.3%**，峰值 197.4%（i7 上 x264 多线程，约 1.5 核）；RSS 841 MB |

关键限定：`veryfast` 在本机**无法稳定实时编码 2K@24**——ffmpeg 运行中段 `speed≈0.95x`，
靠后段追赶才在总量上回到 1x（最终 4188 帧 / 174.41 s = 24.01 fps，但期间出现突发）。
因此运行 1 的 95 次丢帧与 ~124 ms 抖动缓冲，很可能主要来自**推流端节拍不稳**，
而不是 WebRTC 转发路径本身；这是为运行 2 补一组 `ultrafast` 对照的原因。

### 3.2 运行 2：补充基线（`-preset ultrafast`，10 Mbps，60 s）

浏览器 summary（原始 JSON）：

```json
{"type":"summary","elapsedSec":62.17,"intervalMs":5000,"samples":12,"errors":0,
 "peerConnections":[{"pc":1,"connectionState":"connected","samples":12,"spanSec":55,
 "decodedFrames":1320,"avgFps":24,"sampleFpsMean":24,"sampleFpsMin":23.6,
 "framesDropped":0,"nackCount":0,"pliCount":0,"jitterBufferDelayAvg":0.008,"lastRtt":0.001}]}
```

| 指标 | 值 |
|---|---|
| 实际分辨率 | 2560×1440 |
| 平均帧率 | **24.00 fps**（1320 解码帧 / 55 s），单条采样范围 23.6–24.4 fps |
| 丢帧 | **0**（整个采样窗口） |
| NACK | **1**（仅首条采样） |
| PLI | **8**（仅首条采样） |
| jitterBufferDelay | 累计 11.63 s；平均 **8.0 ms**（稳态 7.5–8.3 ms） |
| packetsLost | 累计 **423**（首条采样即 423，之后不变） |
| jitter | 0.001–0.020 s |
| currentRoundTripTime | 0.001–0.002 s |
| 接收码率 | 68,508,830 B / 55 s ≈ **9.97 Mbps** |
| 服务端进程 CPU | 平均 **5.3%**，峰值 7.1%；RSS 平均 142 MB（117–172 MB，含运行 1 之后的堆增长） |
| ffmpeg 编码 CPU | 平均 **53.1%**，峰值 73.7%；RSS 373 MB |

结论：当推流端能稳定输出 24 fps 时，**升级前的 WebRTC 转发路径在回环 0% 丢包下可以满帧
（24.00 fps、0 丢帧、接收码率 9.97 Mbps、jitter buffer 平均 8 ms）**。这正是 Task 7 复测时
应对比的“干净路径”基线。

### 3.3 启动阶段现象（两组一致）

- 每条连接的首条采样都出现 PLI（14 / 8 次）与 NACK（1 次），5 s 之后不再增加；
  丢帧（运行 1）也集中在启动阶段。符合“订阅者从 P 帧中途接入、需等待关键帧”的表现。
- `packetsLost` 在首条采样即报 190 / 423，之后完全不变——这是 RTCP 接收报告口径的累计值，
  包含首个可解码关键帧之前的包；稳态窗口内没有新增丢包。
- 运行 1 首条采样已解码 27 帧、丢 24 帧；运行 2 首条采样已解码 82 帧、丢 0 帧。

### 3.4 服务端观测

- 服务端 `tick` 日志里的 `cpu=34%`（运行 1）与 70–89%（运行 2）是 **gopsutil
  `cpu.Percent` 的整机 CPU**（含 ffmpeg 编码与 Chrome 解码），不是服务端进程 CPU；
  进程 CPU 以上表的 `ps` 数据为准。
- `/api/stream/list` 的 `videoTrack.bps` / `bpsOut` 在两组运行中稳定在 1.14–1.38×10⁶。
  与浏览器实测 ~9.98 Mbps（≈1.25 MB/s）对照，该字段实际是**字节/秒**（约 9.1–11.0 Mbps），
  不是 bit/s。Task 7 对比时不要把它当 bps 直接使用。
- 服务端日志除启动时那条与 WebRTC 无关的 `S3 unavailable ... unsupported_build` ERR 外，
  没有其它 `ERR`/panic；只有订阅者主动断开时的 `rtcp read error ... EOF` 与
  “stream is already closed” WRN（Playwright 关闭页面导致，属正常收尾）。

## 4. 无法测量 / 明确未覆盖

1. **弱网场景未测**：本机是 macOS，没有 `tc netem`；A1c/A2/A3 的 1%/3% 丢包、RTT 50/150 ms
   必须在 Linux 上做，归 Task 7/8。
2. **长稳验收未做**：本报告只有 90 s / 60 s 采样；设计要求的 10 分钟（A1b/A1c）与
   30 分钟（A1）连续播放仍未执行，属后续任务。
3. **端到端延迟 P95 未测**：`getStats()` 只能给出 `jitterBufferDelay`（本次 8–124 ms），
   拿不到采集到渲染的玻璃到玻璃延迟；需要额外植入时间戳（后续任务可考虑）。
4. **服务端 → Publisher 的 PLI 次数未测**：升级前插件不打印该计数，也没有对应 stats；
   浏览器 `pliCount` 只是“观看端 → 服务端”的 PLI。设计文档所述固定 2 s PLI 的实际次数
   需等 Task 3/4 埋点后才能量化。
5. **服务端发送码率/NACK 重传统计未测**：升级前没有 stats interceptor，服务端没有
   目标码率/实际发送码率指标（这也正是本次升级要补的能力）。
6. **多观众/压测（A4/A5）、batchv2、TCP 模式、H265/AV1、WHIP/WHEP** 均不在 Task 0 范围内。
7. **硬件/软件解码路径未区分**：headless Chrome 是否使用 VideoToolbox 未验证；
   与有头浏览器或低端客户端的绝对帧率可能有差异。
8. 浏览器上报的 `framesDropped` 是接收端 jitter buffer/渲染丢弃，不是网络丢包；
   网络丢包请看 `packetsLost`（本机为 0 新增）。

## 5. 复现命令

```bash
# 1) 服务端
cd example/default
RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go

# 2) 推流（运行 1；运行 2 前置 PRESET=ultrafast）
cd example/webrtc-2k-perf
DURATION=120 ./push-2k.sh

# 3) 播放采集（任一方式）
#    - 用户脚本/Playwright 在文档开始时注入 play-collect.js，然后打开：
#      http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k
#    - 或在同源页面控制台粘贴 play-collect.js 后执行：
#      __webrtc2kCollector.subscribe('live/2k')
#    采样 60–90 s 后：
__webrtc2kCollector.stop('baseline')
```

原始数据（未提交，仅本机）：`/tmp/webrtc-2k-samples{,-veryfast}.jsonl`、
`/tmp/webrtc-2k-summary{,-veryfast}.json`、`/tmp/webrtc-2k-console{,-veryfast}.log`、
`/tmp/webrtc-2k-cpu{,-veryfast}.csv`、`/tmp/webrtc-2k-streamlist{,-veryfast}.jsonl`、
`/tmp/webrtc-2k-push-{veryfast,ultrafast}.log`、`/tmp/webrtc-2k-server.log`。

采集结束后已停止 ffmpeg/服务端，未遗留后台进程；服务端运行时产生的
`m7s.db`、`logs/`、`fatal/`、`pending_uploads/`、`shutdown.sh` 已清理。
