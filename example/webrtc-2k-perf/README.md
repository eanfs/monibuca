# WebRTC 2K@24 性能基线测量

本目录用于采集 **pion v4.1.4（升级前）** 下 2K（2560×1440@24fps）H264 经 Monibuca
WebRTC 播放的基线数据，作为 `docs/superpowers/plans/2026-11-18-webrtc-2k-upgrade.md`
中 Task 7/8 验收（A1–A3、A9）的对照。

- `push-2k.sh`：用 ffmpeg `testsrc2` 合成 2560×1440@24 H264（约 10 Mbps）推 RTMP。
- `play-collect.js`：浏览器侧每 5s 调用 `pc.getStats()`，单行 JSON 输出关键指标。
- `report-baseline.md`：本机一次实际基线运行的测量结果、方法与局限。

## 前置条件

- Go（能编译 `example/default`）、ffmpeg（含 libx264）、Chrome。
- 默认端口：HTTP 8080、RTMP 1935、WebRTC UDP 9000-9100（见 `example/default/config.yaml`）。
- macOS 上 RTSP 插件的默认端口 `:554/:322` 需要 root，非 root 运行时可覆盖：

  ```bash
  RTSP_TCP_LISTENADDR=:8554 RTSP_TCP_LISTENADDRTLS=:8322 \
    go run -tags sqlite main.go
  ```

  （配置系统支持 `插件名_字段路径` 形式的大写环境变量覆盖。）

## 快速开始

终端 1 —— 启动服务（HTTP 8080）：

```bash
cd example/default
go run -tags sqlite main.go
```

终端 2 —— 推 2K 测试流：

```bash
cd example/webrtc-2k-perf
./push-2k.sh
# 或限定 90 秒：DURATION=90 ./push-2k.sh
# 或改地址：RTMP_URL=rtmp://127.0.0.1:19350/live/2k ./push-2k.sh
```

终端 3 / 浏览器 —— 播放并采集：

- 播放页：`http://127.0.0.1:8080/webrtc/test/subscribe?streamPath=live/2k`
- 采集方式见下节；跑完后执行 `__webrtc2kCollector.stop()`，控制台会打印一行
  `[webrtc-2k] summary {...}`。

## play-collect.js 的三种用法

播放页自带的 `pc` 是异步 IIFE 内的局部变量，页面加载后从控制台拿不到，因此采集器
通过包装 `window.RTCPeerConnection` 来接管连接：

1. **文档开始时注入（推荐，能接管播放页自己创建的 pc）**
   - Playwright：`context.addInitScript({ content: <play-collect.js 内容> })` 之后再
     `page.goto(播放页)`；
   - 浏览器用户脚本（Tampermonkey 等）：设置为 `document-start` 执行。
2. **控制台粘贴后主动建连**（控制台粘贴的代码不会在刷新后保留）：
   ```js
   __webrtc2kCollector.subscribe('live/2k')   // 自己创建 pc 并播放，自动被采集
   ```
3. **显式接管已有对象**（同源任意页面）：
   ```js
   __webrtc2kCollector.attach(pc)
   ```

可配置项（在脚本执行前赋值）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `window.__WEBRTC_2K_INTERVAL_MS__` | `5000` | 采样间隔 |
| `window.__WEBRTC_2K_DURATION_MS__` | `0` | 自动停止时长，0 = 不自动停止 |
| `window.__WEBRTC_2K_LABEL__` | 空 | 写入每条采样的标签 |

API：`stop(reason)`（打印并返回 summary）、`getRecords()`（返回全部采样数组）、
`attach(pc)`、`subscribe(streamPath, opts)`。

## 采样字段说明

| 字段 | 来源 | 说明 |
|---|---|---|
| `framesDecoded` | `inbound-rtp` | 累计解码帧数 |
| `fps` | 相邻两次采样的 `framesDecoded` 差值 / 实际间隔 | 实际渲染前解码帧率 |
| `framesDropped` | `inbound-rtp` | 累计丢帧数（jitter buffer/解码输入丢弃） |
| `jitterBufferDelay` | `inbound-rtp` | **累计**抖动缓冲延迟（秒） |
| `jitterBufferDelayAvg` | `jitterBufferDelay / jitterBufferEmittedCount` | 平均抖动缓冲延迟（秒） |
| `nackCount` | `inbound-rtp` | 接收侧发出的 NACK 累计数（重传请求） |
| `pliCount` | `inbound-rtp` | 接收侧发出的 PLI 累计数（关键帧请求） |
| `currentRoundTripTime` | 选中的 `candidate-pair` | RTT（秒），未取得时为 `null` |
| `packetsLost` / `jitter` | `inbound-rtp` | 累计丢包与 RFC3550 抖动估计 |
| `bytesReceived` | `inbound-rtp` | 累计接收字节数 |
| `width` / `height` | `inbound-rtp` | 协商后的实际分辨率（应为 2560×1440） |

`summary` 中每路 pc 给出：`spanSec`、`avgFps`（按总解码帧/时长）、`sampleFpsMean`、
`sampleFpsMin`、`framesDropped`、`nackCount`、`pliCount`、`jitterBufferDelayAvg`、
`lastRtt`。

## 注意事项

- **本机是 macOS，没有 `tc netem`**，弱网（1%/3% 丢包）验收必须在 Linux 上做；
  本目录脚本不包含弱网能力。
- 设计里的 10 分钟 / 30 分钟长稳验收（A1、A1b、A1c 等）仍待后续任务执行；
  `report-baseline.md` 只记录一次短时长基线。
- `play-collect.js` 只测接收端可观测指标，服务端 CPU 用 `ps` 另外采样。
- 基线数据只在 **pion v4.1.4** 下有效；Task 1 升级依赖后同一脚本可直接复测对比。
