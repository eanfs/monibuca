# Monibuca v5 Cluster 三节点 E2E 测试报告 — gotask 根治版回归

- **日期**: 2026-06-03(根治版回归轮)
- **被测二进制**: `dist/m7s`(linux/amd64,`CGO_ENABLED=0 -tags "cluster sqlite s3"`),**链接 `github.com/eanfs/gotask v1.0.5`**(BuildInfo 确认)—— 含 Job.Call 同 goroutine 重入死锁的**库层根治**
- **关联修复**: m7s `eanfs/monibuca#6` + gotask `langhuihui/gotask#4`(上游 PR)
- **结论**: ✅ **5/5 全部通过**

## 环境
- 3 节点 Consul 集群:node-1 `172.16.12.131`、node-2 `172.16.12.134`、node-3 `172.16.12.133`
- 各节点本地 sqlite;S3 = 131 自建 MinIO(桶 `m7s-records`)
- 基线:**31 路真实 RTSP 摄像头**(node-1: 4 / node-2: 12 / node-3: 15),全部在线有视频轨
- membership = 3,各节点 `/api/stream/list` 健康(200 / ~10ms)

## 测试用例与结果

### T1 — 同名跨节点拉流死锁回归(根治版关键)
node-1 用**同名** `live/camera20→camera27`(实际 camera27,属 node-3)`pull/add`:
- `acquire stream key failed` → `unpublish reason="cluster: streamPath already owned by peer"`(干净停止)
- node-1 `/api/stream/list` 连续 6 探 **6/6 健康(200)**;node-3 **保留 camera27 属主**
- ✅ **修复前必死锁,根治版无死锁**

### T2 — 跨节点订阅 302 路由
node-2 FLV 拉 `live/camera1`(属 node-1)→ `HTTP 302 Location: http://172.16.12.131:19080/flv/live/camera1` ✅

### T3 — auto-relay(订阅触发)持续供流
node-2 RTSP 订阅 `live/camera3`(属 node-1)→ 15s **343 帧**(稳态 ~23fps);node-2 全程健康;relay 正确跳过 acquire(无失败日志)✅

### T4 — 录制模式(a)拉流节点本地录 + G.711 音频
node-3 本地录 `live/camera27`(带 G.711 音频)18s → S3 `record/T4-local-camera27/*.mp4`(9.6 MiB):
- 视频 h264 2688×1520;**音频 pcm_alaw 8000Hz `channels=1`**(音频修复生效;修复前 channels=0 整文件不可解析)
- 全文件(音频+视频)解码 **exit=0,0 损坏** ✅

### T5 — 录制模式(b)跨节点录制(4K)
node-1 播放 `live/camera29`(属 node-3,**4K 3840×2160**)经 auto-relay 拉成本地流 → 录 20s → S3 `record/T5-xnode-camera29/*.mp4`(5.0 MiB):
- h264 3840×2160,**404 帧**,20.1s;全帧解码 **exit=0,0 损坏**
- node-1 录制全程无 `owned by peer` 停流(relay 经 `activeRelays` 正确识别,未被误停)✅

## 录制产物(S3 `m7s-records`)
| key | 大小 | 内容 |
|---|---|---|
| `record/T4-local-camera27/…mp4` | 9.6 MiB | 2K H264 + G.711(channels=1)|
| `record/T5-xnode-camera29/…mp4` | 5.0 MiB | 4K H264,跨节点 relay 录制 |

## 结论
gotask 库层根治(`eanfs/gotask v1.0.5`)+ m7s 侧 RC1/RC2/RC3 修复 + 音频 channels 修复,在 3 节点活集群上**全部回归通过**:跨节点同名流冲突干净停止不死锁、auto-relay 持续供流、三模式录制 + S3 + 视频/音频完整性均 OK。

## 双架构 Docker 镜像
测试通过后构建多架构镜像(buildx,根治版 cluster 二进制):
- 交叉编译:`monibuca_amd64`(92 MB,x86-64)+ `monibuca_arm64`(85 MB,aarch64),均 `CGO_ENABLED=0 -tags "cluster sqlite s3"`、纯 Go(无 CGO,arm64 直接交叉编译)
- `docker buildx build --platform linux/amd64,linux/arm64`(base `swr…/intetech/ffmpeg:latest` 跟随 TARGETPLATFORM;按 TARGETARCH 选清华 ubuntu/ubuntu-ports 源装 tcpdump)
- 产物:`dist/monibuca-cluster-rootfix.oci.tar`(515 MB,OCI image index),tag `monibuca-cluster:rootfix-eanfs-gotask-v1.0.5`
  - manifest list 含 **linux/amd64 + linux/arm64**(+ SLSA attestation 清单)
- 冒烟(amd64 load 后 `docker run`):容器内 m7s 正常启动(listen :8080/:50051,Cluster 插件已编入),`ffmpeg` + `tcpdump` 在位,二进制 = cluster 根治版
- 备注:Dockerfile 用 `COPY monibuca_${TARGETARCH}` 拷预编译二进制;`.dockerignore` 已排除 `deploy-3node`(含密码)/`.git` 等。amd64 直接构建,arm64 的 apt 在 QEMU 模拟下耗时 ~18 min。
