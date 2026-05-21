# MP4 Trailer 零重写 实施计划 —— fallocate 原地插 moov + fMP4 旁路

> **For agentic workers:** REQUIRED SUB-SKILL: 用 superpowers:subagent-driven-development 或 superpowers:executing-plans 逐 Task 执行。Step 用 checkbox(`- [ ]`)跟踪。

**Goal:** 消除 `record stop` 时 MP4 trailer 重写的 `O(整个录像大小)` 磁盘 IO。当前(`2026-05-16` plan 阶段 A 之后)trailer 仍把整个 `mdat`(单文件 ~300MB-1GB)完整复制一遍只为把 `moov` 移到文件头。本 plan 分两阶段把它降到接近 0:

- **阶段 A**:`fallocate(FALLOC_FL_INSERT_RANGE)` 在文件头就地为 `moov` 撑开空间,`mdat` 数据零移动。trailer 磁盘写 `O(mdat)` → `O(moov)`(~1-2MB)。产物仍是标准 progressive MP4,**兼容性零风险**,默认对所有现有 mp4 录制生效。
- **阶段 B**:对 fMP4(fragmented MP4)录制,`moov` 天生在头部,`record stop` 时连 `INSERT` 都不需要,trailer 完全旁路。是更彻底的形态,但产物为 fMP4,需兼容性确认。

阶段 A 先落地(不改录制格式),阶段 B 再作为可选的更优路径;两者按 muxer 模式共存。

---

## 现状与瓶颈

**录制阶段文件布局**(`muxer.go:76-99 WriteInitSegment`,非 fragment 分支):

```
[ftyp 32B][free 8B][mdat box header 8B][mdat data ...]
                                       ^ mdatOffset = 48
```

`BeforeMdatData = 16`(`record.go:57`)= free 8 + mdat header 8。

**trailer 阶段**(`writeTrailerTask`,`record.go:44` `Start()` + `record.go:65` `Run()`):

- `Start()` → `muxer.WriteTrailer()`(`muxer.go:264`,非 fragment 分支)→ `reWriteMdatSize()` 回填 mdat size + `WriteMoov()` 把 moov **append 到文件尾**。此后文件 = `[ftyp][free][mdat][moov]`。这个尾部 moov 仅作崩溃兜底,成功路径不使用。
- `Run()`(`record.go:65`)→ 把 `[ftyp][moov][mdat]` 写进一个新临时文件:
  - `record.go:105` `io.CopyN(bw, t.file, mdatOffset-BeforeMdatData)` —— 复制 ftyp(32B)
  - `record.go:114` `WriteMoov(bw)` —— 写 moov
  - **`record.go:119` `io.CopyN(bw, t.file, mdatSize+BeforeMdatData)` —— 复制整个 `[free][mdat header][mdat data]`,这是 `O(mdat)` 的全量写**
  - 阶段 2 通过 `TempFileFinalizer` 把临时文件移交给 `storage.File`(对象存储=上传源 / 本地=rename)

**`2026-05-16` plan 阶段 A 消除的是"第二遍回拷"(temp→覆盖回目标),不是重写本身。** `record.go:119` 这笔 `O(mdat)` 全量写依然存在 —— `moov` 通常只有 1-2MB,却要为它搬运 300MB-1GB 的 `mdat`,比例约 1:200。`2026-05-20` 验证报告 §4.3 "IO 量降到 ~moov 大小" 的描述与代码不符。

**环境已确认**(`2026-05-20` 验证,130 UAT):

- `/dev/sda4` = **ext4**,`rotational=0`(**SSD**),kernel 4.19。
- ext4(extent-based)+ kernel 4.1+ → **支持 `FALLOC_FL_INSERT_RANGE`**。
- `golang.org/x/sys v0.41.0` 已在 `go.mod:176` → `unix.Fallocate` 可用。

---

## 方案总览

`writeTrailerTask.Run()` 改为三层分派:

```
1. muxer.isFragment()             → 阶段 B:fMP4 旁路,直接 Close 上传,trailer 0 写入
2. t.file 支持 RangeInserter 能力 → 阶段 A:fallocate INSERT_RANGE 原地插 moov,O(moov) 写入
3. 都不满足                        → 现状 fallback:io.CopyN 全量重写到 temp,O(mdat) 写入
```

第 3 层是现有已合入代码,**完整保留**作为兜底(非 Linux 平台、tmpfs/NFS 等不支持 `INSERT_RANGE` 的文件系统、异常 box 布局)。

---

## 文件结构

```
pkg/storage/
├── fallocate_linux.go      [新] InsertRange / CollapseRange 封装 (//go:build linux)
├── fallocate_other.go      [新] 非 Linux stub,返回 ErrRangeInsertUnsupported
├── fallocate_test.go       [新] 在 t.TempDir() 上验证 INSERT_RANGE 行为(linux)
├── rangeinsert.go          [新] RangeInserter 接口 + ErrRangeInsertUnsupported
├── local.go                [改] LocalFile 实现 RangeInserter
├── s3.go / oss.go / cos.go [改] S3File/OSSFile/COSFile 实现 RangeInserter
└── (finalize.go            [不改] TempFileFinalizer 仍用于 fallback 路径)

plugin/mp4/pkg/
├── record.go               [改] writeTrailerTask.Run() 三层分派 + INSERT fast path + fMP4 bypass
├── record_test.go          [改] 加 INSERT fast path / fMP4 bypass 的产物校验
└── box/box.go              [改] isNil() 修 fMP4 box 树 typed-nil panic(阶段 B)

example/record-test/config.yaml  [改] 加 type:fmp4 录制配置示例注释
docs/superpowers/plans/
└── 2026-05-21-trailer-zero-rewrite.md  [新] 本文件
```

---

# 阶段 A —— fallocate(INSERT_RANGE)原地插 moov

## 原理

`FALLOC_FL_INSERT_RANGE`:在文件指定偏移插入一段空间,该偏移之后的数据**逻辑后移、物理 extent 不动**(O(extent数) 的元数据操作)。约束:`offset` 与 `length` 必须是文件系统块大小(4096)的整数倍。

trailer fast path 流程(在 `t.file` 的底层本地 `*os.File` 上原地操作):

```
录制完(Start 之后):  [ftyp 32][free 8][mdat hdr 8][mdat data][尾部 moov]
                       └──────── 48B ────────┘

1. moovSize = t.muxer.moov.Size()              // Start() 已 MakeMoov,直接取
2. M = align4096(32 + moovSize + 8)            // [0,M) 容纳 [新ftyp][moov][padding free]
3. 各 track 的 Samplelist[i].Offset += M        // mdat 逻辑后移 M
4. moov2 = muxer.MakeMoov(); 断言 moov2.Size()==moovSize  // 不等(跨 4GB)→ 放弃 fast path
5. InsertRange(fd, 0, M)                        // [hole M][ftyp][free][mdat hdr][mdat data][尾部moov]
6. 写 [0, M):  [ftyp 32B] + [moov] + [free padding box 填到 M]
7. 写 [M, M+8):free box header(size=40)        // 把暴露出来的旧 [ftyp 32][free 8] 包成 free box
8. ftruncate 到 (48 + mdatSize + M)             // 截掉尾部 moov(Start 写的崩溃兜底副本)
9. Sync → Close                                 // LocalFile 就位 / S3File 上传

结果:  [ftyp][moov][padding free][free 40][mdat hdr][mdat data]   —— 标准 moov-first MP4
磁盘写入: M(≈moov,1-2MB)+ 8B,mdat 数据零移动
```

**崩溃一致性**:第 5 步 `INSERT_RANGE` 原子;第 6-9 步写入 ~moov 大小(1-2MB,几十 ms)。崩溃窗口比现状(写 300MB-1GB temp,秒级)**小一个数量级**。崩溃后文件为 `[hole][ftyp][free][mdat][尾部moov]` —— 尾部 moov 仍在(`Start()` 写的),`mdat` 数据完好,可人工 `COLLAPSE_RANGE` 去 hole 恢复为可播文件。这是相对"先写完整 temp 再替换"的唯一退步点,但窗口更小,判定为可接受(见风险节)。

## Task A1: storage 加 fallocate 封装 + RangeInserter 接口

**Files:** Create `pkg/storage/fallocate_linux.go`, `pkg/storage/fallocate_other.go`, `pkg/storage/rangeinsert.go`, `pkg/storage/fallocate_test.go`

- [ ] **Step 1**: `pkg/storage/rangeinsert.go` 定义接口与哨兵错误:

```go
package storage

import (
	"errors"
	"os"
)

// ErrRangeInsertUnsupported 表示当前平台或文件系统不支持 fallocate INSERT_RANGE。
// 调用方收到此错误应回退到全量重写路径。
var ErrRangeInsertUnsupported = errors.New("storage: range insert unsupported")

// RangeInserter 是 storage.File 的可选能力:暴露承载数据的底层本地文件句柄,
// 供 MP4 trailer 用 fallocate(INSERT_RANGE) 在文件头就地插入 moov,避免全量重写。
// 仅本地文件 / 对象存储后端的本地暂存文件可实现。
type RangeInserter interface {
	// LocalFd 返回底层本地文件句柄。句柄所有权仍属 storage.File,
	// 调用方可读写 / fallocate / ftruncate,但不得 Close 它。
	LocalFd() *os.File
}
```

- [ ] **Step 2**: `pkg/storage/fallocate_linux.go`:

```go
//go:build linux

package storage

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// InsertRange 在 f 的 offset 处插入 length 字节空间(均须 4096 对齐)。
// offset 之后的数据逻辑后移、物理不动。文件系统不支持时返回 ErrRangeInsertUnsupported。
func InsertRange(f *os.File, offset, length int64) error {
	err := unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_INSERT_RANGE, offset, length)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.ENOSYS) {
		return ErrRangeInsertUnsupported
	}
	return err
}
```

- [ ] **Step 3**: `pkg/storage/fallocate_other.go`:

```go
//go:build !linux

package storage

import "os"

// InsertRange 在非 Linux 平台恒返回 ErrRangeInsertUnsupported,调用方回退全量重写。
func InsertRange(f *os.File, offset, length int64) error {
	return ErrRangeInsertUnsupported
}
```

- [ ] **Step 4**: `pkg/storage/fallocate_test.go`(`//go:build linux`):在 `t.TempDir()` 写一个已知内容文件,`InsertRange(f, 0, 4096)`,验证:① 文件大小 +4096 ② 原内容整体后移 4096 ③ `[0,4096)` 可写入并回读。`t.TempDir()` 若在不支持的 fs 上,`InsertRange` 返回 `ErrRangeInsertUnsupported` → `t.Skip`。

- [ ] **Step 5**: 编译 + 测试

```bash
go build ./pkg/storage/...
GOOS=darwin go build ./pkg/storage/...   # 验证非 Linux stub 编译通过
go test -race -run 'TestInsertRange' ./pkg/storage/...
```

- [ ] **Step 6**: 提交 `feat(storage): 加 fallocate INSERT_RANGE 封装 + RangeInserter 接口`

## Task A2: 四后端实现 RangeInserter

**Files:** Modify `pkg/storage/local.go`, `pkg/storage/s3.go`, `pkg/storage/oss.go`, `pkg/storage/cos.go`

- [ ] **Step 1**: 确认各后端底层本地句柄字段(已知:`LocalFile` 内嵌 `*os.File`(`local.go:630`);`S3File.tempFile *os.File`(`s3.go:324`)。OSS/COS grep 确认):

```bash
grep -nE 'tempFile|type (OSSFile|COSFile) struct' pkg/storage/oss.go pkg/storage/cos.go
```

- [ ] **Step 2**: `LocalFile` 实现(`local.go`,接 `FinalizeFromTemp` 之后):

```go
// LocalFd 实现 storage.RangeInserter。
func (f *LocalFile) LocalFd() *os.File { return f.File }
```

- [ ] **Step 3**: `S3File` 实现(`s3.go`)。注意 `S3File` 的 Read/Write/Seek 全部代理到 `tempFile`,trailer 在 `tempFile` 上 `INSERT_RANGE` 后,`Close()` 仍以 `tempFile` 为上传源 —— 行为自洽:

```go
// LocalFd 实现 storage.RangeInserter,返回上传前的本地暂存文件句柄。
func (w *S3File) LocalFd() *os.File { return w.tempFile }
```

OSS/COS 同构。四个文件末尾各加编译期断言 `var _ RangeInserter = (*XxxFile)(nil)`。

- [ ] **Step 4**: 编译四后端(`go build` 默认 + `-tags s3/oss/cos`)+ `go test -race ./pkg/storage/...`(+ `-tags s3`)无回归。

- [ ] **Step 5**: 提交 `feat(storage/local,s3,oss,cos): 四后端实现 RangeInserter`

## Task A3: writeTrailerTask.Run() 走 INSERT fast path

**Files:** Modify `plugin/mp4/pkg/record.go`

**前置事实**(已核对):`muxer` 不需改动 —— `MakeMoov()`、`CreateFTYPBox()`、`WriteMoov()` 均 public,`Tracks`/`moov`/`mdatOffset`/`mdatSize` 为同包字段,`record.go` 已直接访问。`Start()` 不改(保留尾部 moov 作崩溃兜底)。

- [ ] **Step 1**: 在 `writeTrailerTask.Run()`(`record.go:65`)开头、`os.CreateTemp` 之前,插入 fast path 分派。新增一个方法 `runInsertRangeFastPath() (handled bool, err error)`:

  - 仅当 `t.file` 实现 `storage.RangeInserter` 且 `!t.muxer.isFragment()` 且文件头为标准 `[ftyp 32][free 8][mdat hdr 8]` 布局(`mdatOffset == 48`,即未触发 `reWriteMdatSize` 的 64-bit large mdat 分支)时尝试;否则 `return false, nil` 交回原路径。
  - 流程按本节"原理"的 9 步。`InsertRange` 返回 `ErrRangeInsertUnsupported` → `return false, nil`(回退,不算失败)。
  - 步骤 4 `MakeMoov()` 后断言 `moov2.Size() == moovSize`;不等(单文件接近 4GB,stco→co64 升级)→ `return false, nil` 回退。
  - `INSERT_RANGE` 成功之后的任何失败(写 moov / ftruncate / Sync / Close)→ 走与现有 `recoverToPending` 等价的补传逻辑:此刻底层文件已被 INSERT 改写,但 mdat 完好且尾部 moov 仍在,登记 pending 由补传兜底。
  - 成功路径末尾调用 `t.dbWrite(&writeTrailerQueueTask)`,与现有一致。

```go
func (t *writeTrailerTask) Run() (err error) {
	t.Info("write trailer")
	defer func() {
		if err != nil && t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}()

	// 阶段 B:fMP4 的 moov 天生在头部,无需重写(见 Task B1)
	if t.muxer.isFragment() {
		return t.runFragmentBypass()
	}
	// 阶段 A:progressive MP4 走 fallocate INSERT_RANGE 原地插 moov
	if handled, e := t.runInsertRangeFastPath(); handled {
		return e
	}
	// fallback:现有全量重写到 temp(以下保持不变)
	...
}
```

- [ ] **Step 2**: 实现 `runInsertRangeFastPath`。关键片段:

```go
// 返回 handled=false 表示未处理(应回退);handled=true 表示已接管(成功或已登记补传)。
func (t *writeTrailerTask) runInsertRangeFastPath() (handled bool, err error) {
	inserter, ok := t.file.(storage.RangeInserter)
	if !ok || t.muxer.mdatOffset != 48 {
		return false, nil
	}
	fd := inserter.LocalFd()

	ftyp := t.muxer.CreateFTYPBox()           // 32B
	moovSize := int64(t.muxer.moov.Size())    // Start() 已 MakeMoov
	const blk = 4096
	M := ((int64(ftyp.Size()) + moovSize + box.BasicBoxLen + blk - 1) / blk) * blk

	for _, track := range t.muxer.Tracks {
		for i := range track.Samplelist {
			track.Samplelist[i].Offset += M
		}
	}
	moov := t.muxer.MakeMoov()
	if int64(moov.Size()) != moovSize {       // 跨 4GB,stco→co64,放弃 fast path
		return false, nil                     // 注:offset 已改,回退路径会再 MakeMoov,需确认幂等(Step 3)
	}

	if e := storage.InsertRange(fd, 0, M); e != nil {
		if errors.Is(e, storage.ErrRangeInsertUnsupported) {
			return false, nil
		}
		return false, e                       // INSERT 前失败,文件未改,可安全回退/报错
	}
	// —— 此后底层文件已被改写,失败一律走补传,不可回退 ——
	... 写 [0,M):ftyp + moov + free padding ...
	... 写 [M,M+8):size=40 的 free box header ...
	... ftruncate 到 48+mdatSize+M(截掉尾部 moov)...
	... Sync ...
	... t.file.Close() 失败 → recoverToPending ...
	... t.dbWrite(...) ...
	return true, nil
}
```

- [ ] **Step 3**: 处理回退幂等性。`runInsertRangeFastPath` 在步骤 4 之后回退(`return false`)时,`Samplelist[].Offset` 已被 `+= M`。回退路径(现有全量重写)`record.go:109-113` 还会再 `+= moov.Size()`。**两种处理择一**:① fast path 回退前把 offset 减回去(`-= M`);② fast path 仅在确定能继续(即 `INSERT_RANGE` 之前所有检查通过)后才改 offset。**采用 ②**:把 offset 调整移到 `InsertRange` 成功之后。重排步骤为:先 `MakeMoov` 拿 `moovSize` 与布局校验 → `InsertRange` → 成功后才改 offset + 重新 `MakeMoov`。

- [ ] **Step 4**: 编译 + mp4 单测

```bash
go build ./plugin/mp4/... && go build -tags s3 ./plugin/mp4/...
go test ./plugin/mp4/... -count=1
```

- [ ] **Step 5**: `record_test.go` 加用例:本地后端录一个短 mp4 → 触发 trailer → fast path 生效 → `ffprobe` 校验 moov 在 mdat 前、可解码、时长正确;再用一个不实现 `RangeInserter` 的 mock File 验证回退路径仍 PASS。

- [ ] **Step 6**: 提交 `feat(mp4): writeTrailerTask 走 fallocate INSERT_RANGE 原地插 moov`

## Task A4: 阶段 A 验收

- [ ] **Step 1**: 单测全过 `go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...`(+ `-tags s3`)。

- [ ] **Step 2**: 130 端到端(复用 `example/record-test/verify-130.sh`)。出镜像 → 部署 → 31 路 × 15min 录制 → stop。**量化验收**:
  - 停录窗口宿主 `iostat -dx 0.2 sda` 采样:trailer 阶段 sda 写入应从"全量 mdat 重写"塌缩到仅 moov 量级(31 路合计约几十 MB,单秒峰值预期 << 阶段 A 现状的 104.5 MB/s)。
  - `ffprobe` 31 路:全部可解码,时长 900±5s,`moov` 在 `mdat` 前(`ffprobe -v trace` 看 box 顺序 / `mp4dump`)。
  - `pending_uploads` 最终为 0;DB `record_streams` 记录数 = 31,`end_time` 正常。

- [ ] **Step 3**: 对照 `2026-05-20` 报告补一行真实数据,作为阶段 A 在 130 上的首个有效对照。

---

# 阶段 B —— fMP4 旁路(trailer 完全跳过)

## 原理

fMP4(`muxer` 的 `FLAG_FRAGMENT`)文件结构为 `[ftyp][moov(init)][moof][mdat][moof][mdat]...[mfra]`,`moov` 录制一开始即写在头部。`record stop` 时:

- `Start()` → `muxer.WriteTrailer()`(`muxer.go:265` fragment 分支)→ 写 `mfra`(随机访问索引)到文件尾。**无 moov-first 重写**。
- `Run()` 当前**不区分 fragment**,仍走全量重写路径 —— 而 fMP4 的 `mdatOffset/mdatSize` 恒为 0(`WriteInitSegment` 的 fragment 分支不设置),导致 `record.go:105` `io.CopyN(..., 0-16)` 等错误行为。**当前 fMP4 录制 + trailer 是损坏的**,这是 Task B1 要修的核心。

正确做法:fMP4 的 `Run()` 直接 `Close()` 触发上传,trailer 阶段 **0 字节重写**。

> 注:工作区曾 stash 一版 `if t.muxer.isFragment() { bypass }` 草稿(含 `box.go isNil()` 修复、`server.go OnUploadFailed` 注册)。本阶段将其正式化并补全验证。执行前先 `git stash list` 确认,避免重复。

## Task B1: writeTrailerTask 加 fMP4 bypass

**Files:** Modify `plugin/mp4/pkg/record.go`

- [ ] **Step 1**: 实现 `runFragmentBypass`(Task A3 Step 1 已在 `Run()` 开头接好分派):

```go
// fMP4 的 moov 已在头部,Start() 已写完 mfra,trailer 无需任何重写,直接持久化。
func (t *writeTrailerTask) runFragmentBypass() (err error) {
	if stat, e := t.file.Stat(); e == nil {
		t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", stat.Size()))
	}
	if t.durationMs > 0 {
		t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
	}
	if err = t.file.Close(); err != nil {
		t.file = nil
		// 与 progressive 路径一致:上传失败登记 pending 补传(需 metadata + db)
		... recoverFragmentToPending(err) ...
		return
	}
	t.file = nil
	if t.dbWrite != nil {
		t.dbWrite(&writeTrailerQueueTask)
	}
	return nil
}
```

  fMP4 上传失败的补传:fMP4 文件就是 `t.file` 承载的本地文件(S3File.tempFile / LocalFile),`Close()` 失败后该文件仍完整,登记 `MoveToPendingDir` + `SaveFailedUpload`。复用现有 `recoverToPending` 思路,抽一个不依赖 `tempPath` 的版本。

- [ ] **Step 2**: 编译 + 单测;加 fMP4 录制单测:`NewMuxerWithStreamPath(FLAG_FRAGMENT, ...)` 录短流 → trailer → `ffprobe` 校验产物为合法 fMP4(含 `moof`/`mfra`,`moov` 在头)。

- [ ] **Step 3**: 提交 `fix(mp4): fMP4 录制 trailer 直接旁路,不做 moov-first 重写`

## Task B2: fMP4 box 树 typed-nil panic 修复

**Files:** Modify `plugin/mp4/pkg/box/box.go`

- [ ] **Step 1**: 复现。fMP4 录制路径构造 box 树时,`CreateContainerBox` / `WriteTo`(`box.go`)对 `IBox` 接口值调用 `reflect.ValueOf(child).IsNil()` —— 当 `child` 为 typed-nil(如 `makeTrak` 中 `edts` 在 fMP4 下为 nil 的 `*ContainerBox`)时,`reflect.ValueOf` 拿到的是非指针 Kind 或直接 panic。`muxer.go:229 CreateContainerBox(TypeTRAK, tkhd, mdia, edts)` 在 fMP4 下 `edts == nil`。

- [ ] **Step 2**: 加 `isNil()` helper,对非可空 Kind 安全返回 false:

```go
func isNil(i IBox) bool {
	if i == nil {
		return true
	}
	v := reflect.ValueOf(i)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Map, reflect.Pointer, reflect.UnsafePointer, reflect.Interface, reflect.Slice:
		return v.IsNil()
	}
	return false
}
```

  把 `CreateContainerBox` 与 `WriteTo` 里的 `reflect.ValueOf(x).IsNil()` 替换为 `isNil(x)`。

- [ ] **Step 3**: 加单测:`CreateContainerBox` 传入 typed-nil 子 box 不 panic 且被正确跳过。编译 + `go test ./plugin/mp4/pkg/box/...`。

- [ ] **Step 4**: 提交 `fix(mp4/box): CreateContainerBox/WriteTo 修 typed-nil 子 box panic`

## Task B3: 录制配置 type:fmp4 打通 + 文档

**Files:** Modify `example/record-test/config.yaml`(+ 按需 `example/default/config.yaml`)

- [ ] **Step 1**: 确认链路。`record.go:334` `if r.Event.Type == "fmp4"` → `NewMuxerWithStreamPath(FLAG_FRAGMENT, ...)`;`r.Event.Type` 源自录制配置 `RecConf.Type`(`record.go:306`)。grep 确认 `Type` 字段从 yaml / API 到 `RecConf` 的完整传递路径,补缺口。

- [ ] **Step 2**: 在 `config.yaml` 录制配置块加注释示例:`type: fmp4` 启用 fragmented MP4(trailer 零重写;产物为 fMP4)。

- [ ] **Step 3**: 提交 `docs(record-test): 加 type:fmp4 录制配置示例`

## Task B4: fMP4 兼容性 / seek 验收

- [ ] **Step 1**: 130 端到端:`type: fmp4` 跑 31 路 × 15min → stop。验收:`pending_uploads`=0;trailer 阶段 sda 写入 ≈ 0(仅 `mfra` + 上传读)。

- [ ] **Step 2**: **兼容性矩阵**(关键决策项,需产品确认下游消费方):
  - `ffprobe` / `ffmpeg` 解码、转封装 PASS;
  - 目标 Web 播放器(`<video>` 直链 / MSE / HLS-DASH)播放 + **随机 seek** PASS;
  - 若下游存在旧播放器 / 硬件解码器,实测确认;不通过则 fMP4 仅作为可选项,默认仍 progressive + 阶段 A。

- [ ] **Step 3**: 报告归档,更新决策结论(默认录制格式是否切 fMP4 / 还是 progressive+阶段A 为默认、fMP4 为可选)。

---

## 风险与缓解

| 风险 | 缓解 |
|---|---|
| 原地 `INSERT_RANGE` 后、`moov` 写完前崩溃 → 文件头损坏 | 窗口 ~几十 ms(写 1-2MB),比现状写 temp 的秒级窗口小一个数量级;尾部 moov(`Start()` 写)+ 完好 mdat 仍在,可 `COLLAPSE_RANGE` 人工恢复。判定可接受;如需更强保证,future 可加 sidecar moov 备份。 |
| 文件系统不支持 `INSERT_RANGE`(tmpfs/NFS/旧 fs/非 Linux) | `InsertRange` 返回 `ErrRangeInsertUnsupported` → 自动回退现有全量重写路径,功能不受影响。 |
| S3File 的 `tempFile` 落在不支持的 fs(如 `/tmp` 为 tmpfs) | Task A2 Step 1 确认 `tempFile` 创建目录;必要时让其与录像目录同挂载点。回退路径兜底。 |
| 单文件接近 4GB,`offset += M` 致 stco→co64 升级、`moov` 变大 | Step A3 校验 `MakeMoov` 前后 `Size()` 一致,不等则回退全量重写。 |
| 4096 非实际 fs 块大小 | ext4/xfs 默认 4096;`INSERT_RANGE` 参数非对齐会返回 `EINVAL`,可在 A1 测试中暴露。如需严谨可 `unix.Statfs` 读 `f_bsize`。 |
| fMP4 兼容性 / seek 不达标 | 阶段 B 独立于阶段 A;不达标则 fMP4 仅作可选,progressive+阶段A 仍为默认且已根治大头。 |

## Self-Review

- **Spec 覆盖**:"trailer 磁盘 IO 降到接近 0" → 阶段 A(`O(mdat)`→`O(moov)`,Task A1-A4)+ 阶段 B(`O(moov)`→`0`,Task B1-B4)。
- **分阶段可独立发布**:阶段 A 不改录制格式、产物标准 mp4、对所有现有录制默认生效、`INSERT_RANGE` 不可用时自动回退 —— 零兼容风险,可单独合入。阶段 B 改录制格式,需兼容性验收,独立合入、可选启用。
- **不改动**:`Start()`(保留尾部 moov 崩溃兜底);trailer 队列单线程模型;`TempFileFinalizer` 与现有全量重写路径(降级为 fallback,完整保留)。
- **类型一致性**:`InsertRange(f *os.File, offset, length int64) error`(A1 定义,A3 调用);`RangeInserter.LocalFd() *os.File`(A1 定义,A2 四后端实现,A3 类型断言);`ErrRangeInsertUnsupported` 哨兵错误贯穿。
- **与 `2026-05-16` plan 的关系**:本 plan 不否定其阶段 A(消除回拷仍有效,且是本 plan fallback 路径的一部分);本 plan 进一步消除回拷之外的"那一遍全量写"。其阶段 B 限速器(`TrailerWriteRateMBps`)在 trailer 降到 `O(moov)` 后基本不再需要,保留不动。
- **遗留**:`example/record-test/integrity-130.sh` 的 MinIO 侧 `ffprobe` 校验(`2026-05-20` 报告 §5.4)可在 Task A4/B4 验收中一并补齐。
