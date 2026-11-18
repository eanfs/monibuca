# RTSP 拉流兼容性验证（WebRTC 2K@24 升级 Task 5）

- 采集日期：2026-11-18（UTC+8）
- 被测版本：工作树 `dsh-39c7ba14-fa26`
  - 首次测量 HEAD `6448b9bf`（后提交为 `724263e3`）
  - **默认 GCC 复测（Fix round 1）HEAD `170fe160`**
  （pion/webrtc v4.2.26、pion/interceptor v0.1.51，Task 1–4 已合入）
- 用途：验证 **RTSP 拉流代理 → 2K 流 → WebRTC 播放** 链路，为 A9/A10 提供实测数据。
- 关联脚本：`rtsp-pull.sh`（本目录）、Task 0 的 `push-2k.sh` / `play-collect.js`。

## 1. 结论摘要

| 项 | 结论 | 证据 |
|---|---|---|
| RTSP 拉流代理建立 | **通过** | `pullOnStart=true` 创建后 1–2s 内状态 `Pulling`，输出流 `live/rtsp2k` 为 2560×1440 avc1 |
| 单路 2K 60s WebRTC 播放（默认 GCC） | **通过（Fix round 1 复测）** | 复跑 avgFps **23.65**（断言 PASS），丢帧 32、NACK 0、PLI 8、RTT 1ms；稳态（t≥25s）**24.0 fps**，详见 §4.1 |
| 断流重连（A10 的可用性口径） | **通过（有限口径）** | 源流 217ms 内消失；重启后 0.29s 源流恢复、**0.38s** 拉流可用（默认 GCC） |
| 10 路并发 / 10 分钟长稳 | **待真机** | 本机为 macOS、单路 x264 编码器，按计划留到 Linux 真机 |
| 默认配置（GCC 开启）下播放 | **通过（原阻断已修复）** | `missing transport layer cc header extension` 计数 **0**（修复前单次累计 73,327），见 §4.1 |

**关键限定：**
1. 本验证的 RTSP 源与 RTSP 服务是 **同一个 Monibuca 实例**（RTMP 推入 → 同进程
   RTSP 插件作为服务端 → RTSP 拉流代理再拉回），属于控制回路 sanity check，
   **不等价于独立摄像头/GOP/网络环境的真机验收**。
2. §5 的 WebRTC 指标是**首次测量时**用 `WEBRTC_CC_ENABLE=false` 隔离出的口径。
   默认 `webrtc.cc.enable=true` 当时存在出包被 GCC pacer 全部丢弃的阻断项（§4），
   现已修复（`170fe160`）。**默认 GCC 配置的复测结果见 §4.1**，结论为：视频正常出帧、
   pacer 错误 0、稳态 24.0 fps；60s 窗口含冷启动时 avgFps 在 22.7–23.65 间波动，
   原因为 GCC 初始码率 2 Mbps 对 10 Mbps 2K 流的启动排队（见 §4.1.4）。

## 2. 环境

| 项 | 值 |
|---|---|
| 主机 | macOS 26.7.3（Build 25G342），Intel Core i7-9750H @ 2.60GHz，12 逻辑核，16 GB RAM |
| 服务端 | `example/default`，`go build -tags sqlite` 后运行二进制；Go 1.27.4 darwin/amd64 |
| 端口 | HTTP `:8080`、RTMP `:1935`、RTSP `:8554`（TLS `:8322`）、WebRTC `udp:9000-9100` |
| 推流端 | ffmpeg 7.1.1（libx264），`testsrc2` 合成源，PRESET=ultrafast |
| 播放端 | Google Chrome 158.0.8127.36（Playwright 1.56.0，`channel: 'chrome'`，headless），Node v24.16.0 |
| 网络 | 全本机回环（127.0.0.1），无 `tc netem` |
| 容器 | 未使用 Docker |

说明：
- 非 root 无法绑定 RTSP 默认 `:554/:322`，按 Task 0 的做法用环境变量覆盖为
  `RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322`。
- 启动日志仍有与本次无关的 `S3 unavailable, recording disabled until recovery
  (errorCategory=unsupported_build)`（只带 `sqlite` tag，未带 `s3`）。
- 播放页控制台有一条静态资源 404（两组成败运行都出现，未影响媒体；未进一步定位）。

## 3. 方法

### 3.1 服务端

```bash
cd example/default
go build -tags sqlite -o /tmp/webrtc-2k-rtsp/m7s-server .
# 首次测量口径：GCC 关闭，隔离当时未修复的 §4 阻断项；
# 默认 GCC 的复测命令见 §4.1.2。
RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 WEBRTC_CC_ENABLE=false \
  /tmp/webrtc-2k-rtsp/m7s-server -c config.yaml
```

### 3.2 本机 RTSP 源策略（无摄像头）

Monibuca 的 RTSP 插件同时是 RTSP 服务端，因此不需要外部摄像机：

1. `push-2k.sh` 经 RTMP 把 2560×1440@24 推到 `live/source2k`；
2. RTSP 插件在 `:8554` 提供该流的 RTSP 播放地址；
3. 通过 `/api/proxy/pull/add` 建 RTSP 拉流代理，把
   `rtsp://127.0.0.1:8554/live/source2k` 拉成 `live/rtsp2k`；
4. WebRTC 播放 `live/rtsp2k` 并采集 `getStats()`。

该链路真实经过 RTSP DESCRIBE/SETUP/PLAY 与 RTP 收流（服务端日志有
`connect url=rtsp://127.0.0.1:8554/live/source2k userAgent=monibucav5.0.0`、
`subscribe live/source2k`、`create live/rtsp2k ... codec=avc1 width=2560 height=1440`），
但源和 RTSP 服务同进程，不能替代真实摄像头的行为（GOP、时间戳跳变、音频、网络抖动）。

### 3.3 拉流脚本

`rtsp-pull.sh` 封装了全部步骤（`start/status/play/reconnect/stop`），默认参数：

```bash
API_BASE=http://127.0.0.1:8080
RTSP_URL=rtsp://127.0.0.1:8554/live/source2k
STREAM_PATH=live/rtsp2k
SOURCE_RTMP_URL=rtmp://127.0.0.1/live/source2k
START_SOURCE=1
PLAY_SECONDS=60
RECONNECT_WAIT=5
PLAY_URL_BASE=http://127.0.0.1:8080/webrtc/test/subscribe
PRESET=ultrafast
```

实际执行的创建请求：

```bash
curl -s -X POST http://127.0.0.1:8080/api/proxy/pull/add \
  -H 'Content-Type: application/json' \
  -d '{"name":"rtsp-2k-pull","pullURL":"rtsp://127.0.0.1:8554/live/source2k",
       "streamPath":"live/rtsp2k","type":"rtsp","pullOnStart":true,
       "stopOnIdle":false,"audio":true}'
```

`start` 输出（本次运行，代理 ID 因重复 start 清理而递增，测量时用的是 id=3）：

```
[rtsp-pull] 启动 2K 推流：RTMP_URL=rtmp://127.0.0.1/live/source2k PRESET=ultrafast
[rtsp-pull] 源流就绪：live/source2k（推流 PID=24098）
[rtsp-pull] 拉流代理已创建：rtsp://127.0.0.1:8554/live/source2k -> live/rtsp2k
[rtsp-pull] 拉流就绪：proxyId=3 status=2(Pulling) stream=live/rtsp2k subscribers=0 video=2560x1440 codec=avc1
[rtsp-pull] start 完成，耗时 1795ms
```

拉流代理状态（`GET /api/proxy/pull/list`，节选）：

```json
{
  "ID": 3, "name": "rtsp-2k-pull", "type": "rtsp", "status": 2,
  "pullURL": "rtsp://127.0.0.1:8554/live/source2k",
  "pullOnStart": true, "stopOnIdle": false, "audio": true,
  "streamPath": "live/rtsp2k", "checkInterval": "10s", "readTimeout": "10s"
}
```

状态码语义：0=Offline、1=Online、2=Pulling、3=Disabled。播放期间观察为 `Pulling`。

### 3.4 播放与采集

```bash
./rtsp-pull.sh play    # 内部用 Playwright 注入 play-collect.js 采样 60s
```

播放页：`http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/rtsp2k`。
采集间隔 5s，`getStats()` 字段与 Task 0 相同；脚本断言 `avgFps >= 23.5`。

### 3.5 CPU / 内存

播放期间每 2s 用 `ps -o rss= -o %cpu= -p <pid>` 采样服务端与 ffmpeg
（macOS 的 `%cpu` 是单核百分比，100% = 1 核；为进程生命周期衰减平均值）。

## 4. 阻断项：默认 GCC 开启时 WebRTC 出包全部失败（已修复）

> **状态：已修复（Fix round 1，`170fe160`）**。修复与默认 GCC 复测见 §4.1；
> 下面保留首次发现时的现象与定位作为背景。

**现象（默认配置，`webrtc.cc.enable=true`）：**
- 播放页 ICE `connected`、收到两个 `ontrack`（视频+音频），但 `video.videoWidth`
  始终为 0，30s 内无任何可解码帧；
- 服务端日志刷屏：

  ```
  pacer ERROR: 2026/11/18 19:53:53 failed to write packet: missing transport layer cc header extension
  ```

- 计数：RTSP 拉流播放尝试约 35s 产生 **31,089** 条 pacer ERROR；
  随后直接播放 `live/source2k`（不经 RTSP）再产生 29,797 条，累计 **60,886** 条；
  第一次（19:53–19:59）的服务端日志共 **73,327** 条。
- **直接 RTMP 流同样失败**，因此该阻断与 RTSP 拉流无关，是 WebRTC 发送路径的问题。
- 服务端 answer SDP **包含** `a=extmap:4 http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01`
  与 `a=rtcp-fb:<pt> transport-cc`，Chrome offer 也包含同一 extmap，可排除协商缺失。

**代码级定位（未修改，供后续任务确认）：**
- `plugin/webrtc/pkg/util.go` 的 `createInterceptorRegistry` 只注册了
  `cc.NewInterceptor` + `RegisterDefaultInterceptors`，**没有注册
  `twcc.NewHeaderExtensionInterceptor()`**（负责在出站 RTP 头写入 transport-wide-cc 序号）。
- `pion/interceptor@v0.1.51/pkg/gcc/send_side_bwe.go:155-172`：GCC 从
  `StreamInfo` 取到 transport-cc 的 ext id 后，在 `OnSent` 中直接
  `header.GetExtension(extID)`；没有写扩展的拦截器时返回
  `errMissingTWCCExtension`（`internal/cc/feedback_adapter.go`），
  `pkg/gcc/leaky_bucket_pacer.go:176` 记录错误并丢弃该包。
- 建议修复方向：在 GCC 拦截器之外补注册 `twcc.NewHeaderExtensionInterceptor()`，
  并加回归测试；修复后必须用默认配置复跑本报告 §3 的 60s 用例。

**本次测量口径：** 以 `WEBRTC_CC_ENABLE=false` 启动服务端（配置系统支持
`插件名_字段路径` 环境变量覆盖，实测该变量生效：pacer ERROR 计数为 0，
视频满帧）。这只是为了隔离 RTSP 拉流路径，不代表默认配置已验收。

### 4.1 修复与默认 GCC 配置复测（Fix round 1）

#### 4.1.1 代码修复

`plugin/webrtc/pkg/util.go` 的 `createInterceptorRegistry` 在 `cc.enable=true` 分支里，
`i.Add(ccInterceptor)` 之后、`RegisterDefaultInterceptors` 之前补调用
`webrtc.ConfigureTWCCHeaderExtensionSender(m, i)`（顺序与 pion
`bandwidth-estimation-from-disk` 示例一致：cc → TWCC header extension → defaults）。
该函数注册 `twcc.NewHeaderExtensionInterceptor()`，在出站 RTP 包上写入
transport-wide-cc 序号；顺序必须在 defaults 之前（`interceptor.Chain` 中
后注册者包装先注册者，所以 header extension 会先写入扩展，再由 cc pacer 的
`OnSent` 读取）。`cc.enable=false` 分支不变。
回归测试 `plugin/webrtc/pkg/util_test.go` 钉住链上为 cc + header extension 两个
拦截器、位置分别为 `ai[0]`/`ai[1]`，且 `ai[2:]` 与无 cc 链逐项一致。

#### 4.1.2 复测方法

服务端按默认配置启动（**不设** `WEBRTC_CC_ENABLE`，`webrtc.cc.enable=true`、
`initialbitrate=2000000`、`minbitrate=300000`、`maxbitrate=20000000`）：

```bash
cd example/default
RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go
```

```bash
cd example/webrtc-2k-perf
OUT_DIR=/tmp/webrtc-2k-rtsp-fix ./rtsp-pull.sh start
OUT_DIR=/tmp/webrtc-2k-rtsp-fix PLAY_SECONDS=60 ./rtsp-pull.sh play
```

#### 4.1.3 复测结果

**默认 GCC 下 60s 播放（`OUT_DIR=/tmp/webrtc-2k-rtsp-fix`）：**

| 运行 | avgFps | 解码帧/span | 丢帧 | NACK | PLI | jitter buffer avg | 断言 |
|---|---|---|---|---|---|---|---|
| 首跑（`summary.json`，start 后立即测量） | 17.91 | 985 / 55s | 273 | 0 | 19 | 559 ms | FAIL（冷启动，见 §4.1.4） |
| **复跑 1（`summary-2.json`）** | **23.65** | 1301 / 55s | 32 | 0 | 8 | 240 ms | **PASS** |
| 复跑 2（`summary-3.json`） | 22.71 | 1249 / 55s | 100 | 0 | 8 | 388 ms | FAIL |
| 复跑 3（`summary-4.json`） | 23.24 | 1278 / 55s | 63 | 0 | 7 | 412 ms | FAIL |
| 复跑 4（`summary-5.json`） | 23.15 | 1273 / 55s | 71 | 0 | 7 | 400 ms | FAIL |
| 同机 A/B：`WEBRTC_CC_ENABLE=false` | 24.00 | 1320 / 55s | 0 | 0 | 0 | 8.4 ms | PASS |

- 所有默认 GCC 运行的 `packetsLost` 均为 **0**，RTT 0.001–0.002s，接收码率稳态
  **9.9–10.1 Mbps**（与 cc-off 口径一致）。
- 以 `t ≥ 25s` 计算稳态帧率：24.00 / 23.83 / 24.14 / 24.09 fps —— **默认 GCC 稳态满帧**。
- **日志检查（必须两项都为 0）：**

  ```bash
  grep -c "missing transport layer cc header extension" /tmp/webrtc-2k-rtsp-fix/server.log   # 0
  grep -c "pacer ERROR" /tmp/webrtc-2k-rtsp-fix/server.log                                   # 0
  ```

  修复前同一用例单次累计 **73,327** 条；修复后默认 GCC 下累计 **0**，帧可正常解码。

**默认 GCC 下断流重连（`./rtsp-pull.sh reconnect`）：**

| 指标 | 值 |
|---|---|
| 源流消失（kill 后） | **217 ms** |
| 断流总时长（含刻意 5s） | 5.116 s |
| 源流重新出现 | **0.29 s**（自重启起） |
| 拉流可用 | **0.38 s**（自重启起，代理始终 `Pulling`） |
| 恢复后 25s 复播 | 23.0 fps（460 帧 / 20s），丢帧 37，NACK 0，PLI 6，RTT 1ms |

复播 23.0 fps 同样是源重启后的冷启动窗口（见 §4.1.4）；稳态仍在 24 fps 附近。

#### 4.1.4 遗留现象：默认 GCC 的冷启动帧率下探

- 默认 `initialbitrate=2000000`，而 2K@24 源约 **10 Mbps**。新订阅建立时 GCC pacer
  以 2 Mbps 起步，编码器却立即按 10 Mbps 出帧 → pacer 队列在最初 ~10–20s 积压，
  浏览器 jitter buffer 延迟升到 0.2–0.7s、触发 PLI 与丢帧；GCC 升到 ~10 Mbps 后
  （每秒采样显示 10s 内已到 10 Mbps）队列排空，**t≥25s 稳态 24.0 fps、0 丢包**。
- cc-off A/B 无 pacer，故 60s 全程 24.00 fps、jitter buffer 8.4ms、0 PLI。
- 这是**配置初值**问题而非 header extension 修复问题；本次未改默认值（Task 2 已把
  `initialbitrate=2000000` 写进计划与单测）。生产 2K 场景建议把
  `webrtc.cc.initialbitrate` 提到接近源码率（如 8–10 Mbps），或把会话前 ~20s
  视为预热窗口；计划中的 10 分钟长稳测试会摊薄该窗口。

## 5. 结果：单路 RTSP 拉流 2K 60s（首次测量，cc-off 隔离口径）

> 本节是首次测量时为隔离 §4 阻断项而用 `WEBRTC_CC_ENABLE=false` 得到的数据。
> **默认 GCC 配置的复测数据（Fix round 1）以 §4.1.3 为准。**

### 5.1 WebRTC summary（原始 JSON）

```json
{
  "type": "summary", "elapsedSec": 61.57, "intervalMs": 5000, "samples": 12, "errors": 0,
  "peerConnections": [{
    "pc": 1, "connectionState": "connected", "samples": 12, "spanSec": 54.99,
    "decodedFrames": 1320, "avgFps": 24, "sampleFpsMean": 24, "sampleFpsMin": 23.99,
    "framesDropped": 0, "nackCount": 0, "pliCount": 0,
    "jitterBufferDelayAvg": 0.0079, "lastRtt": 0.001
  }]
}
```

| 指标 | 值 |
|---|---|
| 实际分辨率 | **2560×1440**（`frameWidth/frameHeight`，编码 avc1） |
| 连接状态 | `connected`，全程无断连；RTT 0–2ms |
| 平均帧率 | **24.00 fps**（1320 解码帧 / 54.99s），单采样 23.99–24.04 fps |
| 丢帧 | **0**（整个采样窗口） |
| NACK / PLI | summary 增量 **0 / 0**；首条采样累计为 1 / 5（订阅接入等关键帧），之后不再增长 |
| packetsLost | **0**（首条采样即为 0，全程不变） |
| jitter buffer 平均延迟 | **7.9 ms**（累计 11.02s / 60.35s 采样点；各采样 7.8–8.1ms） |
| 接收码率 | `bytesReceived` 增量 67,847,284 B / 54.993s ≈ **9.87 Mbps** |
| 服务端进程 CPU | 平均 **5.7%**（单核百分比），范围 1.9–7.0% |
| 服务端 RSS | 117–204 MB，均值 161 MB（60s 窗口内随堆增长，未做长稳结论） |
| ffmpeg 编码 CPU | 平均 **49.9%**（范围 41.2–59.4%），RSS 约 412 MB |
| `/api/stream/list` | `live/rtsp2k` subscribers=1，服务端 `fps` 字段 23–24，`bps` 均值 1,244,157 **字节/秒 ≈ 9.95 Mbps**；`live/source2k` 同期 subscribers=1（证明 RTSP 拉流确实在消费源流） |

说明：服务端 `videoTrack.bps/bpsOut` 实际单位是**字节/秒**（与 Task 0 基线一致），
不是 bit/s；浏览器侧 9.87 Mbps 与它 9.95 Mbps 的口径差来自采样窗口与 RTP 开销。

### 5.2 稳定性观察

- 60s 内无 `ERR`、无 panic、无 WebRTC 断连；GCC 关闭时 pacer ERROR 为 **0**。
- 播放结束时浏览器主动断开产生正常的 `rtcp read error ... EOF` / `unsubscribe` WRN。
- 日志中 `publish.delayclosetimeout` 等与 RTSP 拉流无冲突。

## 6. 结果：断流重连（A10 可用性口径，cc-off 首次测量）

> 默认 GCC 配置下的重连复测见 §4.1.3（0.38s 拉流可用，217ms 源流消失，结果一致）。

执行 `./rtsp-pull.sh reconnect`（杀源 → 等 5s → 重启同一 `live/source2k`）：

```
[rtsp-pull] 杀掉源进程 PID=72052（rtmp://127.0.0.1/live/source2k）
[rtsp-pull] 断流观测：源流消失=1（216ms 后）live/rtsp2k present=1 videoBps=1224967
[rtsp-pull] 源已重启：PID=89881，开始计时（断流总时长 5138ms）
[rtsp-pull] 源流已重新出现：0.28s（自重启起）
[rtsp-pull] 拉流可用：耗时 0.37s（proxyId=3 status=2(Pulling) stream=live/rtsp2k video=2560x1440）
```

| 指标 | 值 |
|---|---|
| 源流消失耗时（kill 后） | **216 ms** |
| 断流总时长（含刻意 5s 等待） | **5.138 s** |
| 源流重新出现 | **0.28 s**（自重启 ffmpeg 起） |
| 拉流可用（API 口径） | **0.37 s**（自重启起），代理状态始终 `Pulling` |
| 断流期间 `live/rtsp2k` | **present=1**（publisher 未消失；`videoBps` 字段仍是断流前的移动平均值，不能作为数据流证据） |
| 恢复后 25s WebRTC 复播 | **24.00 fps**（480 帧 / 20s），丢帧 0，NACK/PLI 增量 0，jitter buffer 9.7ms，RTT 1ms |

**重要限定：**
- 该结果证明 **RTSP 拉流代理能在源恢复后自动恢复出流**，且恢复后画面满帧。
- 由于 `live/rtsp2k` 的 publisher 在源断流期间不消失，本脚本测的是“源流恢复 + 代理在线 +
  拉流 present”的**可用性**，**不是观众画面的连续恢复**。观众跨断流是否无感、是否需要
  重新订阅，属于 A10 待真机项（见 §7）。
- 服务端日志显示 RTSP 会话在源 publisher 消失后保持，源恢复时走
  `publisher recover live/source2k sId=80 pid=85`，因此代理不需要重建。

## 7. 待真机项与执行命令

以下项目本机无法给出有效结论，**不标记为通过**：

| 项 | 本机限制 | Linux 真机命令 |
|---|---|---|
| **10 路并发**（Task 5 Step 3） | macOS 无 `tc`；单路 x264 已占约 0.5 核，10 路源编码会先压垮本机 | 见下方 A |
| **10 分钟长稳**（Step 2） | 只做了 60s；默认 GCC 冷启动窗口（§4.1.4）需靠长窗口摊薄 | `PLAY_SECONDS=600 OUT_DIR=/tmp/rtsp-600 ./rtsp-pull.sh play` |
| **真实摄像头 / 真实 GOP** | 源是同进程 `testsrc2`，GOP=48 固定，无音频/无网络抖动 | `START_SOURCE=0 RTSP_URL='rtsp://user:pass@cam:554/stream' STREAM_PATH=live/cam2k ./rtsp-pull.sh start` 后 `PLAY_SECONDS=600 ./rtsp-pull.sh play` |
| **默认 GCC 冷启动帧率** | 60s 窗口含 2 Mbps→10 Mbps 爬升期，avgFps 22.7–23.65；稳态已 24.0 | 调高 `webrtc.cc.initialbitrate`（如 8–10 Mbps）后复跑 `play`，或接受前 ~20s 预热 |
| **观众跨断流连续播放** | 本脚本只做恢复后复播 | 后台 `PLAY_SECONDS=120 ./rtsp-pull.sh play &`，20s 后执行 `./rtsp-pull.sh reconnect`，观察播放端是否自行恢复 |
| **弱网（1%/3% 丢包、RTT 50/150ms）** | macOS 无 `tc netem` | Linux 上用 `tc qdisc add dev lo root netem loss 1%` 后复跑 `play` |

**A. 10 路拉流代理（不同 streamPath，复用同一个 2K 源）：**

```bash
# 终端 1：服务端（Linux 上 RTSP 可直接用默认 554，也可沿用 8554）
cd example/default
go run -tags sqlite main.go

# 终端 2：只起一路 2K 源
cd example/webrtc-2k-perf
RTMP_URL=rtmp://127.0.0.1/live/source2k DURATION=0 PRESET=ultrafast ./push-2k.sh &

# 终端 3：10 路拉流代理
cd example/webrtc-2k-perf
for i in $(seq 1 10); do
  START_SOURCE=0 STREAM_PATH="live/rtsp2k-$i" OUT_DIR="/tmp/rtsp-pull-$i" \
    RTSP_URL=rtsp://127.0.0.1:8554/live/source2k ./rtsp-pull.sh start
done

# 观测 RSS/线程/连接数（Linux）
for i in $(seq 1 30); do
  ps -o rss=,nlwp= -p "$(pgrep -f m7s-server | head -1)"
  ss -tnp | grep -c ':8554'
  sleep 10
done
```

判据：10 路全部 `Pulling`；进程 RSS 无持续单调上涨；无 pacer/RTSP 错误风暴；
`ss` 的 8554 连接数与代理数一致。

## 8. 复现命令（本次实际执行）

```bash
# 0) 构建并启动（默认 GCC 配置，Fix round 1 复测口径；不设 WEBRTC_CC_ENABLE）
cd example/default
RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go

# 1) 起源 + 拉流代理
cd example/webrtc-2k-perf
OUT_DIR=/tmp/webrtc-2k-rtsp-fix ./rtsp-pull.sh start
OUT_DIR=/tmp/webrtc-2k-rtsp-fix ./rtsp-pull.sh status

# 2) 60s 播放 + 断言
OUT_DIR=/tmp/webrtc-2k-rtsp-fix PLAY_SECONDS=60 ./rtsp-pull.sh play

# 3) 断流重连
OUT_DIR=/tmp/webrtc-2k-rtsp-fix ./rtsp-pull.sh reconnect
OUT_DIR=/tmp/webrtc-2k-rtsp-fix PLAY_SECONDS=25 \
  SUMMARY_OUT=/tmp/webrtc-2k-rtsp-fix/summary-post-reconnect.json \
  SAMPLES_OUT=/tmp/webrtc-2k-rtsp-fix/samples-post-reconnect.jsonl \
  CONSOLE_LOG=/tmp/webrtc-2k-rtsp-fix/console-post-reconnect.log \
  ./rtsp-pull.sh play

# 4) 日志检查（都必须为 0）
grep -c "missing transport layer cc header extension" /tmp/webrtc-2k-rtsp-fix/server.log
grep -c "pacer ERROR" /tmp/webrtc-2k-rtsp-fix/server.log

# 5) 清理
OUT_DIR=/tmp/webrtc-2k-rtsp-fix ./rtsp-pull.sh stop
```

> 首次测量时（§4 阻断未修复）用过的隔离命令：
> `WEBRTC_CC_ENABLE=false RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 go run -tags sqlite main.go`。
> 修复后无需该覆盖。

脚本行为要点：
- `start` 会先清理同 `streamPath` 的旧代理；若上次 `start` 的推流进程仍在，会复用其 PID；
- `stop` 通过 `/api/proxy/pull/remove/<id>` 删除代理，只杀状态文件记录且命令行匹配
  `SOURCE_RTMP_URL` 的推流进程，不误杀无关进程；
- `play` 在 Playwright 不可用时直接报错退出，不会静默通过；断言失败会打印实际
  `avgFps` 并以非零码退出；
- `reconnect` 只管理由本脚本启动的源（`START_SOURCE=1`），外部摄像头源需手工重启并计时。

## 9. 原始数据与清理

未提交的原始数据（本机 `/tmp`）：

- `/tmp/webrtc-2k-rtsp/`：首次测量（cc-off 口径）的 summary/samples/console、
  重连计时、CPU/RSS、GCC 开启的失败证据日志、SDP dump；
- `/tmp/webrtc-2k-rtsp-fix/`（Fix round 1，默认 GCC 复测）：
  - `summary.json` / `summary-2.json` … `summary-5.json`、`samples*.jsonl`、`console*.log`：
    5 次 60s 运行（首次 + 4 次复跑）；
  - `server.log`：默认 GCC 服务端日志（两项错误计数均为 0）；
  - `reconnect-result.json`、`summary-post-reconnect.json`、`samples-post-reconnect.jsonl`：
    默认 GCC 的重连与恢复后 25s 复播；
  - `summary-cc-off-ab.json`、`server-cc-off.log`：同机 A/B（cc-off，24.00 fps 对照）。

清理：已执行 `./rtsp-pull.sh stop`（代理已从 `/api/proxy/pull/list` 消失），
停止 ffmpeg/Playwright/服务端进程，删除 `example/default` 下的 `m7s.db`、`logs/`、
`fatal/`、`pending_uploads/`、`shutdown.sh` 及仓库根目录误生成的同类文件；
`git status` 仅显示本任务的报告修改。
Fix round 1 修改了 `plugin/webrtc/pkg/util.go` 与 `plugin/webrtc/pkg/util_test.go`
（提交 `170fe160`），未修改 `go.mod`、`docs/` 或 Task 0 脚本。
