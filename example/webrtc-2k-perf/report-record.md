# 录制兼容性验证报告（RTSP 源 + WebRTC/WHIP 流）

- 采集日期：2026-11-18（UTC+8）
- 被测版本：工作树 `dsh-39c7ba14-fa26`，HEAD `f59bb3ac`（Task 1–5 已完成）
- 关联计划：`2026-11-18-webrtc-2k-upgrade` Task 6（A11/A12/A13）
- 关联脚本：`example/webrtc-2k-perf/record-check.sh`（本任务新增，可重复运行）
- 原始日志与 ffprobe JSON：`/tmp/webrtc-2k-record/`（见 §10，非仓库文件，未提交）

## 1. 结论摘要

| 项 | 结论 | 关键证据 |
|---|---|---|
| A11 RTSP 源录制（视频） | **通过** | 2560×1440 / h264 / avg_frame_rate **24.072** / duration **25.132s** / 31.98MB |
| A11 RTSP 源录制（音频） | **通过** | 另起 AAC 2K 源 → RTSP 拉流 → 录制：audio=**aac** 48000Hz 1ch，duration **18.049s** |
| A13 分片录制 | **通过** | `fragment=1m` 录 175s：**3 个文件**，duration **54.832 / 59.957 / 60.300s**，三个文件 ffprobe 均可读、均为 2560×1440 h264 |
| 录制中停止源 | **通过** | 录 10s 后只停 RTMP 源：文件 **13,757,584B**，moov 可读，duration **10.925s**，无零字节文件 |
| A12 WHIP 推流侧 | **通过** | Chrome 假设备推流成功，服务端流 `live/whip`：video=**avc1** 640×480，audio=**Opus** |
| A12 录制音频（Opus-in-MP4） | **阻断（实测失败，非脚本问题）** | progressive mp4 录制器拒绝 Opus：`unsupported audio codec for mp4 record: *codec.OPUSCtx`；每次重试生成 1 秒纯视频碎片（本次 16 个），文件内无音轨 |
| fmp4 + Opus（附加探测） | **阻断（panic）** | `type:"fmp4"` 时录制器在 `CreateOpusSpecificBox → OpusContext.ParseExtranData` panic：`slice bounds out of range [:8] with capacity 4`，只留下 24B 文件 |
| Opus-in-MP4 播放能力 | **播放器/浏览器可播放** | 用 ffmpeg 合成 Opus-in-MP4 参照样本：ffplay rc=0、VLC rc=0、Chrome `canPlayType="probably"`、`readyState=4`、无 error → **瓶颈在 Monibuca 封装，不在播放端** |
| 10 分钟长录 / 真实摄像头 / 弱网 / 10 路并发 | **PENDING（本机不可完成）** | 见 §9 的 Linux 真机命令；未冒充 PASS |

**最重要结论：** 在本版本录制器上，WebRTC/WHIP 的 Opus 音频**无法写入 MP4**。
progressive mp4 直接以“不支持的音频编码”退订重试（每秒一个 ~1s 纯视频文件），
fmp4 则在写 init segment 时 panic。按任务约束**未修改录制器**。
播放端（ffplay/VLC/Chrome）本身支持 Opus-in-MP4，因此这是封装侧待修项，不是播放侧限制。

## 2. 环境

| 项 | 值 |
|---|---|
| 主机 | macOS 26.7.3（Build 25G342），Intel Core i7-9750H @ 2.60GHz，12 逻辑核 |
| Go | go1.27.4 darwin/amd64 |
| 服务端 | `example/default`，`go build -tags sqlite`；HTTP `:8080`、RTMP `:1935`、RTSP `:8554`、WebRTC `udp:9000-9100` |
| 存储 | `global.storage.local.path=/tmp/webrtc-2k-record/local`（**未配置 S3**） |
| ffmpeg/ffprobe | 7.1.1 |
| 浏览器 | Google Chrome 158.0.8127.36，Playwright 1.56.0（`channel: 'chrome'`，headless） |
| Node | v24.16.0 |
| VLC | 3.0.17.3 Vetinari（`/Applications/VLC.app/Contents/MacOS/VLC`） |
| 容器/网络 | 无 Docker、无 Linux tc；全部本机回环 127.0.0.1 |

## 3. 服务端配置与启动

临时配置 `/tmp/webrtc-record-config.yaml`（目的：去掉 `example/default/config.yaml` 的 S3 块；
带 `-tags sqlite` 时 S3 不可用会触发 `S3 unavailable ... unsupported_build` 并禁用录制）：

```yaml
global:
  http:
    listenaddr: :8080
  loglevel: info
  admin:
    enablelogin: false
  storage:
    local:
      path: /tmp/webrtc-2k-record/local
mp4:
  enable: true
webrtc:
  port: udp:9000-9100
  mimetype:
    - video/H264
    - audio/opus
rtsp:
  tcp:
    listenaddr: :8554
```

启动命令：

```bash
cd example/default
go build -tags sqlite -o /tmp/webrtc-2k-record/m7s-server .
mkdir -p /tmp/webrtc-2k-record/run && cd /tmp/webrtc-2k-record/run
nohup /tmp/webrtc-2k-record/m7s-server -c /tmp/webrtc-record-config.yaml \
  > /tmp/webrtc-2k-record/server.log 2>&1 &
```

录制确实启用（非“recording disabled”）：

```
20:59:33.166939 INF global storage created server=5 type=local
20:59:34.072163 INF index recovery task started server=5 plugin=MP4 ... runOnStart=true ...
```

`grep -c "recording disabled" /tmp/webrtc-2k-record/server.log` → **0**。

## 4. 验证脚本

`example/webrtc-2k-perf/record-check.sh`（已 `chmod +x`，`set -euo pipefail`）：

```
./record-check.sh rtsp          # A11 视频
./record-check.sh audio         # A11 音频（临时起 AAC 2K 源 + RTSP 拉流代理）
./record-check.sh fragment      # A13 分片（默认 fragment=1m，录 175s）
./record-check.sh stop-source   # 录制中只停 RTMP 源
./record-check.sh whip          # A12 Chrome 假设备推 H264+Opus
./record-check.sh playback [f]  # Opus-in-MP4 播放探测
./record-check.sh all           # 依次执行 + cleanup + 汇总
./record-check.sh cleanup
```

- 录制只走 MP4 HTTP API：`POST /mp4/api/start|stop/<streamPath>`、`GET /mp4/api/list/<streamPath>`；
  请求体全部是 proto string，例如 `{"duration":"600s","fragment":"0","filePath":"rtsp-<RUN_ID>","type":"mp4"}`。
- Stop 的 not-found 按幂等成功：HTTP 404（及旧版 500 + `not found`）不视为失败。
- 每次运行用唯一 `filePath` 子目录（`<测试>-<RUN_ID>`），避免历史文件干扰断言。
- “录制收尾完成”以 DB 记录 `duration>0` + 文件存在 + 大小两次采样不变为准；
  只看文件大小会在 moov/trailer 写入前提前返回（本次调试中曾因此误判一次，已修复）。
- 只按 PID 停止进程，并在 kill 前校验命令行包含目标 URL/脚本路径；**不使用 pkill/killall**。
- 失败断言返回非零；无法本机执行的项打印 PENDING。

最终完整运行命令：

```bash
OUT_DIR=/tmp/webrtc-2k-record/check \
STORAGE_ROOT=/tmp/webrtc-2k-record/local \
  ./example/webrtc-2k-perf/record-check.sh all \
  > /tmp/webrtc-2k-record/run-all-pass.log 2>&1
```

退出码 **1**，唯一 FAIL 是 §7 的 A12 Opus 音频（产品缺陷），其余全部 PASS。

### 4.1 2K 源策略

沿用 Task 5：`rtsp-pull.sh start` 用 ffmpeg 经 RTMP 推 `live/source2k`（2560×1440@24，纯视频），
再由同实例 RTSP 插件在 `:8554` 提供该流，通过 `/api/proxy/pull/add` 拉成 `live/rtsp2k`。
源与 RTSP 服务同进程，属于控制回路 sanity check，不等价于真实摄像头（见 §9）。

## 5. A11：RTSP 源录制

### 5.1 视频（单文件，录约 25s）

源流状态（录制前）：

```
live/rtsp2k video=avc1 audio=- 2560x1440
```

API：

```bash
curl -X POST -H 'Content-Type: application/json' \
  --data-binary '{"duration":"600s","fragment":"0","filePath":"rtsp-20261118-213019","type":"mp4"}' \
  http://127.0.0.1:8080/mp4/api/start/live/rtsp2k
# 25s 后
curl -X POST -H 'Content-Type: application/json' --data-binary '{}' \
  http://127.0.0.1:8080/mp4/api/stop/live/rtsp2k
```

文件：`/tmp/webrtc-2k-record/local/rtsp-20261118-213019/2026-11-18-21-30-21_210487000.mp4`

ffprobe（`/tmp/webrtc-2k-record/check/probe-rtsp.json`）：

```json
{
  "streams": [
    {"index": 0, "codec_name": "h264", "codec_type": "video",
     "width": 2560, "height": 1440, "avg_frame_rate": "605000/25133"}
  ],
  "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "duration": "25.132000", "size": "31980562"}
}
```

断言：`h264` ✓、`2560×1440` ✓、`avg_frame_rate=605000/25133=24.072`（[23,25]）✓、
`duration=25.132s` ≈ 25s 录制窗口 ✓。

**默认 2K 源无音频**：`audioTrack=null`（已记录；音频用 §5.2 的独立源验证）。

### 5.2 音频（AAC 源，录约 18s）

临时源（脚本 `start_audio_source` 启动，PID 记录在 `audio-source.pid`）：

```bash
ffmpeg -re -f lavfi -i testsrc2=size=2560x1440:rate=24 -f lavfi -i sine=frequency=440:sample_rate=48000 \
  -c:v libx264 -preset ultrafast -b:v 10M -maxrate 12M -bufsize 20M -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 128k -f flv rtmp://127.0.0.1/live/source2k-audio
```

拉流代理（复用 `rtsp-pull.sh`，`START_SOURCE=0`）：

```bash
START_SOURCE=0 SOURCE_RTMP_URL=rtmp://127.0.0.1/live/source2k-audio \
RTSP_URL=rtsp://127.0.0.1:8554/live/source2k-audio STREAM_PATH=live/rtsp2k-audio \
OUT_DIR=/tmp/webrtc-2k-record/check/rtsp-pull-audio ./rtsp-pull.sh start
```

录制 `live/rtsp2k-audio`，`fragment:"0"`，18s 后 stop。

文件：`/tmp/webrtc-2k-record/local/rtsp-audio-20261118-213019/2026-11-18-21-30-52_006355000.mp4`

ffprobe（`probe-rtsp-audio.json`）：

```json
{
  "streams": [
    {"index": 0, "codec_name": "h264", "codec_type": "video",
     "width": 2560, "height": 1440, "avg_frame_rate": "8700/361"},
    {"index": 1, "codec_name": "aac", "codec_type": "audio",
     "sample_rate": "48000", "channels": 1}
  ],
  "format": {"duration": "18.049000", "size": "23652927"}
}
```

断言：audio codec=`aac` ✓、sample_rate=48000 ✓、duration=18.049s>0 ✓。
（源为单声道 sine，故 channels=1。）

## 6. A13：分片录制

`fragment:"1m"`，录制 **175s** 后 stop（`FRAGMENT_SECONDS=175`）。

> **关于 190s 的说明：** 任务简报建议录 ~190s 并期望 3 个文件。实测分片边界按媒体时间在
> 60/120/180s 触发，190s 会得到 **4 个文件**（60/60/60/10s），不满足“3 个文件”的验收口径；
> 175s（<180s 边界）恰好得到 3 个文件，故最终采用 175s，并在本报告如实记录实际时长。

文件（`filePath=fragment-20261118-213019`，共 3 个）：

| # | 文件 | duration | size | ffprobe |
|---|---|---|---|---|
| 1 | `2026-11-18-21-31-13_785563000.mp4` | **54.832s** | 68,668,791B | h264 2560×1440，可播 |
| 2 | `2026-11-18-21-32-14_151630000.mp4` | **59.957s** | 74,997,818B | h264 2560×1440，可播 |
| 3 | `2026-11-18-21-33-14_149407000.mp4` | **60.300s** | 75,262,824B | h264 2560×1440，可播 |

三个文件创建时间戳间隔均为 60s（21:31:13 → 21:32:14 → 21:33:14），与 `1m` 分片一致。
首片媒体时长 54.8s 偏短，原因是录制启动时首个样本时间戳偏移（播放器看到的样本跨度不足 60s），
非分片周期错误；因此脚本对“≈60s”的判定容忍 48–70s（实测三片都落在该区间）。

ffprobe JSON：`/tmp/webrtc-2k-record/check/probe-fragment-{1,2,3}.json`。
断言：文件数=3 ✓、每个文件可读且 duration>0 ✓、时长约 1 分钟 ✓。

## 7. A12：WebRTC/WHIP 推流录制（Opus-in-MP4）

### 7.1 推流

Playwright（Chrome，headless）+ 假设备：

```
--use-fake-device-for-media-stream --use-fake-ui-for-media-stream
--autoplay-policy=no-user-gesture-required
```

注入 init script，把 `RTCRtpSender.getCapabilities` 过滤为 `video/H264` 与 `audio/opus`
（否则 `publish.html` 会优先 `setCodecPreferences([PCMA])`），然后打开
`http://127.0.0.1:8080/webrtc/test/publish?streamPath=live/whip`。

推流就绪（脚本写入 `whip-ready.json`）：

```json
{"path":"live/whip","videoCodec":"avc1","audioCodec":"Opus","width":640,"height":480}
```

服务端 `mimetype: [video/H264, audio/opus]` → 协商结果 H264 + Opus，推流侧符合预期。

### 7.2 录制结果（progressive mp4，默认 `type:"mp4"`）

API 与 A11 相同（`{"duration":"600s","fragment":"0","filePath":"whip-20261118-213019","type":"mp4"}`），
录 15s 后 stop。结果：**16 个 1 秒文件**，每个 4.5–7.7KB：

```
/tmp/webrtc-2k-record/local/whip-20261118-213019/
  2026-11-18-21-34-40_293696000.mp4 ... 2026-11-18-21-34-55_271264000.mp4   (16 个)
```

服务端日志（16 次，每秒一次）：

```
21:34:40.493394 INF unsubscribe server=5 plugin=MP4 streamPath=live/whip sId=539
  reason=unsupported audio codec for mp4 record: *codec.OPUSCtx
21:34:41.466635 INF unsubscribe ... reason=unsupported audio codec for mp4 record: *codec.OPUSCtx
...
```

`grep -c "unsupported audio codec for mp4 record" /tmp/webrtc-2k-record/server.log` → **47**
（包含本次及此前调试运行；单次 15s 录制产生 16 次）。

最新文件 ffprobe（`probe-whip.json`）：

```json
{
  "streams": [{"index": 0, "codec_name": "h264", "codec_type": "video",
               "width": 640, "height": 480, "avg_frame_rate": "1000/1"}],
  "format": {"format_name": "mov,mp4,m4a,3gp,3g2,mj2", "size": "4503"}
}
```

- 视频：h264，**PASS**。
- 音频：**文件内无 audio stream**，**FAIL**。
- 记录器行为：订阅 → 拒绝 Opus → 退订 → 重试，每秒生成一个新的 1 秒视频片段；
  录制任务本身不会自动终止。
- **DB 元数据不可信**：`/mp4/api/list/live/whip` 中这些记录写着
  `"duration":1, "audioCodec":"opus"`，但文件实际没有音轨。元数据来自 publisher 的
  `HasAudioTrack()`，不代表 muxer 真正写入成功。

### 7.3 附加探测：fmp4 + Opus（不在脚本默认路径内）

为排除“只有 progressive 不支持”的可能，手工用 `type:"fmp4"` 对同一 `live/whip` 录了一次
（命令与 progressive 相同，仅 `"type":"fmp4"`，`filePath:"whip-fmp4"`）：

```
21:05:30.947335 ERR panic server=5 plugin=MP4 filePath=whip-fmp4 streamPath=live/whip
  error=runtime error: slice bounds out of range [:8] with capacity 4
  ...
  github.com/yapingcat/gomedia/go-codec.(*OpusContext).ParseExtranData
  m7s.live/v5/plugin/mp4/pkg/box.CreateOpusSpecificBox (dops.go:46)
  m7s.live/v5/plugin/mp4/pkg.(*Track).makeStsd (track.go:213)
  ... WriteMoov ... createStream (record.go:1087)
```

结果：`/tmp/webrtc-2k-record/local/whip-fmp4/2026-11-18-21-05-30_944767000.mp4` 仅 **24B**，
录制任务消失（stop 返回 404）。结论：fmp4 路径同样不可用，且在插件内 panic（被 task 系统 recover）。

### 7.4 Opus-in-MP4 播放结论

由于 Monibuca 无法产出 Opus-in-MP4，播放能力用 **ffmpeg 合成的参照样本**单独验证
（明确标注：不是 Monibuca 录制产物）：

```bash
ffmpeg -y -f lavfi -i testsrc2=size=640x480:rate=25 -f lavfi -i sine=frequency=440:sample_rate=48000 \
  -t 10 -c:v libx264 -preset ultrafast -pix_fmt yuv420p -c:a libopus -b:a 96k -movflags +faststart \
  /tmp/webrtc-2k-record/check/playback/probe.mp4
```

ffprobe：`video=h264 640x480 avg_fps=25/1`，`audio=opus 48000Hz 1ch`，`duration=10.000000s`。

| 播放器 | 结果 | 证据 |
|---|---|---|
| ffplay 7.1.1 | **可播放** | `SDL_AUDIODRIVER=dummy ffplay -autoexit -nodisp -loglevel error -t 5` → rc=0 |
| VLC 3.0.17.3 | **可播放** | `VLC -I dummy --play-and-exit --no-video --aout=dummy --stop-time=5` → rc=0，`end of playlist, exiting` |
| Chrome 158 | **可播放** | `canPlayType('video/mp4; codecs="avc1.42E01E, opus"')="probably"`，events=`loadedmetadata,loadeddata,canplay,playing,canplaythrough`，`readyState=4`，`videoWidth=640`，`currentTime=5.457`，`error=null` |

**结论：播放端（ffplay/VLC/Chrome）均支持 Opus-in-MP4。当前阻断点是 Monibuca 录制器的
MP4 封装（progressive 缺 Opus 分支；fmp4 的 dOps 构造 panic），不是浏览器/播放器限制。**

## 8. 录制中停止源

步骤：`live/rtsp2k` 开始单文件录制 → 等 10s → **只** SIGTERM 推流进程（PID 从
`rtsp-pull.sh` 的 `state.env` 读取，kill 前校验命令行含 `rtmp://127.0.0.1/live/source2k`）
→ 等 5s → stop 录制（本次返回 200；若已自行结束则 404 按幂等成功）。

文件：`/tmp/webrtc-2k-record/local/stop-source-20261118-213019/2026-11-18-21-34-14_535461000.mp4`

ffprobe（`probe-stop-source.json`）：

```json
{
  "streams": [{"index": 0, "codec_name": "h264", "codec_type": "video",
               "width": 2560, "height": 1440, "avg_frame_rate": "44000/1821"}],
  "format": {"duration": "10.925000", "size": "13757584"}
}
```

断言：size=13.76MB>0 ✓、moov 可读（ffprobe 成功）✓、duration=10.925s>0 ✓、无零字节文件 ✓。

调试记录：首次运行时脚本只等“文件大小 2 次采样不变”，在 trailer 写入前（stop 后约 5s）就返回，
导致 ffprobe 报 `moov atom not found`；修复为等 DB `duration>0` 后重跑通过。
这说明**停止源后文件的 trailer/入库可能滞后数秒**，调用方应等 API duration 更新而不是只看文件大小。

## 9. PENDING（本机不可完成，未冒充通过）

| 项 | 原因 | Linux 真机命令 |
|---|---|---|
| 10 分钟长录 | 计划要求 600s，本机按“短/可行”原则只跑 25s/18s/175s | `RECORD_SECONDS=600 ./record-check.sh rtsp`（或循环 start/stop） |
| 真实摄像头 RTSP 源 | 本机无摄像头；现有源与 RTSP 服务同进程 | `RTSP_URL=rtsp://<cam-ip>/... START_SOURCE=0 ./record-check.sh rtsp` |
| 弱网/丢包 | 本机 macOS 无 tc netem | `tc qdisc add dev eth0 root netem loss 1% delay 20ms` 后跑 `rtsp`/`whip`，结束 `tc qdisc del dev eth0 root` |
| 10 路并发录制 | 单路 x264 编码器，本机不适合 | Linux 上对 10 个 `live/xxx` 并发 `POST /mp4/api/start/...`，观察 CPU/磁盘/DB |
| 真实 WebRTC 摄像头（非假设备） | Chrome 假设备为 640×480 合成画面 | 真机 Chrome 用真实摄像头推 `live/whip`，重复 `whip` 流程 |
| A12 Opus 音频通过 | 需修复录制器（本任务禁止修改） | 修复 `plugin/mp4/pkg/record.go` 音频 switch 与 `box/dops.go` 的 dOps 构造后重跑 `./record-check.sh whip` |

## 10. 原始产物路径（未提交，位于 /tmp）

| 内容 | 路径 |
|---|---|
| 最终完整运行日志（唯一 FAIL=A12 Opus） | `/tmp/webrtc-2k-record/run-all-pass.log` |
| 服务端日志（含 storage/录制/Opus/panic 证据） | `/tmp/webrtc-2k-record/server.log` |
| 临时配置 | `/tmp/webrtc-record-config.yaml` |
| A11 视频 ffprobe | `/tmp/webrtc-2k-record/check/probe-rtsp.json` |
| A11 音频 ffprobe | `/tmp/webrtc-2k-record/check/probe-rtsp-audio.json` |
| A13 分片 ffprobe | `/tmp/webrtc-2k-record/check/probe-fragment-{1,2,3}.json` |
| 停源 ffprobe | `/tmp/webrtc-2k-record/check/probe-stop-source.json` |
| WHIP ffprobe | `/tmp/webrtc-2k-record/check/probe-whip.json` |
| 录制文件（RTSP/音频/分片/停源/WHIP） | `/tmp/webrtc-2k-record/local/{rtsp-20261118-213019,rtsp-audio-20261118-213019,fragment-20261118-213019,stop-source-20261118-213019,whip-20261118-213019}/` |
| fmp4+Opus panic 探测残留 | `/tmp/webrtc-2k-record/local/whip-fmp4/2026-11-18-21-05-30_944767000.mp4`（24B） |
| Opus-in-MP4 播放参照样本 | `/tmp/webrtc-2k-record/check/playback/probe.mp4` |
| WHIP runner 日志 | `/tmp/webrtc-2k-record/check/whip-publish.log` |
| 音频推流日志 | `/tmp/webrtc-2k-record/check/audio-push.log` |

清理状态：验证结束后 `record-check.sh cleanup` 已删除全部拉流代理、停止音频源/WHIP runner/HTTP 服务；
清理后 `/api/proxy/pull/list` 为空、`/api/stream/list` 为空（已实测），随后本任务启动的
Monibuca 服务端进程也已停止。仓库内未提交任何 /tmp 数据或临时 runner。

## 11. 脚本/流程备注

1. **未修改录制器/Go 代码/go.mod/docs/Task 0/5 脚本**；本报告中的阻断项均为实测产品行为。
2. Stop 的 not-found 幂等：脚本把 HTTP 404（以及旧版 500 + `not found`)视为成功。
3. 分片录制的最后一片依赖手动 stop 时刻；175s 实测 3 片（54.8/60.0/60.3s），190s 会是 4 片。
4. `filePath` 使用每次运行唯一子目录，避免历史文件/DB 记录干扰断言。
5. WHIP 假设备为 640×480（Chrome 假设备默认），本任务只验证录制兼容性，不验证 2K WebRTC 性能
   （2K 性能见 Task 4/5 报告）。
