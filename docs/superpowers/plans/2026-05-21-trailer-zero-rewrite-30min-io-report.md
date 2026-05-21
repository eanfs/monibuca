# Trailer 零重写 阶段 A — 31 路 30min 录制 系统 IO 测试报告

**测试对象**:`feature/trailer-zero-rewrite` 分支阶段 A(`fallocate INSERT_RANGE` 原地插 moov),130 部署镜像 `v5.2.5.2605211340`。

**结论**:31 路 × 30min 真实录制下,trailer 重写 100% 走 `INSERT_RANGE` fast path。**进程级 IO 实测:trailer 阶段 monibuca 仅写盘 46 MB**(31 路合计),对比修复前 fallback 全量重写需写 ≈13 GB —— **降低约 99.6%**。0 失败、0 超时、产物标准 moov-first MP4。

---

## 1. 测试参数

| 项 | 值 |
|---|---|
| 流数 | 31 路 RTSP（`192.168.12.x`） |
| 录制时长 | 1800s（30min） |
| record start → stop | 15:48:12 → 16:18:21 |
| 存储后端 | MinIO 单节点 `172.16.12.130:9000`，桶 `vidu-media-bucket` |
| 磁盘 | `/dev/sda4` ext4（SSD），monibuca 暂存 + MinIO 数据 + 系统同分区 |

## 2. 系统 IO 数据（核心)

采集方式:全程并行采两路 —— 进程级 `/proc/<pid>/io`(monibuca pid 3899304 / MinIO pid 7194,每秒)+ 整盘 `iostat -dx 1 sda`。进程级数据能把 monibuca 的 trailer 写入从混杂的整盘 IO 中**精确分离**。

### 2.1 进程级 IO（`/proc/pid/io` write_bytes / read_bytes 增量)

| 窗口 | monibuca 写 | monibuca 读 | MinIO 写 |
|---|---|---|---|
| **录制期** 15:48:12–16:18:21(30min） | **13006 MB** | — | 7 MB |
| **trailer+上传期** 16:18:21 起 | **45.95 MB** | 12997 MB | 13021 MB |

解读:

- **录制期 monibuca 写 13006 MB** —— 31 路 RTSP 媒体数据持续写入各自的本地暂存文件(S3File 上传前的 tempFile),30min 累计 ≈13 GB。这是录制本身的固有写入,任何方案都一样。
- **trailer 期 monibuca 写 45.95 MB** —— 这是 **trailer 重写的实际磁盘写入**。31 路合计 46 MB,平均每路 ≈1.5 MB(= 各路 moov 大小 + 4K 对齐)。`INSERT_RANGE` 在文件头就地撑开 moov 空间,`mdat` 数据零移动,故 trailer 只写 moov。
- **trailer 期 monibuca 读 12997 MB** —— stop 后读回 31 个暂存文件作为上传源(≈13 GB)。
- **trailer 期 MinIO 写 13021 MB** —— MinIO 接收 31 路上传并落盘到 `/data/minio`(≈13 GB)。

### 2.2 整盘 iostat（`sda`)

| 指标 | 峰值 |
|---|---|
| `wkB/s` 写带宽峰值 | **567 MB/s**（top: 567 / 480 / 403 / 360 / 359 MB/s) |
| `%util` | 86.5% |

整盘 567 MB/s 写峰值出现在 trailer+上传期,**主体是 MinIO 落盘那 13 GB**(§2.1),与 monibuca trailer 的 46 MB 不在一个量级。整盘 iostat 无法单独反映 trailer —— 这正是本次用 `/proc/pid/io` 进程级采集的原因。

### 2.3 trailer 写入对照

| 方案 | trailer 阶段 monibuca 写盘 | 说明 |
|---|---|---|
| 修复前 fallback 全量重写 | ≈13 GB | 把整个 `mdat` 复制一遍(≈ 录制期写入量) |
| **阶段 A `INSERT_RANGE` fast path** | **46 MB** | 仅写 moov,`mdat` 零移动 |
| **降幅** | **≈ -99.6%** | |

## 3. 功能验收

| 项 | 结果 |
|---|---|
| `insert-range fast path done`(日志) | **31 / 31** |
| fast path 异常(skip/rollback/failed) | **0** |
| `upload successful` | **31 / 31** |
| `RequestTimeout`(MinIO 锁) | **0** |
| `saved failed` / `pending_uploads` | **0** / 0 |
| record 入库 | **31 条**,`endTime` 全正常,duration 1803–1809s（~30min） |
| 产物 ffprobe（cam1 `2026-05-21-15-48-12_239445148.mp4`,93 MB） | box 序 `ftyp→moov→free→free→mdat`,moov-first,`duration=1809.188`,h264 1920x1080 ✅ |

## 4. 结论

阶段 A(`fallocate INSERT_RANGE`)在 31 路 × 30min 真实负载下系统 IO 表现:

- trailer 阶段 monibuca 磁盘写入 **46 MB**(进程级实测),较 fallback 全量重写的 ≈13 GB **降低约 99.6%**;每路 trailer 写入从「≈整个录像文件」降到「≈一个 moov」(~1.5 MB)。
- `mdat` 数据零移动,产物为标准 progressive moov-first MP4。
- 31/31 走 fast path,0 异常 / 0 上传失败 / 0 补传 / 0 `RequestTimeout`,录像全部入库。
- 整盘写峰值 567 MB/s 来自 MinIO 接收上传落盘(31 路 ≈13 GB),属上传固有成本,与 trailer 优化无关;若需进一步降整盘峰值,应将 MinIO 数据盘与 monibuca 暂存盘分离(见 plan 风险节)。

## 5. 附:数据采集说明

- 进程级 IO:`/proc/3899304/io`(monibuca)、`/proc/7194/io`(MinIO)的 `write_bytes`/`read_bytes`,每秒采样,取窗口端点增量。`write_bytes` 为进程提交到块设备层的字节数,反映真实磁盘写入。
- `verify-130.sh` 自动报告(`reports/verify-130-20260521T154704.md`)的「磁盘写峰值 3 MB/s」来自 `docker stats`,host 网络模式下不可靠,不作依据。
- trailer 窗口起点取 record stop 时刻(16:18:21):stop 后 monibuca 不再写录像数据,其 `write_bytes` 增量即纯 trailer 写入。
