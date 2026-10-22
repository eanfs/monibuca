# 半成品 MP4 反向重建可行性（Task 0 spike 结论）

日期：2026-10-22
样本来源：`172.16.12.201:/home/project/xde-xd/media-docker-compose/monibuca/tmp/s3writer_896943576.tmp`
（生产机 `monibuca/tmp` 下同批还有 200+ 个同类 `.tmp`）

## 1. 结论（TL;DR）

**能。** 没有 moov 的半成品 progressive-MP4 临时文件可以反向重建为可播放 MP4：

- **视频：100% 可恢复**（H.264/AVCC，无需 ffmpeg，用本仓 `plugin/mp4/pkg/box` 即可写盘）。
- **音频：本样本可恢复**（G.711A/`pcm_alaw`，8 kHz 单声道），但属于**启发式**恢复，泛化性有限；
  非定长音频（如 AAC）在 tmp 里没有帧长信息，**不可可靠恢复**。
- **时间戳：只能估算**。tmp 内没有任何时间戳/时长元数据，只能从 SPS VUI 取标称帧率，
  再按均匀帧间隔外推。可变帧率、丢帧、抖动不可知，长录像会有可见的 A/V 漂移（见 §5）。
- **元数据：不可恢复**（真实起录时间、时长、旋转、分片边界、流路径等）。

因此 Task 7 可以按「离线扫 NAL → 重建 moov → 原样拷贝 mdat 载荷」实现，
音频作为 best-effort 附加项，且必须能降级为只恢复视频。

## 2. 半成品文件的真实布局

不是裸 mdat，也不是 Annex B 起始码；这是**未 finalize 的 progressive MP4**：

| 偏移 | 内容 |
|---|---|
| 0 | `ftyp`（32 B，`isom/iso2/avc1/mp41`） |
| 32 | `free`（8 B） |
| 40 | `mdat` box header，**size 字段是占位值 8**（正常 finalize 时才回写真实大小） |
| 48 | 之后全部是样本载荷，按录像写入顺序排列，**没有任何样本边界/大小表** |

载荷内两类样本交错：

- **视频样本**：`[4 字节大端长度][NAL]` 重复（AVCC，`lengthSizeMinusOne=3`）。
  一个访问单元（一帧）通常就是 `[SPS][PPS][slice]` 或 `[slice]`。
- **音频样本**：**裸编码帧，没有长度前缀**。这就是扫描时唯一的“断点”。

## 3. 样本实测数据

`ffprobe /tmp/s3writer_896943576.tmp` 基线：

```
[mov,mp4,m4a,3gp,3g2,mj2] moov atom not found
... Invalid data found when processing input
```

扫描结果（探针逐 4 字节长度链式前进，长度/NAL 头不合理时逐字节重同步）：

- 文件 712,559,794 B；载荷 712,559,746 B。
- 34,773 个 NALU：type 1（非 IDR slice）32,763；type 5（IDR）670；type 7（SPS）670；type 8（PPS）670。
- SPS 只有 **1 种**（`674d0028e900f0044fcb0800001f4800061c102000`，Main/Level 4.0），
  每个 IDR 前重复一次；PPS 同理。→ 不需要处理参数集切换。
- 按「VCL NAL 结束一个 AU」分组：**33,433 个视频样本 / 670 个 IDR**；
  IDR 间隔 50 帧（除文件开头第一个间隔 17 帧），即 25 fps 下约 2 s 一个关键帧。
- SPS VUI 解析出的标称帧率 = **25 fps**（用 `codec.NewH264CtxFromRecord().FPS()`），分辨率 1920×1080。
- 重同步“空洞”共 30,911 段、10,728,320 B（1.51%），长度分布：
  `320 B × 28319`、`640 × 2571`、`960 × 19`、`1280 × 2` —— **全部是 320 的整数倍**。
- 320 B / 帧 = 8 kHz × 40 ms，且字节几乎全是 `0x55`/`0xD5`（A-law 静音），
  因此判定为 **G.711A（PCMA）8 kHz 单声道**，共 **33,526 个音频帧**。
  这也解释了现场既有修复脚本里“去掉文件尾 `0x55/0xD5`”的经验写法。

重建产物 `/tmp/m7s-sample1.mp4`（712,965,095 B）：

```
Stream #0 video: h264, 1920x1080, duration 1337.32 s, 33433 frames
Stream #1 audio: pcm_alaw, 8000 Hz, mono, duration 1341.04 s, 33526 frames
```

- `ffprobe` 正常解析双轨；`ffmpeg -v error -i ... -f null -` **全片 22 分钟解码无任何
  错误输出**；任意 seek（`-ss 600`）正常。
- 音频解码后是静音（s16 幅度 ±24，即 A-law 静音量化噪声），与字节统计一致。
- 交叉验证：现场 `repair_mp4.py` 早已把同一 tmp 用「AVCC→Annex B→ffmpeg remux」
  恢复成 `recovered_mp4/s3writer_896943576.mp4`（**仅视频**，1337.28 s / 33,432 帧）。
  两条独立路径的帧数/时长基本一致（33,433 vs 33,432 帧），说明视频扫描与边界判定正确。

## 4. 重建方法（Task 7 可照抄）

1. 校验头部：`ftyp` @0、`mdat` @40、载荷起点 48；拒绝其它形态。
2. 从 48 开始走 AVCC：
   - 读 4 字节长度 `n`；要求 `0 < n <= 上限`（探针取 8 MiB）且 `pos+4+n <= EOF`；
   - 读 NAL 头，要求 `forbidden_zero_bit == 0` 且 `1 <= type <= 23`；
   - 不满足则 `pos++` 重同步，并把这段记为“空洞”（音频候选）。
3. 分组：遇到 VCL（type 1–5）后，下一个 NAL 开启新样本；样本内含 type 5 即为关键帧。
   SPS/PPS（type 7/8）直接留作 `avcC`。
4. 时间戳：SPS VUI 有 `timing_info` 就用其帧率；没有则回退（默认 25 fps，允许配置）。
   `stts` 可用单条目 `{count=N, delta=1000/fps}`；`mvhd`/`mdhd` timescale=1000。
5. 写容器（无需 ffmpeg）：
   - 视频：`stsz`=每样本字节数、`stco`=每样本在文件中的偏移（1 chunk/sample）、
     `stsc`=单条目、`stss`=IDR 样本号、`stsd`=avc1 + 手写 `avcC`；
   - **两遍写 moov**：先用占位偏移算出 moov 大小，再据此算 `payloadStart` 回填
     `stco`；本样本偏移均 < 4 GiB，`stco` 不会升 `co64`，两遍大小一致（不一致必须报错）；
   - `ftyp | moov | mdat`，mdat 内视频样本按顺序原样拷贝（AVCC 前缀保持不变）。
6. 音频（best-effort）：若所有空洞都是同一常数 `F` 的整数倍，按 `F` 切帧；
   若帧内容以 `0x55/0xD5`（A-law）或 `0xFF/0x7F`（µ-law）静音字节为主，选对应
   `alaw`/`ulaw` sample entry（timescale=8000、每帧样本数=F）；否则**丢弃音频并告警**，
   只输出视频。

## 5. 已知局限

- **A/V 漂移**：本样本音频 33526 帧 × 40 ms = 1341.04 s，视频 33433 帧 × 40 ms = 1337.32 s，
  差 3.72 s（0.28%）。tmp 不记录真实时间戳，视频丢帧无法感知。若同时恢复了音频，
  可用音频总时长反推视频帧间隔并分摊，但只能减小漂移，不能消除。
- **音频不可泛化**：AAC 等变长帧在 tmp 中没有长度前缀，帧边界不可知 → 只能报“音频不可恢复”。
  G.711 也不能 100% 区分 A-law/µ-law，只能靠静音字节/统计启发式。
- **末尾截断**：最后一个不完整 NAL/音频帧会被自然丢弃（长度校验失败并重同步）；
  文件尾可能残留 `0x55/0xD5` 填充。把样本截到 100 MB 实测：重同步会在尾部产生一个
  176 KB 的超大“空洞”——切音频时必须拒绝非固定倍数/超大空洞，否则会把损坏的 NAL
  残片误当音频写进音轨。
- **元数据全丢**：起录时间、分片、旋转、流路径需从文件名/数据库/mtime 外部补齐。
- **性能**：本探针用 `ReadAt` 逐 4 字节读，712 MB 仅扫描约 52 s（user 11 s / sys 20 s，
  系统调用占大头）；扫描+重建完整跑约 63 s。Task 7 应改为顺序缓冲读
  （`bufio.Reader` + 大块比较），预计可降到秒级。

## 6. 样本为何不入库

`/tmp/m7s-sample1.tmp` **712 MB**，是生产摄像头录像（含真实业务画面），
既超出 git 合理体积，也不应把现场视频提交到仓库。复现方式：

```bash
sshpass -f ~/.ssh/xd_pass ssh root@172.16.12.201 \
  'ls -S /home/project/xde-xd/media-docker-compose/monibuca/tmp/s3writer_*.tmp | head -1'
# scp 该文件到 /tmp，然后：
ffprobe -v error /tmp/<name>.tmp     # 预期 moov atom not found
```

Task 7 的自动化测试不要依赖此样本，应使用由本仓 muxer 生成、故意不写 moov 的小样本
（几秒 320×240 合成流），大样本只用于人工/现场验证。
