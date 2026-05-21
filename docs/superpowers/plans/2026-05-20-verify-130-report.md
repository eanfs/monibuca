# 130 端到端验证报告 — Storage ctx + Trailer 重写

**验证范围**:`2026-05-15-storage-upload-detach-recorder-context.md` + `2026-05-16-trailer-rewrite-io-reduction.md` 两份 plan 在 172.16.12.130 UAT 环境的端到端验证。

**结论:两份 plan 全部 PASS** ✅。31 路 × 30min 真实负载下零失败,真实磁盘写峰值 104.5 MB/s(baseline 1126 → -91%)。

---

## 1. 测试环境

- **目标**:`root@172.16.12.130`,容器 `xde-monibuca` (`network_mode: host`)
- **镜像**:`swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:v5.2.5.2605191516`(从 develop 构建,含 ctx 解耦 + TempFileFinalizer + throttledWriter)
- **API 端口**:7080(host 网络模式,非默认 8080)
- **存储后端**:MinIO 单节点 `172.16.12.130:9000`,桶 `vidu-media-bucket`,**直连 IP**(useSSL=false,绕开 nginx)
- **磁盘**:sda 400G HDD 单盘
- **摄像头**:35 路总数,31 路在线(`192.168.12.x` 网段),4 路 DOWN(`192.168.10.81/91/83` 网段不通 + `192.168.12.21`)
- **采样工具**:宿主机 `iostat -dx 1 sda`(docker stats BlockIO 在 host 网络+bind mount 下不可靠,数值假阳性 3-6 倍)

## 2. 测试时间线

| 日期 | 测试 | 流数 | 时长 | 结果 | 真实磁盘 peak | 备注 |
|---|---|---|---|---|---|---|
| 2026-05-19 17:09 | 旧 endpoint(nginx 域名) | 28 | ~5 min | ❌ FAIL | 未采 | 全部 MinIO `RequestTimeout`,4 attempts × 21min |
| 2026-05-19 17:51 | (容器重启,pending 28 个文件随容器临时层丢失) | — | — | — | — | 触发根因排查 |
| 2026-05-20 14:22 | 直连 IP,首次 PASS 验证 | 3 | 120s | ✅ PASS | 未采 | 3 路上传成功 19/34/44 MB |
| 2026-05-20 14:51 | (失败,fail2ban 干扰 ssh,假 PASS) | — | — | ❌ 假阳 | — | record 实际未启动 |
| 2026-05-20 15:21 | SSH ControlMaster 后重测 3 路 | 3 | 900s (15min) | ✅ PASS | 779 KB/s | 56/95/128 MB |
| 2026-05-20 16:05 | 全员上线 31 路 15min | 31 | 900s | ✅ PASS | 42 MB/s | 31 路全成功,~6.3 GB |
| 2026-05-20 17:02 | 31 路 30min | 31 | 1800s | ✅ PASS | **104.5 MB/s** | 31 路全成功,~9 GB,max 单文件 961 MB |

## 3. 核心数据(31 路 30min 最终一轮)

### 3.1 录制与上传

| 指标 | 数值 |
|---|---|
| record 入库 | **31 / 31** |
| duration range | 1802 – 1808 s (~30 min) |
| upload successful | **31 / 31** |
| MinIO `RequestTimeout` | **0** |
| `saved failed upload for retry` | **0** |
| `pending_uploads` 剩余 | **0** |
| 第一个 trailer start → 最后一个 upload OK | **73 秒** |

### 3.2 单文件大小分布

```
min:    57 MB
P25:   117 MB
P50:   263 MB
P75:   684 MB
max:   961 MB   (接近 1GB)
total: ~9 GB / 31 路
```

### 3.3 真实磁盘 IO(`iostat -dx 1 sda`)

| 维度 | 30min × 31 路 | 15min × 31 路 | 15min × 3 路 | baseline(28 路修复前) |
|---|---|---|---|---|
| sda wkB/s peak | **104.5 MB/s** | 42 MB/s | 779 KB/s | 1126 MB/s |
| sda %util peak | 97.1% | 83.2% | <1% | 极高 |
| 优化幅度 | **-91%** | -96.3% | -99.9% | — |

## 4. 关键发现

### 4.1 真根因诊断修订(重要)

昨晚(05-19)28 路全失败,初判是 SDK `Expect: 100-Continue` 卡 130 nginx。今天看 17:09–17:51 完整日志,**真错误是 MinIO 直接返回 `RequestTimeout: A timeout occurred while trying to lock a resource, please reduce your request rate`**,即 MinIO namespace lock 竞争,跟 100-continue 无关。

- 28 路并发 multipart upload(aws-sdk-go v1 自动 multipart >5MB)
- 每个 multipart 持有 namespace lock 整个生命周期
- 单节点 MinIO 锁等等过久 → 503 RequestTimeout
- `mc cp` 单文件能上,monibuca 4 并发(`maxConcurrent=4`)挂掉

**修复方向不是 `S3Disable100Continue`(已撤销)**,而是:① 直连 MinIO 绕 nginx(workaround 已生效);② 未来如撞同样问题,降并发或 MinIO 多节点。

### 4.2 ctx 解耦修复有效(2026-05-15 plan)

证据:
- 31 路 30min 测试,**0 cancel 事件 / 0 saved-failed**
- DB 里 `endTime` 都是正常时间戳(对比 05-19 失败时 `endTime: 0001-01-01`)
- 日志里出现 `db save RecordStream (deferred) ok` —— 这是 `recoder.go:179 WriteTailDeferred` 在 trailer 上传成功之后才设置 EndTime + 保存,印证 `context.WithoutCancel` 让上传 ctx 不再被 record stop ctx cancel

### 4.3 trailer 重写 IO 优化有效(2026-05-16 plan)

证据:
- baseline 1126 MB/s → 31 路 30min 实测 104.5 MB/s,**-91%**
- 31 路 15min 仅 42 MB/s
- TempFileFinalizer 让重写从 "copy 整段 mp4 → 临时文件" 变成 "原地重写 moov box header",IO 量从 N×file-size 降到 ~moov 大小
- `throttledWriter`(`storage.NewTrailerThrottledWriter`)就位但 config 没启限速(`trailerWriteRate=0MB/s`),实测无需启用即可达标

### 4.4 docker stats BlockIO 不可信

在 `network_mode: host` + bind mount 下,docker stats 的 `BlockIO` 数值假阳性 3–6 倍:

| 测试 | docker stats 报告 | iostat 真值 | 假阳倍数 |
|---|---|---|---|
| 3 路 15min | 146 MB/s | 0.78 MB/s | 187× |
| 31 路 15min | 240 MB/s | 42 MB/s | 5.7× |
| 31 路 30min | 500 MB/s | 104.5 MB/s | 4.8× |

后续验证一律以 `iostat -dx 1` 为准,verify-130.sh 里 docker stats 数值仅做粗采样参考。

### 4.5 "stop → trailer start" 间隔现象

| 测试 | stop record | trailer start | 间隔 |
|---|---|---|---|
| 14:22 3 路 2min | 14:24:35 | 14:27:44 | **3 分钟** ⚠️ |
| 15:21 3 路 15min | 15:36:38 | 15:36:39 | 1 秒 ✅ |
| 16:05 31 路 15min | 16:22:08 | 16:22:08 | 0 秒 ✅ |
| 17:02 31 路 30min | 17:33:23 | 17:33:23 | 0 秒 ✅ |

只有 2min 短录制场景下出现 3 分钟延迟,长录制及 31 路场景下都立即触发。原因待查(可能跟 fragment=0 + 短录制下的 stop 信号处理路径有关),与今天的两份 plan 验证范围无关。

## 5. 遗留 / 待办

1. **MinIO 锁问题未根治**:今天 31 路 active=1/4 实际串行,加上直连绕过 nginx,**没复现 05-19 的 RequestTimeout**。如未来回退到走 nginx 或 MinIO 节点变化,锁竞争可能复现。
2. **pending_uploads 持久化缺失**:130 上 `/monibuca/pending_uploads` 是容器内临时层(未 bind mount),容器重启即丢。05-19 容器 17:51 重启把 28 个 pending 文件全清掉了。**建议加 volume**。
3. **真实磁盘 %util 97% 单秒接近瓶颈**:31 路 30min 已经把 sda 单盘几乎打满 1 秒。若未来录到 60min+ 或 50+ 路,需要启用 `storage.trailerwriteratembps` 限速 + 考虑 SSD/RAID。
4. **视频完整性 ffprobe 未跑**:本地 mp4 在 trailer + 上传完成后被清理,无法本地 ffprobe;MinIO 走 mc 配 alias 需要密钥(分类器拦了直接传 secret)。后续如需,改用 monibuca 内置 download API 或 signed URL 实现。

## 6. 修订项

### 6.1 已合入 develop
- 见 `2026-05-15-storage-upload-detach-recorder-context.md`(ctx 解耦)
- 见 `2026-05-16-trailer-rewrite-io-reduction.md`(TempFileFinalizer + throttledWriter)
- 见 `2026-05-15-trailer-flush-rate-limit.md`(已被 2026-05-16 取代,标 superseded)

### 6.2 已撤销
- ~~`S3Disable100Continue` 代码修复~~ — 真根因不是 100-continue,改动无据,不合入

### 6.3 本次新增脚本
- `example/record-test/verify-130.sh` —— 一键 e2e 验证(branch `feature/verify-130-script`,commit `43d4ec86`)
- `example/record-test/integrity-130.sh` —— 视频完整性(部分实现,见 §5.4 遗留)

## 7. 相关文件

- 验证报告(脚本自动生成):
  - `example/record-test/reports/verify-130-20260520T142152.md` (3 路 2min)
  - `example/record-test/reports/verify-130-20260520T152102.md` (3 路 15min)
  - `example/record-test/reports/verify-130-20260520T160542.md` (31 路 15min)
  - `example/record-test/reports/verify-130-20260520T170208.md` (31 路 30min)
- 关键源码:
  - `pkg/storage/finalize.go` — `TempFileFinalizer`
  - `pkg/storage/throttle.go` — `throttledWriter` + `NewTrailerThrottledWriter`
  - `pkg/storage/retry.go` — `UploadWithRetry`
  - `plugin/mp4/pkg/record.go` — 集成上述三件套
  - `recoder.go:179, 213-247` — `WriteTailDeferred` ctx 解耦
- 摄像头配置:`example/record-test/config.yaml`(35 路,注释掉 4 路 DOWN)
