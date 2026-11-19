# WebRTC 2K@24 升级后验收报告（Task 7）

- 采集日期：2026-11-18（本机时间 UTC+8）
- 被测版本：工作树 `dsh-39c7ba14-fa26`，**HEAD `db205192`**（`fix(mp4): 收紧分片顺序与时长断言`，仅改 Task 6 脚本/报告；生产代码与 `53e06d6b` 相同）
- 被测能力：Task 1–6 交付的 pion v4.2.26 + GCC 拥塞控制 + PLI 节流 + 服务端发送指标
- 结论口径：**只写实测值**。本机可跑完的短测标“已实测”，30 分钟 / 弱网 / H265/AV1 / TCP / 50 观众等按 Controller Ruling 3 标 **PENDING**，不冒充通过。

## 1. 环境

| 项 | 值 |
|---|---|
| 主机 | macOS 26.7.3（Build 25G342），Intel Core i7-9750H @ 2.60GHz，**6 物理核 / 12 逻辑核**，16 GB RAM |
| 服务端 | 仓库 `example/default`，`-tags sqlite`，Go 1.27.4 darwin/amd64 |
| 推流端 | ffmpeg 7.1.1（libx264），`testsrc2` 合成源 |
| 播放端 | Google Chrome 158.0.8127.36（Playwright 1.56.0，`channel: 'chrome'`，headless） |
| 网络 | 全本机回环（127.0.0.1），**macOS 无 Linux `tc netem`** |
| 端口 | HTTP `:8080`、RTMP `:1935`、WebRTC `udp:9000-9100` |

与设计目标的差异：目标环境是 16 核 Linux 真机；本机为 12 逻辑核 Intel Mac，CPU 预算类断言（A4/A5）不在本报告范围。Chrome 为 headless，是否走硬件解码未验证。

## 2. 服务端配置与启动

临时配置 `/tmp/webrtc-perf-config.yaml`（无 S3，避免存储错误污染播放日志；DB 放 `/tmp` 避免仓库产物）：

```yaml
global:
  http:
    listenaddr: :8080
  loglevel: debug          # A7 需要 10s stats 行
  admin:
    enablelogin: false
  db:
    dsn: /tmp/webrtc-2k-verify/m7s.db
    dbtype: sqlite
webrtc:
  port: udp:9000-9100
```

构建与启动（`go run` 会派生临时子进程，不便按 PID 精确停止；改用与 `go run -tags sqlite main.go` 完全相同的标签/入口先构建再运行）：

```bash
cd example/default
go build -tags sqlite -o /tmp/webrtc-2k-verify/m7s-server main.go

# 默认 GCC（A1 / A1b / A7 / A6）
cd /tmp/webrtc-2k-verify
./m7s-server -c /tmp/webrtc-perf-config.yaml

# A8 回滚：cc.enable=false（环境变量覆盖，优先级高于文件）
cd /tmp/webrtc-2k-verify
WEBRTC_CC_ENABLE=false ./m7s-server -c /tmp/webrtc-perf-config.yaml
```

生效配置在启动日志中原样打印，已核对：

```
cc:
    enable: true            # 默认 GCC 口径
    initialbitrate: 2000000
    maxbitrate: 20000000
    minbitrate: 300000
```

```
cc:
    enable: false           # A8 口径（WEBRTC_CC_ENABLE=false 生效）
    initialbitrate: 2000000
    maxbitrate: 20000000
    minbitrate: 300000
```

## 3. 推流、播放与采集命令

### 3.1 推流

```bash
cd example/webrtc-2k-perf

# A1 / A8：2K@24 10 Mbps（Task 0 脚本）
PRESET=ultrafast DURATION=0 ./push-2k.sh
# 实际执行：
# ffmpeg -re -f lavfi -i testsrc2=size=2560x1440:rate=24 \
#   -c:v libx264 -preset ultrafast -b:v 10M -maxrate 12M -bufsize 20M \
#   -g 48 -pix_fmt yuv420p -f flv rtmp://127.0.0.1/live/2k

# A1b：1080p@30 4 Mbps
SIZE=1920x1080 FPS=30 BITRATE=4M MAXRATE=5M BUFSIZE=10M GOP=60 \
  RTMP_URL=rtmp://127.0.0.1/live/1080p PRESET=ultrafast DURATION=0 ./push-2k.sh

# A6：2K@24 + AAC-LC 48 kHz 单声道
ffmpeg -hide_banner -loglevel info -re \
  -f lavfi -i 'testsrc2=size=2560x1440:rate=24' \
  -f lavfi -i 'sine=frequency=1000:sample_rate=48000' \
  -c:v libx264 -preset ultrafast -b:v 10M -maxrate 12M -bufsize 20M -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 128k -f flv rtmp://127.0.0.1/live/aac2k
```

2K 源的 ffmpeg 自报最终统计：`frame=5491 fps=24 q=-1.0 Lsize=279164KiB time=00:03:48.79 bitrate=9995.6kbits/s speed=1x`（ultrafast 可稳定实时；`veryfast` 会掉到 0.95x，故本轮统一 ultrafast）。

### 3.2 播放与采集

播放页：`http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/<path>`。

采集用 Task 0 的 `play-collect.js`：Playwright `addInitScript` 在文档开始时注入，接管播放页自建的 `RTCPeerConnection`，每 5 s `getStats()` 采样一次并输出 summary。临时 runner 复制/参数化自 `/private/tmp/webrtc-2k-run/run-baseline.js`，放在 `/tmp/webrtc-2k-verify/run-collect.js`，**未提交**：

```bash
cd /tmp/webrtc-2k-verify

PLAY_URL='http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k' \
RUN_SECONDS=120 \
CONSOLE_LOG=/tmp/webrtc-2k-verify/console-a1.log \
SUMMARY_PATH=/tmp/webrtc-2k-verify/summary-a1.json \
SAMPLES_PATH=/tmp/webrtc-2k-verify/samples-a1.jsonl \
node run-collect.js
```

其余三组仅替换 `PLAY_URL` / `RUN_SECONDS` / 输出路径（1080p 60 s、cc-off 2K 60 s、AAC 30 s 并额外设置 `PROBE_PATH=/tmp/webrtc-2k-verify/probe-aac.json` 导出音轨/SDP/getStats 探测结果）。

后续统计由临时脚本 `/tmp/webrtc-2k-verify/analyze.js` 从 `samples.jsonl` 计算：整体窗口、稳态窗口（t≥30 s 及 t≥80 s）、丢帧/NACK/PLI 增量、jitter buffer 均值、RTT、接收码率。summary 的窗口从首条采样（t≈5.3 s）到最后一条，因此“120 s 运行”的有效跨度为 115 s。

## 4. 实测结果

### 4.1 A1-sanity：2K@24 直连 RTMP，默认 GCC（120 s）

浏览器 summary 原始 JSON：

```json
{"type":"summary","elapsedSec":124.46,"intervalMs":5000,"samples":24,"errors":0,
 "peerConnections":[{"pc":1,"connectionState":"connected","samples":24,"spanSec":115,
 "decodedFrames":2381,"avgFps":20.7,"sampleFpsMean":20.7,"sampleFpsMin":13.4,
 "framesDropped":277,"nackCount":0,"pliCount":22,
 "jitterBufferDelayAvg":0.5786,"lastRtt":0.002}]}
```

| 窗口 | 跨度 | 平均帧率 | 丢帧 | NACK 增量 | PLI 增量 | jitterBufferDelayAvg | RTT 均值 | 接收码率 |
|---|---|---|---|---|---|---|---|---|
| 全程 0–120 s | 115.0 s | **20.70 fps**（2381 帧） | 277 | 0 | 22 | 578.6 ms | 0.9 ms | 10.17 Mbps |
| 稳态 t≥30 s | 90.0 s | **21.67 fps**（1950 帧） | 158 | 0 | 15 | 650.0 ms | 0.9 ms | 9.98 Mbps |
| 稳态 t≥80 s | 40.0 s | **23.88 fps**（955 帧） | 10 | 0 | 0 | 545.1 ms | 1.1 ms | 10.02 Mbps |

逐 5 s 采样（fps / 接收 Mbps / jitterBufferDelayAvg / 该区间 PLI）：

```
t=10.3  14.01 fps / 6.07 Mbps / 158 ms / 0 PLI
t=20.3  17.40 fps / 14.19 Mbps / 394 ms / 4 PLI
t=30.3  21.00 fps / 9.84 Mbps / 742 ms / 0 PLI
t=40.3  13.40 fps / 9.87 Mbps / 777 ms / 7 PLI
t=50.3  17.00 fps / 9.95 Mbps / 800 ms / 3 PLI
t=60.3  19.60 fps / 10.18 Mbps / 773 ms / 0 PLI
t=70.3  23.00 fps / 10.02 Mbps / 718 ms / 0 PLI
t=80.3  24.39 fps / 10.11 Mbps / 645 ms / 0 PLI
t=90.3  23.00 fps / 9.95 Mbps / 590 ms / 0 PLI
t=100.3 23.60 fps / 10.04 Mbps / 538 ms / 0 PLI
t=110.3 24.20 fps / 10.03 Mbps / 497 ms / 0 PLI
t=120.3 23.20 fps / 10.28 Mbps / 463 ms / 0 PLI
```

判读（必须如实记录）：

- **120 s 平均 20.70 fps < 23.5 fps**，`t≥30 s` 窗口 21.67 fps 也未达标；只有 **t≥80 s 之后**回到 23.88 fps、PLI/丢帧停止增长。**不能据此宣称 A1 通过**（A1 是 30 分钟长稳，本轮未做）。
- 冷启动特征与 Task 5 一致：服务端 `target_bitrate` 从 4.5 Mbps → 11.9 Mbps → 20 Mbps 爬升（见 §4.4），而源约 10 Mbps，pacer 在 `initialbitrate=2000000` 下先积压，Chrome 通过丢帧/PLI 追赶；本轮队列影响持续到约 80 s，比 Task 5 RTSP 路径（约 20–25 s）更长。30 分钟窗口会把该窗口摊薄，但仍须真机验证。
- 全程 `nackCount` 增量 0、`packetsLost` 从首条采样 212 到末条仍是 212（无新增网络丢包）；`framesDropped` 是接收端 jitter buffer/渲染丢弃，不是网络丢包。
- RTT 0.9–1.1 ms（回环）。

### 4.2 A1b：1080p@30（60 s，默认 GCC）

summary 原始 JSON：

```json
{"type":"summary","elapsedSec":62.4,"intervalMs":5000,"samples":12,"errors":0,
 "peerConnections":[{"pc":1,"connectionState":"connected","samples":12,"spanSec":55,
 "decodedFrames":1650,"avgFps":30,"sampleFpsMean":30,"sampleFpsMin":29.99,
 "framesDropped":0,"nackCount":0,"pliCount":0,
 "jitterBufferDelayAvg":0.0139,"lastRtt":0.001}]}
```

| 指标 | 值 |
|---|---|
| 分辨率 / 帧率 | 1920×1080 / 30 fps |
| 平均帧率 | **30.00 fps**（1650 帧 / 55.0 s） |
| 单采样最低 | **29.99 fps** |
| 丢帧 | **0** |
| NACK / PLI 增量 | 0 / 0 |
| jitterBufferDelayAvg | **13.9 ms** |
| RTT | 1 ms |
| 接收码率 | **4.00 Mbps** |

断言 `平均帧率 ≥ 29 fps`：**PASS（60 s 短测）**。1080p 源只有 4 Mbps，初始 2 Mbps 造成的积压很快被 GCC 吸收，因此没有 2K 那样的长冷启动；10 分钟长稳仍 PENDING。

### 4.3 A8：回滚 `cc.enable=false`（2K，60 s）

summary 原始 JSON：

```json
{"type":"summary","elapsedSec":61.63,"intervalMs":5000,"samples":12,"errors":0,
 "peerConnections":[{"pc":1,"connectionState":"connected","samples":12,"spanSec":55,
 "decodedFrames":1320,"avgFps":24,"sampleFpsMean":24,"sampleFpsMin":23.99,
 "framesDropped":0,"nackCount":0,"pliCount":0,
 "jitterBufferDelayAvg":0.0085,"lastRtt":0.001}]}
```

| 指标 | cc-off（本轮 60 s） | 升级前基线（Task 0，ultrafast 60 s） | 默认 GCC（同机 2K，前 60 s 窗口） |
|---|---|---|---|
| 平均帧率 | **24.00 fps** | 24.00 fps | 18.02 fps |
| 单采样最低 | 23.99 fps | 23.6 fps | 13.4 fps |
| 丢帧 | **0** | 0 | 229 |
| NACK / PLI 增量 | 0 / 0 | 首采样 1 / 8，其后 0 | 0 / 17 |
| jitterBufferDelayAvg | **8.5 ms** | 8.0 ms | 568.2 ms |
| RTT | 1 ms | 1–2 ms | 0.8 ms |
| 接收码率 | 9.87 Mbps | 9.97 Mbps | 10.36 Mbps |

行为验证（不是只看配置）：

- 服务端 stats 行中 **`target_bitrate=0`**、`send_bitrate≈9.8–10.2 Mbps`（见 §4.4），说明该链路没有 GCC 估计器参与（`OnBandwidthEstimate` 只由 cc 拦截器回调）。
- 60 s 指标与升级前基线一致（24.00 fps、0 丢帧、jitter buffer 8 ms 量级），符合 A8“行为与基线一致”。
- 注：上表 GCC 列不是等价 A/B——它包含 2K 冷启动窗口；GCC 的 `t≥80 s` 稳态为 23.88 fps / 10 丢帧 / 0 PLI，与 cc-off 相同量级。`cc.enable=false` 的代码路径与升级前完全一致已由 Task 2 单测钉住（无 cc 链不含 cc/TWCC 拦截器）。

### 4.4 A7：可观测性（`global.loglevel: debug`）

服务端每 10 s 一条 `webrtc stats`，默认 GCC 口径的实测日志（节选自 `/tmp/webrtc-2k-verify/server-a1.log`，已去除 ANSI 颜色）：

```
22:21:49.024244 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=4523871 send_bitrate=9975046 packets_sent=8678 packets_lost=212 pli_requests=0
22:21:59.025592 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=11945974 send_bitrate=9812258 packets_sent=17207 packets_lost=212 pli_requests=0
22:22:09.065311 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=20000000 send_bitrate=9779295 packets_sent=25738 packets_lost=212 pli_requests=0
22:22:19.065762 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=20000000 send_bitrate=9908068 packets_sent=34347 packets_lost=212 pli_requests=0
```

cc-off 口径（`server-ccoff.log`）：

```
22:27:28.951196 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=0 send_bitrate=10152617 packets_sent=8832 packets_lost=0 pli_requests=0
22:27:38.953172 DBG webrtc stats server=5 plugin=WebRTC metrics=target_bitrate=0 send_bitrate=9773058 packets_sent=17326 packets_lost=0 pli_requests=0
```

结论：

- `target_bitrate` 与 `send_bitrate` **都能从日志读出**；默认 GCC 下 target 非零并随估计器收敛（4.5 Mbps → 11.9 Mbps → 20 Mbps 上限）。
- `send_bitrate` ≈ **9.8–10.1 Mbps**，与源（ffmpeg 自报 ~10.0 Mbps）吻合；cc-off 下 target 恒为 0 而 send 仍约 10 Mbps，可作为“GCC 未启用”的运行时判据。
- 默认 GCC 运行中 `pli_requests=0`（服务端向源请求关键帧的计数），浏览器侧 PLI 增量为 22（集中于冷启动）。
- **弱网时 target_bitrate 是否下降：PENDING**（本机无 `tc netem`，见 §6）。

### 4.5 A6：AAC→Opus（可行部分）

源：2K@24 H264 + AAC-LC 48 kHz 单声道（`live/aac2k`）。服务端 `/api/stream/list` 确认：

```json
{"path":"live/aac2k","audioTrack":{"codec":"mp4a","meta":"sample rate: 48000, channels: 1, object type: 2"},
 "videoTrack":{"codec":"avc1","meta":"fps: 24, resolution: 2560x1440"}}
```

WebRTC 播放 30 s（summary：24.00 fps、600 帧 / 25 s、0 丢帧、0 NACK/PLI、jitter 9.9 ms），同时探测浏览器侧音轨与 answer SDP：

| 探测项 | 结果 |
|---|---|
| answer SDP 音频 m-line | `m=audio 9 UDP/TLS/RTP/SAVPF 0 8`（**port 9 = 拒绝**），`a=mid:1`、`a=sendonly`、`a=rtpmap:0 PCMU/8000`、`a=rtpmap:8 PCMA/8000` |
| 浏览器 receivers | video: `live`；audio: `live` 但 **`muted: true`**，`codec: null` |
| `inbound-rtp` audio | **不存在**（`inboundAudio: []`） |
| codec stats | 仅 `video/H264`（payload 102）；无 `audio/opus` |
| 服务端警告 | `WRN AAC->Opus transcode unavailable, WebRTC video only ... error=aac_opus_transcode build tag not enabled`（1 次/连接） |

结论：**本构建没有 AAC→Opus 转码能力**。`pkg/aacopus` 的 CGO 实现受 `aac_opus_transcode` 构建标签门控，且仓库内无 `3rd/ffmpeg6/` 预编译库，`-tags sqlite` 命中 stub（`errors.New("aac_opus_transcode build tag not enabled")`），WebRTC 只能出视频。**A6 的 AAC→Opus 项记为 PENDING（构建能力缺失），不是通过**；修复方向：带 `aac_opus_transcode` + `3rd/ffmpeg6` 库构建，或改为服务端转码链路，然后重跑本节的探测。

正向证据（来自 Task 6）：WHIP 推流（Chrome 假设备 H264 + Opus）在服务端 `/api/stream/list` 显示 `video=avc1`、`audio=Opus`，推流侧 H264+Opus 可用；**拉流侧 AAC→Opus 仍缺**。H265/AV1/TCP/WHEP/batchv2 未测（Ruling 3 PENDING）。

### 4.6 与 Task 5（A9/A10）的交叉引用

Task 5（提交 `f59bb3ac`，报告 `example/webrtc-2k-perf/report-rtsp-pull.md`）在同一台机器上测得：

- 2K RTSP 拉流单路 60 s：默认 GCC 平均 **23.65 fps**（1301 帧 / 55 s），`t≥25 s` 稳态 **24.0 fps / 0 包丢失**（60 s 内 PLI 累计 8、丢帧 32，集中在冷启动），jitter buffer 均值 240 ms；`cc.enable=false` 对照 24.00 fps、0 丢帧、8.4 ms。
- 断流重连（默认 GCC）：源恢复后 **0.38 s** 拉流可用，恢复后 25 s 复播 23.0 fps。

与本节直连 RTMP 的差异：直连路径冷启动更长（约 80 s 才回到 24 fps），RTSP 拉流路径约 20–25 s；两者稳态都满帧，均未做 30 分钟长稳。**A9/A10 的结论以 Task 5 报告为准，本报告不重复判定。**

### 4.7 与 Task 6（A11/A12/A13）的交叉引用

- **A11 PASS**：RTSP 源录制 h264 2560×1440 / 24.072 fps / 25.132 s；含 AAC 音频的录制 18.049 s。
- **A12 产品阻断**：MP4 录制器不支持 Opus（progressive 报 `unsupported audio codec for mp4 record: *codec.OPUSCtx`，fMP4 在 `CreateOpusSpecificBox` panic，DB 元数据误报 `audioCodec=opus`）。详见 Task 6 报告与 `report-record.md`。
- **A13 PASS**：`fragment=1m` 175 s 得 3 片（60.300 / 59.957 / 54.832 s，合计 175.089 s），逐片 ffprobe 可读。

## 5. 验收表（A1 / A1b / A1c / A2 / A3 / A6 / A7 / A8）

| 项 | 设计要求 | 本轮状态 | 实测证据 | 原因 / 备注 |
|---|---|---|---|---|
| **A1** | 单观众 2K@24，0% 丢包，**30 分钟**；帧率 ≥ 23.5 fps、无连续 >1s 花屏、延迟 P95 ≤ 1.2s | **PENDING（30 min）/ 短测未达阈值** | 120 s：平均 20.70 fps；t≥30 s 21.67 fps；t≥80 s **23.88 fps**；丢帧 277；NACK 0；PLI 22；jitter 均值 579 ms；RTT 0.9 ms；10.17 Mbps | 直连 RTMP 冷启动队列影响约 80 s（initialbitrate 2 Mbps vs 源 10 Mbps），短窗被摊薄不足；**不能据此判 A1 通过**。P95 端到端延迟无玻璃到玻璃时间戳，未测 |
| **A1b** | 1080p@30，0% 丢包，10 分钟；平均帧率 ≥ 29 fps | **PARTIAL（60 s PASS；10 min PENDING）** | 60 s：**30.00 fps**，最低采样 29.99，0 丢帧，0 NACK/PLI，jitter 13.9 ms，4.00 Mbps | 短测通过；10 分钟长稳需 Linux/真机 |
| **A1c** | 1080p@30，1%/3% 丢包各 10 分钟；平均 ≥ 24 fps，无持续 <20 fps | **PENDING** | — | macOS 无 `tc netem`；命令见 §6 |
| **A2** | 2K@24，1% 丢包 10 分钟；无永久卡死、恢复 ≤ 3s、平均 ≥ 22 fps | **PENDING** | — | 同上 |
| **A3** | 2K@24，3% 丢包 10 分钟；无永久卡死、恢复 ≤ 5s、画面可辨 | **PENDING** | — | 同上 |
| **A6** | 既有功能回归：WHIP/WHEP/batchv2/TCP/H265/H264/AV1（含 AAC→Opus） | **PARTIAL** | H264 2K 播放 24.00 fps（AAC 源同测 30 s）；AAC→Opus **转码器不可用**（answer 拒绝音频、无 inbound audio、stub 警告）；WHIP H264+Opus 推流由 Task 6 实测通过 | AAC→Opus 因构建标签/三方库缺失为 PENDING；H265/AV1/TCP/WHEP/batchv2 按 Ruling 3 PENDING |
| **A7** | 日志能读出目标码率与实际码率；弱网时目标码率下降 | **PARTIAL（target/send PASS；RTT/jitter/PLI 字段后续补齐；弱网下降 PENDING）** | A1 窗口 12 条 10 s stats 行；默认 GCC `target 4.5→11.9→20 Mbps`、`send 9.8–10.1 Mbps`；cc-off `target=0`、`send≈9.8–10.2 Mbps`。审查修复后同一日志行新增 `rtt/jitter/pli_requests`，本次 Task 7 测量未重跑 | 弱网需 `tc netem`，见 §6 |
| **A8** | `cc.enable=false` 后行为与升级前一致 | **MEASURED（60 s）** | 24.00 fps、0 丢帧、0 NACK/PLI、jitter 8.5 ms、9.87 Mbps；与升级前基线（24.00 / 0 / 8.0 ms / 9.97 Mbps）一致；运行时 `target_bitrate=0` | 30 分钟 cc-off 长稳未做；代码路径一致性由 Task 2 单测钉住 |

## 6. PENDING 清单与 Linux 真机命令（Ruling 3）

macOS 不能执行这些项的原因：`tc`/`netem` 是 Linux 流量控制子系统；macOS 对应的是 Dummynet（`dnctl` + `pfctl`），语义/统计口径不同，且不能在不改动系统防火墙的情况下对回环做 1%/3% 随机丢包与 50/150 ms 时延组合。Ruling 3 要求这些项在 Linux 真机执行，不用近似值代替。

在 Linux 测试机（服务端 + 推流 + Chrome 同机或分机均可）执行：

```bash
# A2：1% 丢包 + 50 ms 时延（回环拓扑）
sudo tc qdisc add dev lo root netem loss 1% delay 50ms
# 另开终端推 2K 源并播放 10 分钟：
PRESET=ultrafast DURATION=0 ./example/webrtc-2k-perf/push-2k.sh
PLAY_URL='http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k' RUN_SECONDS=600 \
SUMMARY_PATH=/tmp/a2-summary.json SAMPLES_PATH=/tmp/a2-samples.jsonl \
node /tmp/webrtc-2k-verify/run-collect.js
sudo tc qdisc del dev lo root

# A3：3% 丢包 + 150 ms 时延
sudo tc qdisc change dev lo root netem loss 3% delay 150ms
# 同上播放 10 分钟；结束后 sudo tc qdisc del dev lo root

# A1：30 分钟 0% 丢包（先确认无 netem）
ip -s link show lo
PRESET=ultrafast DURATION=0 ./example/webrtc-2k-perf/push-2k.sh
PLAY_URL='http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k' RUN_SECONDS=1800 \
SUMMARY_PATH=/tmp/a1-30min-summary.json SAMPLES_PATH=/tmp/a1-30min-samples.jsonl \
node /tmp/webrtc-2k-verify/run-collect.js

# A1c：1080p 1% / 3%（各 10 分钟）
SIZE=1920x1080 FPS=30 BITRATE=4M MAXRATE=5M BUFSIZE=10M GOP=60 \
  RTMP_URL=rtmp://127.0.0.1/live/1080p PRESET=ultrafast ./example/webrtc-2k-perf/push-2k.sh
sudo tc qdisc add dev lo root netem loss 1% delay 50ms      # 再 change 为 3%
PLAY_URL='http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/1080p' RUN_SECONDS=600 \
SUMMARY_PATH=/tmp/a1c-summary.json SAMPLES_PATH=/tmp/a1c-samples.jsonl \
node /tmp/webrtc-2k-verify/run-collect.js
sudo tc qdisc del dev lo root
```

若服务端与浏览器分机：把 `tc qdisc` 加在**服务端出向网卡**（如 `eth0`）而非 `lo`，并保证推流走另一网卡或先在服务端本地推流，避免把推流也纳入丢包口径。

其余 PENDING：

| 项 | 说明 / 命令要点 |
|---|---|
| A2/A3 的“恢复 ≤3s/≤5s” | 需在丢包窗口内制造源中断（`kill` 推流 → 重启）并用 samples 时间戳判定恢复；脚本 `rtsp-pull.sh reconnect` 可作参考 |
| H265 / AV1 | 用 `ffmpeg -c:v libx265` / `libaom-av1` 推 RTMP（需服务端/协议支持），Chrome 播放验证 `frameWidth/Height` 与 fps |
| TCP 模式 | 服务端 `webrtc.port: tcp:9000`（或 `tcp:9000-9100`）启动后重跑 §3.2；需在防火墙放行 |
| 50 观众 batchv2（A5） | Task 8 覆盖 10 观众；50 观众需真机压测，观察服务端 CPU、出向码率、日志错误风暴 |
| 真实设备 | 真实摄像头 RTSP/GB28181 源、真实 Chrome 有头浏览器；本机为合成源 + headless |
| A6 AAC→Opus | 带 `aac_opus_transcode` 标签和 `3rd/ffmpeg6` 预编译库构建后，重跑 `live/aac2k` 探测（answer SDP 应出现 opus、`inbound-rtp audio` 非空） |

## 7. 限制与诚实性声明

1. **未做 30 分钟长稳**：A1、A1b/A1c 的长窗口均未执行；本报告的 60/120 s 数据只证明短时行为，不构成 A1/A1b 的最终通过。
2. **未做弱网**：A1c/A2/A3 与 A7 的“弱网 target_bitrate 下降”全部 PENDING，没有用任何近似/推算值填充。
3. **2K 冷启动**：默认 `initialbitrate=2000000` 对 10 Mbps 源明显偏低，本轮直连 RTMP 冷启动约 80 s 才回到满帧；120 s 平均值因此只有 20.70 fps。这是实测，不是噪声；Task 5 的 RTSP 路径冷启动约 20–25 s，说明冷启动时长与接入路径有关。生产建议把 `initialbitrate` 提到接近源码率（如 8–10 Mbps），但本轮未改默认值。
4. **采样口径**：`play-collect.js` 每 5 s 采样，首条在 t≈5.3 s，summary 的 `spanSec=115` 对应 120 s 运行；`jitterBufferDelayAvg` 是**累计平均**（早期大延迟会把均值拖高、回落很慢），不等于瞬时队列；`framesDropped` 是接收端丢弃，不是网络丢包（`packetsLost` 增量全程为 0）。
5. **端到端延迟 P95 未测**：没有玻璃到玻璃时间戳，`getStats()` 只能给 jitter buffer 延迟，不能代替 A1 的 P95 ≤ 1.2 s。
6. **主机差异**：12 逻辑核 / 6 物理核 Intel Mac，非 16 核 Linux 目标机；headless Chrome 的解码路径（硬件/软件）未验证。绝对 CPU/帧率不能直接外推到目标环境。
7. **推流端质量**：`veryfast` 在本机 2K 下约 0.95x（非实时），因此所有 2K 数据都用 `ultrafast`（speed≈1x）；1080p 4 Mbps 无此问题。
8. **A6 负结果**：AAC→Opus 的缺失是构建能力问题（stub + 缺少 `3rd/ffmpeg6`），不是网络/配置问题；正向的 WHIP H264+Opus 推流证据来自 Task 6，拉流侧仍未闭环。

## 8. 原始产物（未提交，均在 `/tmp/webrtc-2k-verify/`）

| 内容 | 路径 |
|---|---|
| 服务端配置 | `/tmp/webrtc-perf-config.yaml` |
| 服务端二进制（83 MB，`-tags sqlite`） | `/tmp/webrtc-2k-verify/m7s-server` |
| 默认 GCC 服务端日志（A1/A1b/A7） | `/tmp/webrtc-2k-verify/server-a1.log` |
| cc-off 服务端日志（A8/A6） | `/tmp/webrtc-2k-verify/server-ccoff.log` |
| A1 summary / samples / console | `summary-a1.json`、`samples-a1.jsonl`、`console-a1.log` |
| A1b summary / samples | `summary-1080p.json`、`samples-1080p.jsonl` |
| A8 summary / samples | `summary-ccoff.json`、`samples-ccoff.jsonl` |
| A6 summary / samples / 探测 | `summary-aac.json`、`samples-aac.jsonl`、`probe-aac.json` |
| A7 stats 行摘录 | `stats-a1.txt` |
| 分析结果 | `analyze-a1.json`、`analyze-a1-steady80.json`、`analyze-1080p.json`、`analyze-ccoff.json` |
| 推流日志 | `push-2k-a1.log`、`push-1080p.log`、`push-2k-ccoff.log`、`push-aac2k.log` |
| 临时 runner / 分析脚本（不提交） | `run-collect.js`、`analyze.js` |

## 9. 复现步骤汇总

```bash
# 1) 构建并启动（默认 GCC）
cd example/default && go build -tags sqlite -o /tmp/webrtc-2k-verify/m7s-server main.go
cd /tmp/webrtc-2k-verify && ./m7s-server -c /tmp/webrtc-perf-config.yaml

# 2) 2K 推流 + 120 s 播放（A1）
cd example/webrtc-2k-perf && PRESET=ultrafast DURATION=0 ./push-2k.sh
cd /tmp/webrtc-2k-verify && PLAY_URL='http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k' \
  RUN_SECONDS=120 SUMMARY_PATH=/tmp/webrtc-2k-verify/summary-a1.json \
  SAMPLES_PATH=/tmp/webrtc-2k-verify/samples-a1.jsonl node run-collect.js

# 3) 1080p（A1b）：先停 2K 源，再按 §3.1 推 1080p，RUN_SECONDS=60

# 4) A8：停止服务端，WEBRTC_CC_ENABLE=false 重启，重复 2K 60 s

# 5) A7：grep -a "webrtc stats" /tmp/webrtc-2k-verify/server-a1.log

# 6) A6：起 AAC 源，RUN_SECONDS=30 PROBE_PATH=/tmp/webrtc-2k-verify/probe-aac.json
```

所有进程已在测量后停止（8080/1935 无监听、无残留 ffmpeg/server/Playwright runner）；仓库工作树干净，未提交临时脚本与 `/tmp` 数据。
