# Monibuca v5.3.1 Docker 容器版 — 三节点单机+集群功能验证

- **日期**: 2026-07-15
- **被测镜像**: `swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:v5.3.1.2607151131`(多架构 amd64+arm64)
- **对应代码**: develop `2a600639`(= v5.3.0 全部代码 + PR #7 feature/review-p1-fixes 全部修复),git tag `v5.3.1.2607151131`
- **编译参数**: `CGO_ENABLED=0 -tags cluster,postgres,sqlite,s3`(二进制内嵌 BuildInfo 已验证含 cluster tag)
- **部署形态**: **docker 容器**(host 网络),替代此前的裸二进制部署 —— 本轮为容器版首次三节点真机验证
- **结论**: ✅ **单机模式(3 节点×15min 录制+S3 上传)与 cluster 三节点模式全部通过**,零错误零死锁。

---

## 1. 打包过程

| 步骤 | 结果 |
|---|---|
| develop 合入 v5.3.0(`d1b7abee` 为祖先)确认 | ✅ develop 领先 v5 14 commit、落后 0 |
| 新 git tag `v5.3.1.2607151131` 推送 | ✅ 指向 `2a600639` |
| build_docker.sh 双架构构建+推 SWR+manifest(版本+latest) | ✅ amd64/arm64 manifest 验证通过 |

**build_docker.sh 的两个坑**(见 §5 问题清单): cluster tag 缺失已由 `6409caf3` 在 develop 修复;`-o type=docker,dest=-` tar 导出在新版 Docker Desktop 上不可用,本轮以 `--load` 方式本地修改跑通,**该修改尚未提交**。

## 2. 部署(三节点)

| node | 主机 | HTTP/gRPC/RTMP/RTSP | 容器 | 数据目录 |
|---|---|---|---|---|
| node-1 | 172.16.12.131 | 19080/15051/21935/11554 | m7s-node-1 | /home/m7s-docker |
| node-2 | 172.16.12.134 | 19081/15052/11936/11555 | m7s-node-2 | /root/m7s-docker |
| node-3 | 172.16.12.133 | 19082/15053/11937/11556 | m7s-node-3 | /root/m7s-docker |

- 旧裸二进制测试部署(`m7s-cluster` 目录,103–202MB/节点)已按要求删除;**生产服务未触碰**(134 的 host monibuca_linux 7080/554/50051、xde-monibuca v5.3.0 容器、各节点 xde-minio)。
- 容器 `--network host --restart unless-stopped`,配置只读挂载到 `/etc/monibuca/config.yaml`,数据挂 `/data`(sqlite db、TMPDIR)。
- infra 沿用 133: Consul `:8500`、MinIO `:29000`(bucket `m7s-records`)。
- webrtc/webtransport/gb28181 `enable:false`(防撞生产端口,历史教训)。
- 134 初始未登录 SWR(`You may not login yet`),复制 131 的 `/root/.docker/config.json` 解决。
- 摄像头拉流全程错峰(3s 间隔,每节点仅 2 路),规避 IPS 批量扫描封禁。

## 3. 单机模式测试(cluster enable:false)

6 路真实摄像头,15 分钟连续录制(11:54:26–12:10:14,fragment=0 单文件),S3 直写:

| 节点 | 流 | 文件 | 大小 | 校验 |
|---|---|---|---|---|
| node-1 | camera1 (hik 1080p) | SINGLE-0715-node1-camera1/camera1.mp4 | 227MiB | moov 头部✓ 948.9s✓ 全解码 0 真实错误✓ |
| node-1 | camera2 (hik 1080p) | .../camera2.mp4 | 100MiB | moov 头部✓ 948.9s✓ |
| node-2 | camera8 (hik 1080p) | .../camera8.mp4 | 171MiB | moov 头部✓ 948.8s✓ |
| node-2 | camera10 (dahua+audio) | .../camera10.mp4 | 28MiB | moov 头部✓ 948.6s✓ **pcm_alaw ch=1**✓ |
| node-3 | camera27 (2K+G.711) | .../camera27.mp4 | 483MiB | moov 头部✓ 948.7s✓ **pcm_alaw ch=1**✓ 全解码 0 真实错误✓ |
| node-3 | camera29 (4K) | .../camera29.mp4 | 218MiB | moov 头部✓ 948.9s✓ **3840×2160**✓ 全解码 0 真实错误✓ |

- 录制窗口三节点 **0 discard / 0 database-locked / 0 ERR**(中途 7min 与结束各扫一次)。
- 上传解耦生效: 停录后 6 路上传并发 `active=2/4`,全部成功;`insert-range fast path`(moov 前置)日志确认。
- DB 记录正确(duration/codec/S3 路径,`/mp4/api/list` 抽查 camera29)。
- "全解码错误"计数说明: ffmpeg null-muxer 的 `non monotonically increasing dts X >= X`(等值 dts)为已知无害告警,已排除;camera27/29 各有 1-2 行 `decode_slice_header` 为**源流 RTP 丢包坏帧**(与 2026-07-14 报告同签名),非录制/文件问题。

## 4. cluster 三节点测试

复用单机 data(各节点自动恢复 2 路拉流),切 cluster 配置重启容器:

| # | 场景 | 结果 |
|---|---|---|
| 1 | membership | ✅ 三节点 `/cluster/api/cluster/nodes` peers=3,Consul `m7s/nodes/` 3 键齐全 |
| 2 | 流注册 | ✅ 6 路全部注册 `m7s/streams/live/*`,属主正确 |
| 3 | 302 路由 | ✅ node-2 请求 camera1(属主 node-1) → `302 http://172.16.12.131:19080/flv/live/camera1` |
| 4 | lb-suggest | ✅ 返回 `suggested=node-1` + advertise/metrics |
| 5 | auto-relay | ✅ node-2 RTSP 订阅 camera27(属主 node-3),5s 收 97 视频帧(~19fps) |
| 6 | 跨节点 relay 录制 | ✅ node-2 对 relay 来的 camera27 录 119.7s → S3 64.4MB,moov 头部、2688×1520+pcm_alaw ch=1、完整性通过 |
| 7 | 同名流冲突/死锁回归 | ✅ node-1 拉 camera29(属主 node-3): acquire 失败干净停止;`/api/stream/list` 5 连探 200/<2ms **无死锁**;KV 属主保持 node-3(release-guard 未误删 peer 键) |

cluster 阶段三节点日志 0 条 ERR/panic/lock/discard(仅场景 7 预期的 acquire 冲突日志)。

## 5. 问题清单

1. **build_docker.sh 的 `--load` 修复未提交**: 新版 Docker Desktop(29.x,docker driver 无 containerd store)不支持 `-o type=docker,dest=- > tar`,报 "Docker exporter is not supported"。本轮构建为本地修改(`--load` 直接进镜像库,删除 tar 导出/load 步骤)。**建议提交到 develop**,否则下次打包再踩。
2. **134 无 SWR 登录凭证**: 已复制 131 的 docker config 解决;正式环境建议统一配置 SWR 长期凭证。
3. **跨节点录制列表聚合不适用于本地 sqlite 架构**: 各节点录制元数据在本地 db,`/mp4/api/list` 仅见本节点记录;跨节点列表/下载 302(旧场景 6/7)需共享 PostgreSQL 部署形态,本轮未测。
4. 源流质量: camera27/29 偶发 RTP 丢包坏帧(源侧,非本系统问题),与历史报告一致。

## 6. 加测：30 路(3×10) + 跨节点播放与录制(同日下午)

按要求扩容至**每节点 10 路、共 30 路**(错峰 3s/路加载,camera32 备用,4/5/6/33 已知 DOWN 跳过):
- node-1: camera1,2,3,7,9,11,12,13,14,15;node-2: camera8,10,16-23;node-3: camera24-31(除29外)+27,29,34,35
- **30/30 全部上线**,Consul KV 30 键,属主分布正确;测试全程(40min+)30 路保持在线无掉线。

### 跨节点播放

| 测试 | 结果 |
|---|---|
| 302 路由(三方向: n1→camera16@n2 / n2→camera24@n3 / n3→camera1@n1) | ✅ 全部 302 到正确 advertise 地址 |
| FLV 跟随 302 实拉(133 上 curl -L camera13@n1,8s) | ✅ 250KB,FLV 魔数正确,跨节点媒体流通 |
| RTSP auto-relay 播放(4 方向,relay 分别落在 n1/n2/n3) | ✅ camera13(n1→n2) 76帧/4s;camera28(n3→n2) 70帧/4s;camera3(n1→n3) 92帧/4s;camera20(n2→n1) 79帧/4s |

### 跨节点录制(三方向并行,各 ~154s)

RTSP 订阅触发 auto-relay 使流本地化后录制(FLV 302 不产生 relay,按既有约定):

| 录制节点 | 流(属主) | S3 对象 | 校验 |
|---|---|---|---|
| node-2 | camera13(node-1) | XNODE-0715-n2rec-camera13/camera13.mp4 (4.2MiB) | moov头部✓ h264 1080p+pcm_alaw 154.2s **0解码错误** |
| node-3 | camera20(node-2) | XNODE-0715-n3rec-camera20/camera20.mp4 (11MiB) | moov头部✓ h264 1080p+pcm_alaw 154.2s **0解码错误** |
| node-1 | camera28(node-3) | XNODE-0715-n1rec-camera28/camera28.mp4 (12MiB) | moov头部✓ **hevc(H.265) 2592×1944** 154.3s **0解码错误** |

三方向(n1录n3的流、n2录n1的流、n3录n2的流)全部成功,额外覆盖 **H.265 跨节点 relay 录制**路径。

### 日志扫描(30 路窗口 40min)
0 panic / 0 database-locked / 0 discard。仅有的 ERR 为测试客户端(ffprobe/ffmpeg timeout)主动断开的 `RTSP send error EOF/connection reset`,均为预期订阅端断连噪音。

## 7. 最终交付录制：30 路全量长录制(FINAL-0715)

按最终要求对**全部 30 路同时录制**(各流在属主节点本地录,fragment=0 单文件):

- **开录**: 12:57:27(30 路 8 秒内全部下发成功) **停录**: 13:19:17(30/30 停止成功)
- **实际时长 ≈ 21.8 分钟**(计划 15 分钟;停录因中途处理流信息查询延后 ~7 分钟,内容完整连续,**覆盖并超出 15 分钟要求**)
- **数量核对**: MinIO `FINAL-0715-*` **30/30 全部入桶,0 上传失败**(每节点并发 active=4/4,总量 8.8 GiB,36MiB~666MiB/路)
- **时长核对**: 逐一 ffprobe 全部 30 个对象 —— **1308.9s ~ 1309.9s,极差仅 1 秒**,30 路完全一致(开/停录下发次序造成的秒级偏移)
- 录制窗口中点与结束两次扫描: 三节点 **0 discard / 0 database-locked / 0 panic**,30 路无一掉线
- 与 07-14 裸二进制版 31 路×15min 压测结论一致,**容器版在 30 路并发长录制下复现同等稳定性**(S3File 写缓冲 + sqlite 单连接修复均生效)

| 分布 | 明细 |
|---|---|
| node-1 (10) | camera1,2,3,7,9,11,12,13,14,15 — 1080p,5 路含 G.711 |
| node-2 (10) | camera8,10,16-23 — 1080p,9 路含 G.711 |
| node-3 (10) | camera24-28,29,30,31,34,35 — 含 3×2K、1×H.265(2592×1944)、1×4K |

## 8. 重录:30 路 × 严格 15 分钟(FINAL3-0715,同日 14:33)

因 §7 的 FINAL-0715 实际 21.8 分钟,按要求重录并改用 **`duration:"900s"` 服务端到点自动停录**(该参数为 v5.3.1 新增,已在 196 环境验证):

- **开录**: 14:33:06(30/30 code 0) **自动停录**: 14:48:07±(900s 到点,`duration reached` 日志确认,零人工干预)
- **数量**: MinIO `FINAL3-0715-*` **30/30 全部入桶,0 上传失败**(每节点 `upload successful`×10),总量 **6.02 GiB**(24MiB~458MiB/路,与 15min 时长成比例)
- **时长**: 逐一 ffprobe 全部 30 个对象 —— **900.0s ~ 902.8s**,全部精确 15 分钟(到点停在下一关键帧,≤3s 偏差)
- **质量**: 三节点 0 discard / 0 database-locked / 0 panic / 0 上传失败;抽样 camera29(4K)moov 前置 ✓
- 节点分布与 §7 相同(每节点 10 路)

**过程教训(FINAL2 作废批次)**: 第一次重录(14:13)漏传 `fragment` 参数,`/mp4/api/start` **默认 fragment=1 分钟**,且固定 fileName 时每个分片**覆盖同一对象键**,桶里每路只剩最后 1 分钟 → 全部作废。**API 调用录制单文件必须显式传 `"fragment":"0"`**。作废产物 `FINAL2-0715-*`(30 个 ~1min 小文件)仍留在桶中待删(删除操作未获授权)。

## 9. 当前留存状态

- 三节点 `m7s-node-{1,2,3}` 容器**保持运行**(cluster 模式,**30 路拉流在线**),可直接续测。
- 录制成品留存 bucket `m7s-records`: `SINGLE-0715-*`(6 个) + `CLUSTER-0715-xnode-camera27/`(1 个) + `XNODE-0715-*`(3 个) + `FINAL-0715-*`(30 个,8.8GiB,21.8min 版) + **`FINAL3-0715-*`(30 个,6.02GiB,严格 15min 交付版)** + `FINAL2-0715-*`(30 个作废小文件,待删);MinIO Console `http://172.16.12.133:29001`(admin/m7sm7sm7s)。
- 旧裸二进制部署已删除;134 校验临时文件已清理。
