# Trailer 零重写 阶段 A — 130 端到端验收报告

**验收对象**:`2026-05-21-trailer-zero-rewrite.md` plan 阶段 A(`fallocate INSERT_RANGE` 原地插 moov),分支 `feature/trailer-zero-rewrite`(commit `9a9f23df` / `31c833fc` / `d6fb9025`)。

**结论:阶段 A 验收通过** ✅。31 路 × 15min 真实录制下,trailer 重写 100% 走 `INSERT_RANGE` fast path,monibuca trailer 写入从「≈整个文件大小」降到「≈moov 大小」(cam1 实测 48MB → 100KB,-99.8%),产物为标准 moov-first MP4,0 失败。

---

## 1. 验收环境

- **目标**:`172.16.12.130` UAT,容器 `xde-monibuca`(`network_mode: host`)
- **镜像**:`swr.cn-east-3.myhuaweicloud.com/intetech/monibuca:v5.2.5.2605211340`(从 `feature/trailer-zero-rewrite` 交叉编译 `example/cluster`,含阶段 A 的 A1-A3)
- **磁盘**:`/dev/sda4` ext4(SSD),系统 / monibuca 录像 / MinIO `/data/minio` / `/tmp` 同分区;`/tmp` 单独挂 tmpfs
- **存储后端**:MinIO 单节点 `172.16.12.130:9000`,桶 `vidu-media-bucket`,直连 IP
- **摄像头**:31 路在线(`192.168.12.x`)
- **测试**:`verify-130.sh run-test`,`STREAM_COUNT=31 RECORD_SECONDS=900`;宿主 `iostat -dx 1 sda` 全程采样

## 2. 验收项总览

| # | 验收项 | 方法 | 结果 |
|---|---|---|---|
| 1 | `INSERT_RANGE`/`COLLAPSE_RANGE` syscall | `storage.test` 交叉编译到 130 ext4 跑 `TestInsertRange` | **PASS** |
| 2 | `pkg/storage` 单测无回归 | darwin `go test ./pkg/storage/` | **ok** 全 PASS |
| 3 | 31 路录制走 fast path | 日志 `insert-range fast path done` 计数 | **31 / 31** |
| 4 | fast path 异常 | 日志 `skipped`/`rolled back`/`write head failed` | **0** |
| 5 | 上传成功 | 日志 `upload successful` | **31 / 31** |
| 6 | MinIO 锁竞争 | 日志 `RequestTimeout` | **0** |
| 7 | 失败补传 | 日志 `saved failed` + `pending_uploads` | **0** / 0 |
| 8 | record 入库 | `mp4/api/list` 今日 14 点条目 | **31 条,`endTime` 全正常** |
| 9 | 产物 moov-first 可播 | 下载 MinIO 对象 `ffprobe` | **PASS**(见 §3.5) |

## 3. 关键数据

### 3.1 INSERT_RANGE syscall 实证(130 ext4)

`pkg/storage/fallocate_test.go` 交叉编译为 Linux 二进制,传 130,在 `/root`(`/dev/sda4` ext4)上运行:

```
=== RUN   TestInsertRange
--- PASS: TestInsertRange (0.00s)
```

验证 `fallocate(FALLOC_FL_INSERT_RANGE)` 在文件头插入块后原数据整体后移、新空间可写;`COLLAPSE_RANGE` 可撤销。注:130 的 `/tmp` 为 tmpfs 不支持 `INSERT_RANGE`,测试在 ext4 目录运行才生效(tmpfs 上会正确 SKIP)。

### 3.2 31 路 fast path 100% 生效

31 路 × 15min 录制(14:03:23 启动,14:18:33 stop),日志统计:

```
insert-range fast path done : 31
insert-range 异常            : 0
upload successful            : 31
RequestTimeout               : 0
saved failed                 : 0
```

### 3.3 trailer 写入量塌缩(核心指标)

fast path 日志 `insertedBytes` = monibuca 在 trailer 阶段往文件写入的**全部字节数**(ftyp + moov + 4K 对齐 padding):

| 路 | moovBytes | insertedBytes | 对应文件大小 | trailer 写入 / 文件 |
|---|---|---|---|---|
| cam1 | 98 528 | 102 400 (100 KB) | ~48 MB | **0.2%** |
| cam26 | 198 946 | 200 704 (196 KB) | — | — |
| cam18 | 404 044 | 405 504 (396 KB) | — | — |

对比修复前 fallback 全量重写:trailer 阶段需把整个 `mdat` 复制一遍(≈ 文件大小,百 MB 级)。**fast path 把 monibuca trailer 写入降到「≈moov 大小」,即原来的约 0.2%**。

### 3.4 iostat 整盘读数说明(避免误读)

宿主 `iostat -dx 1 sda` 在 stop 窗口峰值 `wkB/s` ≈ **699 MB/s**。**这不是 trailer 写入** —— `sda` 同时承载 MinIO `/data/minio`,该峰值是 **MinIO 接收 31 路上传(每路几十 MB)落盘**的写入,属上传固有成本,与 trailer 优化无关。

monibuca 自身 trailer 写入由 §3.3 的 `insertedBytes`(100-405 KB/路)实证;`docker stats` 报 monibuca 容器 BlockIO 仅 2 MB/s,方向一致(但 host 网络模式下 docker stats 不可靠,不作主证据)。要单独测 trailer 真实磁盘峰值需 monibuca 与 MinIO 分盘,见 plan 风险节。

### 3.5 产物 moov-first 验证

从 MinIO 下载 cam1 对象 `2026-05-21-14-03-23_291036932.mp4`(50 509 637 B),容器内 `ffprobe`:

- **box 解析顺序**:`ftyp → moov → free → free → mdat` —— 正是 INSERT fast path 设计布局(moov 紧随 ftyp,旧 `[ftyp][free]` 被 free box 覆盖,`mdat` 在尾且数据零移动)
- 文件头实测:offset 0 = `ftyp`(32B),offset 32 = `moov`(size `0x000180e0` = 98528,与日志 `moovBytes` 一致)
- `ffprobe`:`duration=909.678`,`codec=h264`,`1920x1080`,`format=mov,mp4` —— 完整可播放的标准 moov-first MP4

## 4. 结论

阶段 A(`fallocate INSERT_RANGE` 原地插 moov)在 130 真实 31 路 × 15min 负载下端到端验收通过:

- trailer 重写不再复制 `mdat`,monibuca trailer 写入从「≈文件大小」降到「≈moov 大小」(-99.8%)
- `mdat` 数据零移动,产物为标准 progressive moov-first MP4,兼容性无变化
- 31/31 走 fast path,0 异常、0 上传失败、0 补传、0 `RequestTimeout`,record 全部入库
- fallback 路径(`INSERT_RANGE` 不支持时的全量重写)保留,本次未触发

阶段 B(fMP4 旁路)不在本次执行范围。

## 5. 附:测量方法说明

- `verify-130.sh` 自动报告(`reports/verify-130-20260521T140214.md`)的「磁盘写峰值 2 MB/s」来自 `docker stats`,host 网络 + bind mount 下不可靠,**不作验收依据**;本报告以日志 `insertedBytes` + `ffprobe` 实证为准。
- fast path 是否生效的判定锚点:日志 `insert-range fast path done`(成功)/ `insert-range skipped|rolled back`(回退)。本次 31 条 done、0 回退。
