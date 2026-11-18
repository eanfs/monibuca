# WebRTC 2K@24 观众数压测报告（Task 8）

- 采集日期：2026-11-18（本机时间 UTC+8）
- 被测版本：工作树 `dsh-39c7ba14-fa26`，**HEAD `fa63c113`**（Task 7 验收报告提交；Task 8 未改任何代码）
- 被测能力：Task 1–7 交付的 pion v4.2.26 + GCC 拥塞控制 + PLI 节流 + 服务端发送指标，2K@24 源单路多观众
- 结论口径：**只写实测值**。本机可完成的 10 观众短测标“已实测”；50 观众 batchv2（A5）与真实 16 核目标机按 Ruling 3 标 **PENDING**，不冒充通过，不用推算值填充。

## 1. 环境

| 项 | 值 |
|---|---|
| 主机 | macOS 26.7.3（Build 25G342），Intel Core i7-9750H @ 2.60 GHz，**6 物理核 / 12 逻辑核**，16 GB RAM |
| 服务端 | `example/default`，`go build -tags sqlite`，Go 1.27.4 darwin/amd64，PID 86643 |
| 推流端 | ffmpeg 7.1.1（libx264，`testsrc2` 合成源），2K@24 10 Mbps |
| 观众端 | Google Chrome 158.0.8127.36（Playwright 1.56.0，`channel: 'chrome'`，headless，单浏览器/单 context/10 页） |
| 网络 | 全部本机回环 127.0.0.1；服务端 HTTP `:8080`、RTMP `:1935`、WebRTC `udp:9000-9100`、RTSP `:8554` |
| 采样 | 服务端 CPU/RSS 每 5 s；`/api/stream/list` 每 5 s；整机 CPU（`top`）每 10 s；浏览器 `getStats()` 每 5 s |

与设计目标的差异：目标环境是 **16 核 Linux**；本机为 12 逻辑核 Intel Mac，且 10 个观众（软件解码）与服务端、编码器同机竞争 CPU。**A4/A5 的 CPU 类验收不能在本机闭环**，详见 §3.3/§5。

## 2. 服务端、源与 runner

### 2.1 服务端

临时配置 `/tmp/webrtc-2k-load/config.yaml`（无 S3；DB 与日志都在 `/tmp`，不污染仓库）：

```yaml
global:
  http:
    listenaddr: :8080
  loglevel: info
  admin:
    enablelogin: false
  db:
    dsn: /tmp/webrtc-2k-load/m7s.db
    dbtype: sqlite
webrtc:
  port: udp:9000-9100
rtsp:
  tcp:
    listenaddr: :8554
```

构建与启动（与 Task 7 相同的标签/入口；用构建产物而非 `go run`，便于按 PID 精确采样与停止）：

```bash
cd example/default
go build -tags sqlite -o /tmp/webrtc-2k-load/m7s-server .

cd /tmp/webrtc-2k-load
nohup ./m7s-server -c /tmp/webrtc-2k-load/config.yaml > /tmp/webrtc-2k-load/server.log 2>&1 &
echo $! > /tmp/webrtc-2k-load/server.pid     # 本报告为 86643
```

### 2.2 2K 源

```bash
cd example/webrtc-2k-perf
PRESET=ultrafast DURATION=0 ./push-2k.sh
# 实际执行：
# ffmpeg -re -f lavfi -i testsrc2=size=2560x1440:rate=24 \
#   -c:v libx264 -preset ultrafast -b:v 10M -maxrate 12M -bufsize 20M \
#   -g 48 -pix_fmt yuv420p -f flv rtmp://127.0.0.1/live/2k
```

ffmpeg 自报最终统计：`frame=13009 fps=24 bitrate=9996.1kbits/s speed=0.999x`；服务端 `/api/stream/list` 同期显示 `live/2k` 2560×1440、fps 23–24、源 `bps` ≈ 1.25 MB/s（≈10.0 Mbps）。

batchv2 sanity 另加了一路小源（验证单 PC 多流），随后已停：

```bash
SIZE=1280x720 FPS=24 BITRATE=2M MAXRATE=3M BUFSIZE=6M GOP=48 \
  RTMP_URL=rtmp://127.0.0.1/live/720p PRESET=ultrafast DURATION=0 ./push-2k.sh
```

### 2.3 观众 runner（临时脚本，未提交）

`/tmp/webrtc-2k-load/run-load.js`（基于 Task 0 的 `play-collect.js` 与 Task 7 的 Scratch runner 改写）：

- `chromium.launch({ channel: 'chrome', headless: true })`，参数含 `--autoplay-policy=no-user-gesture-required`、`--disable-background-timer-throttling`、`--disable-renderer-backgrounding`、`--disable-backgrounding-occluded-windows`、`--mute-audio`；
- 一个 context，`addInitScript` 注入 `play-collect.js`（接管页面自建的 `RTCPeerConnection`，每 5 s `getStats()`）；
- 并发打开 **10 个页面**：`http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k`；
- 等到每页 `pc.connectionState === 'connected'` 且 `video.videoWidth > 0` 后，开始 60 s 计时；
- 计时结束对每页调用 `__webrtc2kCollector.stop('load')`，取 summary/采样/`getVideoPlaybackQuality()`；
- 期间采样服务端进程 `ps -o %cpu=,rss=,time= -p <pid>`（5 s）、`netstat -ibn` lo0 字节（5 s）、`/api/stream/list`（5 s）、整机 `top`（10 s）。

CPU 口径（重要）：

- `ps`/`top` 的 CPU 百分比均为 **per-core 语义，100% = 占满 1 个逻辑核**；本机 12 逻辑核满载 = 1200%。
- macOS `ps %cpu` 是**衰减平均**（不是瞬时值），因此报告以 **`ps -o time=` 的 cputime 增量 / 墙钟增量** 作为区间 CPU 主指标（`intervalCpuPct`），`ps %cpu` 仅作交叉参考。
- 服务端 RSS 由 `ps rss`（KB）换算 MB。

```bash
cd /tmp/webrtc-2k-load
NODE_PATH=/private/tmp/webrtc-2k-run/node_modules \
  VIEWERS=10 RUN_SECONDS=60 RUN_PREFIX=10v SERVER_PID=$(cat server.pid) node run-load.js
```

同一 runner 以 `VIEWERS=1 RUN_SECONDS=20` 跑了一次单观众参照；10 观众共跑两轮（`10v`、`10v-run2`）用于复现性检查。

## 3. 10 观众（A4）

### 3.1 运行 A（60 s，22:41:28–22:42:28）

10 个页面全部建连成功，测量窗口内订阅者恒为 10。逐观众：

| # | avgFps | 5 s 采样最低 | 丢帧 | NACK | PLI | jitter buffer 均值 | RTT | 接收码率 |
|---|---|---|---|---|---|---|---|---|
| 1 | 24.02 | 22.60 | 19 | 0 | 5 | 86 ms | 1 ms | 10.41 Mbps |
| 2 | 24.18 | 23.80 | 15 | 0 | 3 | 94 ms | 1 ms | 10.40 Mbps |
| 3 | 24.29 | 22.60 | 16 | 0 | 3 | 190 ms | 1 ms | 10.41 Mbps |
| 4 | 24.04 | 18.20 | 18 | 0 | 7 | 220 ms | 0 ms | 10.41 Mbps |
| 5 | 24.11 | 23.80 | 15 | 0 | 4 | 93 ms | 1 ms | 10.41 Mbps |
| 6 | 24.15 | 21.60 | 18 | 0 | 4 | 142 ms | 1 ms | 10.46 Mbps |
| 7 | 24.09 | 18.00 | 17 | 0 | 7 | 214 ms | 1 ms | 10.40 Mbps |
| 8 | 24.25 | 23.00 | 15 | 0 | 3 | 122 ms | 1 ms | 10.40 Mbps |
| 9 | 23.95 | 18.40 | 24 | 0 | 7 | 210 ms | 2 ms | 10.43 Mbps |
| 10 | 24.09 | 22.79 | 15 | 0 | 5 | 87 ms | 1 ms | 10.40 Mbps |

聚合：

| 指标 | 值 |
|---|---|
| 平均帧率（跨观众） | **mean 24.12 fps / min 23.95 / max 24.29** |
| 5 s 采样最低（跨观众） | mean 21.48 / worst 18.00 fps |
| 丢帧合计 | 172（人均 17.2，55 s 窗口） |
| NACK / PLI 合计 | **0 / 48**（PLI 集中在冷启动，稳态 0） |
| jitter buffer 均值（跨观众） | 145.7 ms |
| RTT 均值 | 1 ms（回环） |
| 客户端接收码率合计 | **104.13 Mbps**（人均 10.41；min 10.40 / max 10.46） |
| 页面错误 / 崩溃 | 0 / 0 |

服务端（窗口内 12 个 5 s 采样）：

| 指标 | 值 |
|---|---|
| 订阅者 | mean 10 / max 10 |
| 进程 CPU（区间增量，100 = 1 核） | **mean 50.3% / peak 64.8% / P95 64.8%** |
| 折合逻辑核 | mean 0.503 / peak 0.648 |
| **12 逻辑核归一化** | **mean 4.19% / peak 5.40%** |
| 16 核算术归一化（仅参考，不是验收） | mean 3.14% / peak 4.05% |
| RSS | **mean 330.6 MB / peak 361.7 MB**（空载基线 ≈55 MB） |
| `videoTrack.bps_out`（服务端统计的出向） | **mean 12,630,372 B/s = 101.04 Mbps / peak 12,936,005 B/s = 103.49 Mbps** |
| 源 `bps` | 10.01 Mbps |
| lo0 出向总量（含推流+WebRTC+ACK） | mean 119.31 Mbps / peak 153.79 Mbps |
| 整机 CPU（`top`，12 核） | mean 51.3% / peak 61.14% |
| 区间 CPU 采样序列 | 38.8, 59.4, 64.8, 47.4, 45.6, 48.4, 47.2, 51.4, 50.0, 47.4, 51.3, 51.6（%） |

### 3.2 运行 B（60 s，22:46:09–22:47:09，复测）

同一服务端进程、同一 runner 参数重跑（服务端未重启）。逐观众：

| # | avgFps | 5 s 采样最低 | 丢帧 | NACK | PLI | jitter buffer 均值 | RTT | 接收码率 |
|---|---|---|---|---|---|---|---|---|
| 1 | 24.04 | 18.20 | 22 | 0 | 6 | 212 ms | 1 ms | 10.39 Mbps |
| 2 | **16.33** | 13.00 | 300 | 0 | 21 | 657 ms | 1 ms | 10.47 Mbps |
| 3 | **16.13** | 12.80 | 310 | 0 | 23 | 654 ms | 1 ms | 10.47 Mbps |
| 4 | 23.51 | 17.61 | 50 | 0 | 7 | 364 ms | 1 ms | 10.43 Mbps |
| 5 | 24.80 | 23.80 | 10 | 0 | 0 | 117 ms | 0 ms | 10.39 Mbps |
| 6 | 24.87 | 24.00 | 5 | 0 | 0 | 147 ms | 0 ms | 10.39 Mbps |
| 7 | 24.80 | 23.79 | 9 | 0 | 0 | 111 ms | 1 ms | 10.39 Mbps |
| 8 | 24.84 | 23.80 | 9 | 0 | 0 | 124 ms | 1 ms | 10.40 Mbps |
| 9 | 23.00 | 17.00 | 80 | 0 | 7 | 376 ms | 1 ms | 10.45 Mbps |
| 10 | 24.80 | 23.80 | 10 | 0 | 0 | 124 ms | 1 ms | 10.40 Mbps |

聚合：

| 指标 | 值 |
|---|---|
| 平均帧率（跨观众） | **mean 22.71 fps / min 16.13 / max 24.87** |
| 5 s 采样最低（跨观众） | mean 19.78 / worst 12.80 fps |
| 丢帧合计 | 805（人均 80.5） |
| NACK / PLI 合计 | **0 / 64** |
| jitter buffer 均值（跨观众） | 288.6 ms |
| RTT 均值 | 0.8 ms |
| 客户端接收码率合计 | **104.18 Mbps**（人均 10.42；min 10.39 / max 10.47） |
| 页面错误 / 崩溃 | 0 / 0 |

服务端：

| 指标 | 值 |
|---|---|
| 订阅者 | mean 10 / max 10 |
| 进程 CPU（区间增量） | **mean 50.7% / peak 64.0% / P95 64.0%** |
| **12 逻辑核归一化** | **mean 4.23% / peak 5.33%** |
| 16 核算术归一化（仅参考） | mean 3.17% / peak 4.00% |
| RSS | mean 538.1 MB / peak 557.7 MB（进程未重启，含前几轮保留堆） |
| `bps_out` | **mean 101.04 Mbps / peak 102.96 Mbps** |
| 源 `bps` | 10.00 Mbps |
| lo0 出向总量 | mean 115.41 Mbps / peak 142.44 Mbps |
| 整机 CPU（`top`，12 核） | mean 42.6% / peak 52.63% |

`ps %cpu` 交叉参考（macOS 衰减平均，最后一条）：运行 A 58.9%、运行 B 47.7%，与区间增量口径同量级。

### 3.3 判读（A4）

1. **服务端侧能力没有成为瓶颈**：两轮服务端均稳定送出 101 Mbps（≈10.1 Mbps/观众，与源 10 Mbps 吻合），订阅者恒为 10，`bps_out` 峰值 103.5 Mbps；`/api/stream/list` 无流缺失，服务端日志无 panic/ERROR 级连接故障（仅建连期 ICE 与断开期 EOF 噪声，见 §7）。
2. **客户端帧率两轮不一致，原因在客户端侧**：运行 A 全部 10 路 23.95–24.29 fps；运行 B 有 3 路跌到 16.13/16.33/23.00 fps，但同时这 3 路**接收码率仍是 10.45–10.47 Mbps（与其余观众相同）**，NACK 增量 0，说明服务端把完整码流送到了，跌帧发生在同机 10 个 headless Chrome 软件解码/渲染竞争 CPU（运行 B 整机 CPU 反而更低，说明调度抖动而非服务端带宽不足）。**不能把运行 B 的客户端低帧率归因于服务端。**
3. **A4 的 16 核 ≤50% 不能在本机判定**：
   - 实测服务端 CPU = **0.503–0.507 个逻辑核**（mean），peak 0.640–0.648 核；
   - 相对本机 12 逻辑核 = **4.19–4.23%（peak 5.33–5.40%）**；
   - 若按“同样绝对 CPU 搬到 16 核”做**算术归一化** = 3.14–3.17%（peak 4.00–4.05%）；相对 A4 的 8 核预算（16×50%）只占 6.3%；
   - 但这是**归一化，不是 16 核真机实测**：观众与编码器同机，服务端可能被抢 CPU（数值偏低），也没有 50 观众负载；**不宣称 A4 通过**，16 核 Linux 真机 + 分布式观众仍 PENDING。
4. **容量参考（仅供规划，不是验收）**：10 观众时服务端增量 CPU ≈ (50.3−1.4)/10 ≈ **4.9% 个逻辑核/观众**（run B 为 4.85%）；按线性外推 50 观众 ≈ 2.5 个逻辑核 ≈ 16 核的 15.6%。线性外推忽略合流/加密/调度非线性，不能当作 50 观众结论。
5. **接收质量（运行 A 代表值）**：24.12 fps 均值、人均丢帧 17.2/55 s、NACK 0、PLI 3–7 次/观众（集中在冷启动）、jitter buffer 均值 146 ms、RTT 1 ms。运行 B 的低帧率观众同时伴随 650 ms 级 jitter buffer 与 300 帧级丢帧，符合“客户端解码跟不上导致队列堆积”的特征。
6. **单观众参照（20 s，非 A1）**：1 观众时服务端 CPU mean 9.9% 个核（peak 12.8%）、RSS 96 MB、`bps_out` 9.94 Mbps；浏览器 avgFps 26.47（20 s 短窗含启动突发，不能当稳态值）。

## 4. batchv2 小规模 sanity（**不是 A5**）

用 `/webrtc/test/batchv2` 页面自动化：1 个页面 = 1 个 WebSocket + 1 个 PeerConnection，同时订阅 **2 路流**（`live/2k` 2560×1440 + `live/720p` 1280×720）。流程：Connect → Get Stream List → Select All → Play Selected。

| 项 | 结果 |
|---|---|
| 页面步骤 | Connect / Get Stream List（返回 `live/2k,live/720p`）/ Select All / Play Selected 全部成功 |
| 远端视频 | 2 个 `<video>` 均 `readyState=4`，分辨率分别 2560×1440、1280×720 |
| 页面错误条目（`.log-entry.error`） | **0** |
| 采集 summary（45 s 运行） | avgFps **22.18**（40 s 采样跨度）、5 s 最低 16.6、丢帧 103、NACK 0、PLI 7、jitter 335.5 ms、RTT 1 ms |
| 视频元素帧计数 | 2K 1012 帧 / 720p 1024 帧（约 50 s 页面生命周期，含冷启动） |
| 连接状态 | 结束时 `connected`；关闭页面时服务端记录 2 条 `WebSocket read error ... 1001`（预期内） |

20 s 短窗复跑为 avgFps 15.4（冷启动占比更大），45 s 窗口升到 22.18，趋势与标准订阅页的冷启动一致。**该 sanity 只证明 batchv2 通路可用、单 PC 多流可稳定播放，不代表 50 观众（A5）的性能结论。**

## 5. 50 观众 batchv2（A5）：PENDING

**为什么本机不做**：

- 50 个 2K 解码器 + 服务端 + 编码器同机：仅 10 个 headless 观众就已占满本机 42.6–51.3% 的 12 核（且出现客户端侧 16 fps 的调度抖动），50 个观众需要约 5 倍的客户端算力，本机不具备；
- 目标验收环境是 16 核 Linux 真机，本机 12 逻辑核 + 客户端同机无法映射 CPU 结论；
- 单 Chrome 实例开 50 页/50 个解码器本身不是生产观众形态（生产是 50 台/50 个真实浏览器），压测结果会被客户端资源争抢污染。

**真机执行方案（PENDING，按此复跑）**：

```bash
# 1) 16 核 Linux 服务端：构建并启动（配置同 §2.1；日志 info 或 debug）
cd example/default && go build -tags sqlite -o /tmp/m7s-server main.go
/tmp/m7s-server -c /tmp/webrtc-2k-load/config.yaml &   # 记录 PID

# 2) 2K 源
cd example/webrtc-2k-perf && PRESET=ultrafast DURATION=0 ./push-2k.sh

# 3) 50 个 batchv2 观众（建议分布在多台客户机，每台 5–10 页）：
#    每页打开 http://<server>:8080/webrtc/test/batchv2
#    → Connect → Get Stream List → 选中 live/2k → Play Selected；
#    或直接复用 /webrtc/batchv2 WebSocket 协议（plugin/webrtc/web/BatchV2Client.js）写 50 个客户端。
#    每页/每客户端注入 play-collect.js 采集 getStats。

# 4) 服务端采样（与本次同口径）：
pidstat -p <pid> 1 600          # 或 ps -o %cpu=,rss=,time= -p <pid> 每 5 s
curl -s http://127.0.0.1:8080/api/stream/list   # 每 5–10 s 记录 subscribers / bps_out
# 全程 tail 服务端日志，统计 ERROR/WRN 速率、是否出现 panic、连接是否重连循环

# 5) 判据（A5）：subscribers 稳定 50；无 panic/连接崩溃；无错误风暴
#    （建议门槛：连接类 ERROR < 1 条/s 且无持续重连；10 分钟窗口内订阅者不下跌）。
#    6) 结束后清理：kill 50 个客户端 → 确认 subscribers=0、无残留 Chrome/WS。
```

**注意**：不要用 `/api/metrics` 作为本压测的采样端点——本构建存在视频-only 流触发 panic 的缺陷（§7），会直接杀死服务端。用 `/api/stream/list` 的 `subscribers` 与 `videoTrack.bps_out` 即可。

## 6. 限制与诚实性声明

1. **观众同机**：10 个 headless Chrome 与服务端、ffmpeg 编码器共享 12 逻辑核；客户端帧率受解码/调度影响（运行 B 已实证），服务端 CPU 也可能被抢而偏低。10 观众数据只证明“服务端能稳定送出 10×10 Mbps”，不等于生产观众质量。
2. **12 核 vs 16 核**：A4 的 16 核 ≤50% 未实测，报告中的 16 核数字是算术归一化；不做通过判定。50 观众 A5 PENDING。
3. **headless Chrome**：是否走硬件解码（VideoToolbox）未验证；运行 A/B 的差异说明同机客户端内部调度会显著影响帧率。真实有头浏览器/真实设备仍 PENDING。
4. **合成源**：`testsrc2` 恒定 10 Mbps，非真实摄像头码率波动；仅验证传输/解码链路。
5. **窗口长度**：每轮 60 s，未做 10–30 分钟长稳；RSS 跨轮次上升（330 → 538 → 558 MB，进程未重启）可能包含 Go 堆保留，未做内存长稳判定。
6. **采样口径**：`jitterBufferDelayAvg` 是累计平均（冷启动大延迟会拖高均值）；`framesDropped` 是接收端丢弃，不是网络丢包（两轮 NACK 增量均为 0，`packetsLost` 无新增）；客户端接收码率由首末 `bytesReceived` 差值计算，忽略 RTP 头开销。
7. **batchv2 sanity**：1 页面 2 路流、45 s，只作通路 sanity，不是 50 观众 A5；其 avgFps 22.18 包含冷启动，不能作为性能结论。
8. **服务端统计**：`bps_out` 是所有订阅者 reader BPS 之和（有效载荷口径），与网卡实际出向（lo0 119/115 Mbps，含 RTMP 推流与协议开销）不同。

## 7. 观测到的产品缺陷（不在本任务范围，未改代码）

### 7.1 `/api/metrics` 在视频-only 流上 panic（实测）

首次准备采样时对 `live/2k`（无音轨）请求 `GET /api/metrics`，服务端立即崩溃：

```
panic: runtime error: invalid memory address or nil pointer dereference
[signal SIGSEGV: segmentation violation code=0x1 addr=0x48 pc=0x10268b0d3]

goroutine 218 [running]:
m7s.live/v5.(*Server).Collect-range1(0x25a4b8220488)
	.../prometheus.go:82 +0x253
m7s.live/v5/pkg/util.(*Collection[...]).Range(...)
	.../pkg/util/collection.go:91 +0xd0
m7s.live/v5.(*Server).Collect(...)
	.../prometheus.go:79 +0x779
github.com/prometheus/client_golang/prometheus.(*Registry).Gather.func1()
```

崩溃日志保留在 `/tmp/webrtc-2k-load/fatal/2026-11-18 22:38:10.log`。原因是 `prometheus.go:82` 直接访问 `stream.AudioTrack.AVTrack.BPS`，视频-only 流的 `AudioTrack` 为 nil；Prometheus 抓取线程 panic 会杀死整个进程。**本报告全部数据来自重启后的新进程（PID 86643），压测过程未再访问 `/api/metrics`。** 修复方向：`Collect` 内对 `VideoTrack/AudioTrack` 判空（一行防护 + 单测），建议另开任务处理。

### 7.2 日志噪声（无功能影响）

重启后服务端日志（`/tmp/webrtc-2k-load/server.log`）统计：

- 251 条 `Failed to ping without candidate pairs. Connection is not possible yet.`（pion ICE，集中在 10 个观众建连的 1–2 分钟内，稳态为 0）；
- 23 条 `Failed to accept RTP ...` + 23 条 `Failed to accept RTCP ...`、25 条 `rtcp read error ... EOF`（观众断开时预期清理噪声）；
- 4 条 `Failed to send mDNS packet ... no route to host`（容器/接口环境相关）；
- 2 条 `ERR WebSocket read error ... ws closed: 1001`（batchv2 页面主动关闭）；另有启动期与 WebRTC 压测无关的 WebTransport `context canceled`、GB28181 未配置提示各 1 条；
- **无 panic**（重启后）、无连接崩溃、无错误风暴。

## 8. 原始产物（未提交，均在 `/tmp`）

| 内容 | 路径 |
|---|---|
| 服务端配置 / 二进制 / PID | `/tmp/webrtc-2k-load/config.yaml`、`m7s-server`、`server.pid` |
| 服务端日志 / 崩溃日志 | `/tmp/webrtc-2k-load/server.log`、`/tmp/webrtc-2k-load/fatal/2026-11-18 22:38:10.log` |
| 10 观众（运行 A）/ 运行 B | `aggregate-10v.json`、`summaries-10v.json`、`records-10v.json`、`server-samples-10v.json`、`console-10v.log`；`*-10v-run2.*` |
| 单观众参照 | `aggregate-smoke.json`、`server-samples-smoke.json` 等 `*-smoke.*` |
| batchv2 sanity | `batchv2-sanity.json`、`console-batchv2.log` |
| 源日志 | `push-2k.log`、`push-720p.log` |
| 临时 runner（不提交） | `/tmp/webrtc-2k-load/run-load.js`、`run-batchv2.js` |

## 9. 清理

测量结束后已停止 2K/720p 源、服务端与所有 Playwright/headless Chrome：`8080/8554/1935` 无监听、无 `m7s-server`/`ffmpeg`/`node run-load.js` 残留。仓库未提交临时脚本或 `/tmp` 数据；本任务仅新增 `example/webrtc-2k-perf/report-load.md`。
