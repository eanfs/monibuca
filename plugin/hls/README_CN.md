# HLS 插件指南

## 目录

- [简介](#简介)
- [核心特性](#核心特性)
- [配置示例](#配置示例)
- [直播播放接口](#直播播放接口)
- [拉取远端 HLS 流](#拉取远端-hls-流)
- [录像与点播](#录像与点播)
- [内置播放器资源](#内置播放器资源)
- [低延迟 HLS（LL-HLS）](#低延迟-hlsll-hls)
- [故障排查](#故障排查)

## 简介

HLS 插件让 Monibuca 具备完整的 HTTP Live Streaming 产出能力：实时切片、内存缓存、录像、下载、代理等功能一应俱全。它会在 `/hls` 下暴露标准的 `.m3u8` 播放列表，支持携带时间范围参数播放历史录像，还能将外部 HLS 流拉回 Monibuca。插件自带打包好的 `hls.js` 演示页面，可直接验证播放效果。

## 核心特性

- **直播 HLS 输出**：任何进驻 Monibuca 的流都可以在 `http(s)://{host}/hls/{streamPath}.m3u8` 访问，分片长度与窗口可配置。
- **内存缓存**：默认配置 `5s x 3`，即 5 秒分片、保留 3 个完整分片，形成约 15 秒滚动缓存，可按需调整。
- **远端 HLS 拉流**：拉取外部 `.m3u8`，重新封装 TS 分片并再次发布。
- **录像流程**：可录制 TS，支持通过配置或 REST API 启停，并导出 TS/MP4/FMP4 文件。
- **时间段下载**：根据录制记录拼接 TS，若不存在 TS 则自动将 MP4/FMP4 转码为 TS 并下发。
- **点播播放列表**：`/vod/{streamPath}.m3u8` 支持 `start`、`end`、`range` 等查询参数，快速构建点播列表。
- **自带播放器**：插件打包常用的 `hls.js` Demo（benchmark、metrics、light 等），无需额外托管即可测试。

## 配置示例

```yaml
hls:
  onpub:
    transform:
      ^live/.+: 5s x 3        # 分片时长 x 播放列表窗口
    record:
      ^live/.+:
        filepath: record/$0
        fragment: 1m          # 每 1 分钟切一个 TS 文件
  pull:
    live/apple-demo:
      url: https://devstreaming-cdn.apple.com/.../prog_index.m3u8
      relaymode: mix          # remux（默认）、relay 或 mix
  onsub:
    pull:
      ^vod_hls_\d+/(.+)$: $1  # 拉取按需点播资源
```

### Transform 的输入格式

`onpub.transform` 接受字符串 `{分片时长} x {窗口大小}`，例如 `5s x 3` 表示生成 5 秒分片并保留 3 个已完成分片（外加一个进行中的分片）。窗口越大，类 DVR 时长越长；窗口越小，首屏延迟越低。

### 录像参数

`onpub.record` 复用全局的 `config.Record` 结构：

- `fragment`：TS 文件时长（`1m`、`10s` 等）。
- `filepath`：存储目录，`$0` 会替换为匹配到的流路径。
- `realtime`：`true` 代表实时写入文件；`false` 表示先写入内存，分片结束后再落盘。
- `append`：是否向已有文件追加内容。

也可通过 HTTP 接口动态开启或停止录像，详见[录像与点播](#录像与点播)。

### 拉流参数

`hls.pull` 使用 `config.Pull`，常用字段如下：

- `url`：远端播放列表 URL（建议 HTTPS）。
- `maxretry`、`retryinterval`：断开重试策略（默认每 5 秒重试一次）。
- `proxy`：可选的 HTTP 代理。
- `header`：自定义请求头（Cookie、Token、UA 等）。
- `relaymode`：TS 处理方式：
  - `remux`（默认）—— 只解复用 TS，按 Monibuca 内部轨道重新封装。
  - `relay` —— 保留原始 TS 分片，跳过解复用/录制。
  - `mix` —— 既重新封装供播放，又保留 TS 分片，便于下游复用。

## 直播播放接口

- `GET /hls/{streamPath}.m3u8`：实时播放列表，可携带 `?timeout=30s` 等参数等待发布者上线（等待期间插件会自动内部订阅）。
- `GET /hls/{streamPath}/{segment}.ts`：TS 分片，会从内存缓存或磁盘读取。
- `GET /hls/{resource}`：访问 `hls.js.zip` 中的静态资源，例如 `index.html`、`hls.js`。

若使用标准端口监听，插件会自动在 `PlayAddr` 中登记 `http://{host}/hls/live/demo.m3u8` 与 `https://{host}/hls/live/demo.m3u8` 等地址。

## 拉取远端 HLS 流

1. 在配置中新增 `pull` 项，或调统一接口：
   ```bash
   curl -X POST http://localhost:8080/api/stream/pull \
     -H "Content-Type: application/json" \
     -d '{
       "protocol": "hls",
       "streamPath": "live/apple-demo",
       "remoteURL": "https://devstreaming-cdn.apple.com/.../prog_index.m3u8"
     }'
   ```
2. 拉流器会抓取主播放列表，必要时跟进多码率分支，下载最新 TS 分片并在 Monibuca 中重新发布。
3. 若需要最大限度保留原始 TS，可设置 `relaymode: mix`；插件会在 `MemoryTs` 中维持滚动分片缓存。

任务进度可通过任务系统查看，关键步骤名称包括 `m3u8_fetch`、`parse`、`ts_download`。

## 录像与点播

### REST 启停

- `POST /hls/api/record/start/{streamPath}?fragment=30s&filePath=record/live`：启动 TS 录像，返回任务指针 ID。
- `POST /hls/api/record/stop/{id}`：停止录像（`id` 为启动接口返回的数值）。

如果同一 `streamPath` + `filePath` 已存在任务会返回错误；插件会在启用数据库时将录像元数据写入 `RecordStream` 表。

### 时间范围播放列表

- `GET /vod/{streamPath}.m3u8?start=2024-12-01T08:00:00&end=2024-12-01T09:00:00`
- `GET /vod/{streamPath}.m3u8?range=1700000000-1700003600`

处理逻辑会查询数据库中类型为 `ts`、`mp4` 或 `fmp4` 的录像记录，返回一个指向文件路径或 `/mp4/download/{stream}.fmp4?id={recordID}` 的播放列表。

### TS 下载接口

`GET /hls/download/{streamPath}.ts?start=1700000000&end=1700003600`

- 如果存在 TS 录像，会拼接多个分片，并在首个文件后跳过重复 PAT/PMT。
- 若只有 MP4/FMP4，插件会调用 MP4 解复用器，将样本转为 TS 实时输出。
- 响应包含 `Content-Type: video/mp2t` 与 `Content-Disposition: attachment`，方便浏览器直接下载。

## 内置播放器资源

`hls.js.zip` 中打包的静态文件可直接从 `/hls` 访问，常用入口：

- `/hls/index.html` —— 全功能 Demo。
- `/hls/index-light.html` —— 精简界面版本。
- `/hls/basic-usage.html` —— 入门示例。
- `/hls/metrics.html` —— 延迟、缓冲等指标可视化。

这些页面加载本地的 `/hls/hls.js`，无需外部 CDN 即可测试拉流。

## 低延迟 HLS（LL-HLS）

同目录还注册了 `llhls` 插件，可生成低延迟播放列表。示例：

```yaml
llhls:
  onpub:
    transform:
      ^live/.+: 1s x 7   # SegmentMinDuration x SegmentCount
```

LL-HLS 的访问路径为 `http(s)://{host}/llhls/{streamPath}/index.m3u8`，内部使用 `gohlslib` 将 H.264/H.265 与 AAC 轨道写入低延迟分片，默认总时移小于 2 秒。

## 故障排查

- **播放列表为空**：确认发布者在线，并确保 `onpub.transform` 正则匹配流路径；可在请求中增加 `?timeout=30s` 给予自动订阅时间。
- **分片过快被清理**：增大窗口（例如 `5s x 6`），或在拉流任务中改用 `relaymode: mix` 以延长原始 TS 的保留时长。
- **下载返回 404**：确认已启用数据库并存在对应 `RecordStream` 元数据，插件依赖数据库定位文件。
- **长时间段下载卡顿**：下载流程串行读写，建议拆分时间段或使用更快的存储介质。
- **浏览器跨域访问**：`/hls` 是标准 HTTP GET 接口，跨域访问需自行配置 CORS 或反向代理。


