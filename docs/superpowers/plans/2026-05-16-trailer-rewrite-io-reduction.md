# MP4 Trailer 重写 磁盘 IO 削减 实施计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 削减 `record stop` 时 MP4 trailer 重写的磁盘写入量与峰值带宽：分两阶段——阶段 A 消除「temp → 目标文件」的全量回拷（每个录像 2× 文件大小 → 1×，约降 50%）；阶段 B 给 trailer 临时文件写入加可配置限速器，把剩余写入的峰值压到目标值（如 300 MB/s）。

**Architecture:**
- **问题定位（已验证）**：`writeTrailerQueueTask`（`task.Work`）的 event loop 是单线程的——`event_loop.go:136` 用 `v.start()` 处理子任务，`task.go:387-390` 中 `start()` **同步调用** `Run()`。所以 31 个 trailer 严格串行，并发信号量（旧 plan `2026-05-15-trailer-flush-rate-limit.md`）从来都是死代码，其回滚是正确的。
- **真正的瓶颈**：`writeTrailerTask.Run()` 为把 moov 移到文件头，把媒体数据**写了两遍盘**——先 `io.CopyN` 原文件 mdat → `/tmp` 临时文件（全量 #1），再 `io.Copy` 整个临时文件 → 覆盖回目标文件（全量 #2）。31 路 ×〜300MB ×2 ≈ 18 GB 在 stop 时以串行流连续打出，单条顺序写流即可把 SSD 打满到 ~1.1 GB/s。
- **阶段 A（方案 1）**：临时文件本身已是完整的 moov-first MP4。新增可选能力接口 `storage.TempFileFinalizer`，让 `storage.File` 直接接管这份临时文件——对象存储后端（S3/OSS/COS）把它作为上传源（省去 temp→temp 回拷），本地后端 `os.Rename` 到目标路径（同盘零写入）。`writeTrailerTask.Run()` 通过类型断言走快路径，保留旧 `io.Copy` 作为 fallback。
- **阶段 B（方案 4）**：trailer 串行后，剩余的大块磁盘写入只剩「写临时文件」这一笔。新增 `throttledWriter` 令牌桶限速器（手写，不引入 `golang.org/x/time/rate` 依赖），用可配置速率包住临时文件写入，把尖峰压成平台。

**Tech Stack:** Go 标准库（`os` / `io` / `bufio` / `time`）；可选能力接口 + 类型断言；手写累积式限速 writer。

**修复范围：**
- 改 `pkg/storage`：新增 `TempFileFinalizer` 接口 + 后端实现 + 限速器 + 一个配置字段
- 改 `plugin/mp4/pkg/record.go`：`writeTrailerTask.Run()` 走快路径 + 限速
- **不动** trailer 队列的串行模型（gotask framework，且串行本身不是 bug）
- **不动** 旧 plan 遗留的 `trailerSem` / `MaxConcurrentTrailerWrites` [预留] 字段（保持现状，超出本 plan 范围）
- **不动** flv plugin（flv 不做 moov 重写，无此问题）

**Spec：** 见本文件首段 + Task A5 / Task B4 验收节。无独立 spec 文件。

---

## 文件结构

```
pkg/storage/
├── finalize.go            [新] TempFileFinalizer 接口 + adoptUploadTempFile 辅助函数
├── finalize_test.go       [新] adoptUploadTempFile + LocalFile.FinalizeFromTemp 单测
├── throttle.go            [新] throttledWriter 限速器 + NewTrailerThrottledWriter
├── throttle_test.go       [新] 限速器行为单测
├── local.go               [改] LocalFile 实现 FinalizeFromTemp
├── s3.go                  [改] S3File 实现 FinalizeFromTemp
├── oss.go                 [改] OSSFile 实现 FinalizeFromTemp
├── cos.go                 [改] COSFile 实现 FinalizeFromTemp
└── upload_manager.go      [改] UploadConfig 加 TrailerWriteRateMBps 字段 + Init 时读入

plugin/mp4/pkg/
└── record.go              [改] writeTrailerTask.Run() 走 TempFileFinalizer 快路径 + 限速写入

example/record-test/config.yaml  [改] 加 trailerwriteratembps 配置示例注释

docs/superpowers/plans/
└── 2026-05-16-trailer-rewrite-io-reduction.md  [新] 本文件
```

---

# 阶段 A —— 方案 1：消除 trailer 回拷

## Task A1: pkg/storage 加 TempFileFinalizer 接口 + adoptUploadTempFile 辅助函数

**Files:**
- Create: `pkg/storage/finalize.go`
- Create: `pkg/storage/finalize_test.go`

- [ ] **Step 1: 写失败测试**

文件 `pkg/storage/finalize_test.go`：

```go
package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// 写一个带内容的文件, 返回路径
func writeTempWithContent(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestAdoptUploadTempFile_ReplacesOldFile(t *testing.T) {
	dir := t.TempDir()
	oldPath := writeTempWithContent(t, dir, "old.tmp", "OLD")
	srcPath := writeTempWithContent(t, dir, "src.tmp", "NEW-CONTENT")

	old, err := os.OpenFile(oldPath, os.O_RDWR, 0644)
	if err != nil {
		t.Fatalf("open old: %v", err)
	}

	f, err := adoptUploadTempFile(old, oldPath, srcPath)
	if err != nil {
		t.Fatalf("adoptUploadTempFile: %v", err)
	}
	defer f.Close()

	// 旧文件应被删除
	if _, statErr := os.Stat(oldPath); !os.IsNotExist(statErr) {
		t.Fatalf("old file should be removed, stat err = %v", statErr)
	}
	// 返回的句柄应指向 src 内容
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	if got := string(buf[:n]); got != "NEW-CONTENT" {
		t.Fatalf("adopted file content = %q, want %q", got, "NEW-CONTENT")
	}
}

func TestAdoptUploadTempFile_NilOldHandle(t *testing.T) {
	dir := t.TempDir()
	srcPath := writeTempWithContent(t, dir, "src.tmp", "DATA")
	f, err := adoptUploadTempFile(nil, "", srcPath)
	if err != nil {
		t.Fatalf("adoptUploadTempFile with nil old: %v", err)
	}
	defer f.Close()
}

func TestAdoptUploadTempFile_SrcMissing(t *testing.T) {
	f, err := adoptUploadTempFile(nil, "", "/nonexistent/path/xyz.tmp")
	if err == nil {
		f.Close()
		t.Fatal("expected error when srcPath missing")
	}
}
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
go test -run 'TestAdoptUploadTempFile' ./pkg/storage/...
```

预期：`undefined: adoptUploadTempFile`。

- [ ] **Step 3: 写实现**

文件 `pkg/storage/finalize.go`：

```go
package storage

import "os"

// TempFileFinalizer 是 storage.File 的可选能力。实现者可直接接管调用方
// 已经写好的一个完整本地文件作为自身内容，避免再做一次全量拷贝。
//
// 背景：MP4 录制结束时 trailer 重写会把 moov 移到文件头，先把 [ftyp][moov][mdat]
// 写进一个临时文件，再整体覆盖回目标文件——媒体数据被写了两遍盘。临时文件本身
// 已是完整的 moov-first MP4，通过本接口直接移交，可省掉第二遍全量写入。
//
// 调用方先把完整内容写到一个本地临时文件，然后调用 FinalizeFromTemp，再调用
// File.Close() 完成最终持久化。
type TempFileFinalizer interface {
	// FinalizeFromTemp 让本 File 以 srcPath 指向的完整本地文件作为其最终内容。
	// 成功返回后 srcPath 的所有权移交给实现方，调用方不得再删除或写入它；
	// 失败时 srcPath 仍归调用方所有。
	// 调用之后再调用 File.Close() 完成最终持久化：
	//   - 对象存储后端（S3/OSS/COS）：上传该文件
	//   - 本地后端：文件已 rename 到目标路径，Close 仅关闭句柄
	FinalizeFromTemp(srcPath string) error
}

// adoptUploadTempFile 供对象存储后端（S3/OSS/COS）的 FinalizeFromTemp 复用：
// 关闭并删除旧的（通常为空的）内部临时文件，然后以读写方式打开 srcPath 接管它。
// 返回打开的文件句柄；失败时 srcPath 不被接管（调用方仍负责清理），旧文件已被关闭并删除。
func adoptUploadTempFile(old *os.File, oldPath, srcPath string) (*os.File, error) {
	if old != nil {
		old.Close()
	}
	if oldPath != "" && oldPath != srcPath {
		os.Remove(oldPath)
	}
	return os.OpenFile(srcPath, os.O_RDWR, 0644)
}
```

- [ ] **Step 4: 跑测试，确认 3 个 case 全 PASS**

```bash
go test -race -run 'TestAdoptUploadTempFile' ./pkg/storage/...
```

预期：`PASS`。

- [ ] **Step 5: 提交**

```bash
git add pkg/storage/finalize.go pkg/storage/finalize_test.go
git commit -m "feat(storage): 加 TempFileFinalizer 接口, 为 trailer 重写消除回拷做准备"
```

---

## Task A2: LocalFile 实现 FinalizeFromTemp

**Files:**
- Modify: `pkg/storage/local.go`
- Modify: `pkg/storage/finalize_test.go`

- [ ] **Step 1: 写失败测试**

在 `pkg/storage/finalize_test.go` 末尾追加：

```go
func TestLocalFileFinalizeFromTemp_SameDir(t *testing.T) {
	dir := t.TempDir()
	destPath := filepath.Join(dir, "dest.mp4")

	// 模拟 LocalStorage.CreateFile：以 O_RDWR 打开目标文件
	destFile, err := os.OpenFile(destPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	lf := &LocalFile{destFile}

	srcPath := writeTempWithContent(t, dir, "rewritten.tmp", "MOOV-FIRST-MP4")
	if err := lf.FinalizeFromTemp(srcPath); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	// 调用方随后会 Close
	if err := lf.Close(); err != nil {
		t.Fatalf("Close after finalize: %v", err)
	}

	// src 应已移走
	if _, statErr := os.Stat(srcPath); !os.IsNotExist(statErr) {
		t.Fatalf("src should be moved away, stat err = %v", statErr)
	}
	// dest 应含 src 内容
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != "MOOV-FIRST-MP4" {
		t.Fatalf("dest content = %q, want %q", string(got), "MOOV-FIRST-MP4")
	}
}

// 验证 LocalFile 实现了 TempFileFinalizer 接口（编译期断言）
var _ TempFileFinalizer = (*LocalFile)(nil)
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
go test -run 'TestLocalFileFinalizeFromTemp' ./pkg/storage/...
```

预期：`*LocalFile does not implement TempFileFinalizer (missing method FinalizeFromTemp)`。

- [ ] **Step 3: 在 `pkg/storage/local.go` 加实现**

在文件末尾 `SetMetadata` 之后、`init()` 之前加：

```go
// FinalizeFromTemp 用 srcPath 指向的完整文件替换本地目标文件。
// 优先用 os.Rename（同盘移动，零数据写入）；跨设备时回退到复制+删除。
// 实现 storage.TempFileFinalizer，供 mp4 trailer 重写消除全量回拷。
func (f *LocalFile) FinalizeFromTemp(srcPath string) error {
	destPath := f.File.Name()
	// 关闭目标文件当前句柄：录制阶段写入的旧内容会被 src 整体取代。
	if err := f.File.Close(); err != nil {
		return fmt.Errorf("close dest before finalize: %w", err)
	}
	if err := os.Rename(srcPath, destPath); err != nil {
		// 跨设备（如 /tmp 与录像目录不同挂载点）：复制后删除源。
		if copyErr := copyFileContents(srcPath, destPath); copyErr != nil {
			return fmt.Errorf("cross-device finalize: %w", copyErr)
		}
		os.Remove(srcPath)
	}
	// 重新打开目标文件，使后续 Close() 仍然有效。
	reopened, err := os.OpenFile(destPath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("reopen dest after finalize: %w", err)
	}
	f.File = reopened
	return nil
}

// copyFileContents 把 src 内容完整复制到 dst（含 fsync），用于跨设备 finalize。
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}
```

注：`local.go` 已 import `fmt` / `io` / `os`，无需改 import 区。

- [ ] **Step 4: 跑测试，确认 PASS**

```bash
go test -race -run 'TestLocalFileFinalizeFromTemp|TestAdoptUploadTempFile' ./pkg/storage/...
```

预期：`PASS`，且编译期断言 `var _ TempFileFinalizer = (*LocalFile)(nil)` 通过。

- [ ] **Step 5: 提交**

```bash
git add pkg/storage/local.go pkg/storage/finalize_test.go
git commit -m "feat(storage/local): LocalFile 实现 FinalizeFromTemp (rename, 跨设备回退复制)"
```

---

## Task A3: S3 / OSS / COS 三后端实现 FinalizeFromTemp

**Files:**
- Modify: `pkg/storage/s3.go`
- Modify: `pkg/storage/oss.go`
- Modify: `pkg/storage/cos.go`

- [ ] **Step 1: 看三后端 File struct 的临时文件字段名**

```bash
grep -nE 'tempFile|filePath|^type (S3File|OSSFile|COSFile) struct' pkg/storage/s3.go pkg/storage/oss.go pkg/storage/cos.go
```

预期：
- `S3File`：字段 `tempFile *os.File`、`filePath string`、接收器 `w`（见 `s3.go:319-328`）
- `OSSFile` / `COSFile`：同样有 `tempFile` / `filePath`，接收器 `f`

**若 OSS/COS 的字段名或接收器名与下方代码不一致，按实际名替换后再继续。**

- [ ] **Step 2: S3File 加 FinalizeFromTemp**

在 `pkg/storage/s3.go` 中 `func (w *S3File) Stat()` 之后加：

```go
// FinalizeFromTemp 让 S3File 直接以 srcPath 指向的完整文件作为上传源，
// 省去调用方「temp → S3File 内部 temp」的全量回拷。
// 后续 Close() 会上传该文件；上传成功删除它，失败保留它供补传。
// 实现 storage.TempFileFinalizer。
func (w *S3File) FinalizeFromTemp(srcPath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := adoptUploadTempFile(w.tempFile, w.filePath, srcPath)
	if err != nil {
		w.tempFile = nil
		return fmt.Errorf("finalize from temp: %w", err)
	}
	w.tempFile = f
	w.filePath = srcPath
	return nil
}
```

并在文件末尾 `init()` 之前加编译期断言：

```go
var _ TempFileFinalizer = (*S3File)(nil)
```

- [ ] **Step 3: OSSFile 加 FinalizeFromTemp**

在 `pkg/storage/oss.go` 中 OSSFile 的 `Stat()` 方法之后加（接收器名 `f` 按 Step 1 实际值；`mu` 字段若不存在则去掉锁两行）：

```go
// FinalizeFromTemp 让 OSSFile 直接以 srcPath 指向的完整文件作为上传源。
// 实现 storage.TempFileFinalizer。详见 s3.go 同名方法。
func (f *OSSFile) FinalizeFromTemp(srcPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	adopted, err := adoptUploadTempFile(f.tempFile, f.filePath, srcPath)
	if err != nil {
		f.tempFile = nil
		return fmt.Errorf("finalize from temp: %w", err)
	}
	f.tempFile = adopted
	f.filePath = srcPath
	return nil
}
```

并在文件末尾加：

```go
var _ TempFileFinalizer = (*OSSFile)(nil)
```

- [ ] **Step 4: COSFile 加 FinalizeFromTemp**

在 `pkg/storage/cos.go` 中 COSFile 的 `Stat()` 方法之后加（接收器名按 Step 1 实际值）：

```go
// FinalizeFromTemp 让 COSFile 直接以 srcPath 指向的完整文件作为上传源。
// 实现 storage.TempFileFinalizer。详见 s3.go 同名方法。
func (f *COSFile) FinalizeFromTemp(srcPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	adopted, err := adoptUploadTempFile(f.tempFile, f.filePath, srcPath)
	if err != nil {
		f.tempFile = nil
		return fmt.Errorf("finalize from temp: %w", err)
	}
	f.tempFile = adopted
	f.filePath = srcPath
	return nil
}
```

并在文件末尾加：

```go
var _ TempFileFinalizer = (*COSFile)(nil)
```

- [ ] **Step 5: 编译三后端（带各自 build tag）**

```bash
go build -tags s3 ./pkg/storage/...
go build -tags oss ./pkg/storage/...
go build -tags cos ./pkg/storage/...
```

预期：全部通过。若报「`adoptUploadTempFile` undefined」检查 Task A1 是否已合入；若报字段名错按 Step 1 实际名修正。

- [ ] **Step 6: 跑 storage 测试无回归**

```bash
go test -race -count=1 ./pkg/storage/...
go test -race -count=1 -tags s3 ./pkg/storage/...
```

预期：原有 + 新增测试全 PASS。

- [ ] **Step 7: 提交**

```bash
git add pkg/storage/s3.go pkg/storage/oss.go pkg/storage/cos.go
git commit -m "feat(storage/s3,oss,cos): 三后端实现 FinalizeFromTemp, 直接以现成文件为上传源"
```

---

## Task A4: writeTrailerTask.Run() 走 TempFileFinalizer 快路径

**Files:**
- Modify: `plugin/mp4/pkg/record.go`

- [ ] **Step 1: 确认现状**

```bash
sed -n '57,214p' plugin/mp4/pkg/record.go
```

预期看到 `BeforeMdatData` 常量（行 57）+ `func (t *writeTrailerTask) Run() (err error)`（行 64 起），且 `Run()` 内含 `io.Copy(t.file, temp)` 全量回拷（约行 164）。

确认 `OnUploadFailed` 当前未被赋值（即对象存储后端的失败回调是死代码，trailer 失败处理只走 record.go 的 `MoveToPendingDir`）：

```bash
grep -rn 'OnUploadFailed *=' --include='*.go' .
```

预期：0 行。这保证 Step 3 改写后失败补传不会与 storage 层重复处理。

- [ ] **Step 2: 备份对照——记录当前 Run() 行为要点**

当前 `Run()` 的关键行为（改写后必须保持）：
1. mdat 重写产生完整 moov-first MP4
2. 设置元数据 `video-size-bytes` / `video-duration-ms`
3. `t.file.Close()` 失败时把重写后的临时文件移入 pending 目录 + `SaveFailedUpload` 入库
4. 成功时调 `t.dbWrite(&writeTrailerQueueTask)`
5. 任何错误路径关闭 `t.file`
6. `pkg.ErrSkip` 时返回 `task.ErrTaskComplete`

- [ ] **Step 3: 用下面整段替换 `func (t *writeTrailerTask) Run()` 整个方法**

替换 record.go 中从 `func (t *writeTrailerTask) Run() (err error) {`（行 64）到该方法结束 `}`（行 214）的全部内容为：

```go
// 将 moov 从文件末尾移动到文件头：先把 [ftyp][moov][mdat] 写入临时文件，
// 然后让 storage.File 承载这份临时文件。
//
// 阶段 2 优先走 TempFileFinalizer 快路径：临时文件本身已是完整 moov-first MP4，
// 直接移交给 storage.File（对象存储=上传源；本地=rename 到目标路径），省去一次
// 全量回拷。未实现 TempFileFinalizer 的 File 回退到旧的 io.Copy 路径。
func (t *writeTrailerTask) Run() (err error) {
	t.Info("write trailer")

	// 确保任何错误路径下 t.file 都被关闭
	defer func() {
		if err != nil && t.file != nil {
			t.file.Close()
			t.file = nil
		}
	}()

	var temp *os.File
	temp, err = os.CreateTemp("", "*.mp4")
	if err != nil {
		t.Error("create temp file", "err", err)
		return
	}
	tempPath := temp.Name()
	// tempOwned 表示 tempPath 文件当前是否仍由本函数负责删除。
	// 移交给 storage.File（FinalizeFromTemp 成功）或移入 pending 目录后置 false。
	tempOwned := true
	defer func() {
		temp.Close()
		if tempOwned {
			os.Remove(tempPath)
		}
	}()

	// ---- 阶段 1：把 [ftyp][moov][mdat] 写入临时文件 ----
	if _, err = t.file.Seek(0, io.SeekStart); err != nil {
		t.Error("seek file", "err", err)
		return
	}
	// 使用带缓冲的 writer 减少写入 syscall（moov 由大量小块组成）
	bw := bufio.NewWriterSize(temp, 1<<20) // 1 MB write buffer

	// 复制 mdat box 之前的内容
	if _, err = io.CopyN(bw, t.file, int64(t.muxer.mdatOffset)-BeforeMdatData); err != nil {
		t.Error("copy pre-mdat data", "err", err)
		return
	}
	for _, track := range t.muxer.Tracks {
		for i := range len(track.Samplelist) {
			track.Samplelist[i].Offset += int64(t.muxer.moov.Size())
		}
	}
	if err = t.muxer.WriteMoov(bw); err != nil {
		t.Error("write moov to temp", "err", err)
		return
	}
	// 复制 mdat box
	if _, err = io.CopyN(bw, t.file, int64(t.muxer.mdatSize)+BeforeMdatData); err != nil {
		if err == pkg.ErrSkip {
			return task.ErrTaskComplete
		}
		t.Error("rewrite with mdat", "err", err)
		return
	}
	if err = bw.Flush(); err != nil {
		t.Error("flush temp file", "err", err)
		return
	}

	// 验证临时文件完整性
	tempStat, statErr := temp.Stat()
	if statErr != nil {
		err = statErr
		t.Error("stat temp file", "err", err)
		return
	}
	expectedSize := tempStat.Size()
	if expectedSize == 0 {
		err = fmt.Errorf("temp file is empty after MOOV rewrite")
		t.Error("temp file empty", "err", err)
		return
	}

	// 在最终持久化前设置元数据（文件大小 + 时长）
	metadata := map[string]string{
		"video-size-bytes": fmt.Sprintf("%d", expectedSize),
	}
	t.file.SetMetadata("video-size-bytes", fmt.Sprintf("%d", expectedSize))
	if t.durationMs > 0 {
		metadata["video-duration-ms"] = fmt.Sprintf("%d", t.durationMs)
		t.file.SetMetadata("video-duration-ms", fmt.Sprintf("%d", t.durationMs))
	}

	// ---- 阶段 2：让 storage.File 承载这份临时文件 ----
	if finalizer, ok := t.file.(storage.TempFileFinalizer); ok {
		// 快路径：直接移交 tempPath，省去全量回拷。
		if err = finalizer.FinalizeFromTemp(tempPath); err != nil {
			t.Error("finalize from temp", "err", err)
			return
		}
		tempOwned = false // 所有权已移交 t.file
	} else {
		// 回退路径：旧的全量回拷（供未实现 TempFileFinalizer 的 File）。
		if _, err = t.file.Seek(0, io.SeekStart); err != nil {
			t.Error("seek file for overwrite", "err", err)
			return
		}
		if _, err = temp.Seek(0, io.SeekStart); err != nil {
			t.Error("seek temp file", "err", err)
			return
		}
		var written int64
		if written, err = io.Copy(t.file, temp); err != nil {
			t.Error("copy temp to file", "err", err, "written", written, "expected", expectedSize)
			return
		}
		if written != expectedSize {
			err = fmt.Errorf("MOOV rewrite incomplete: expected %d bytes, wrote %d", expectedSize, written)
			t.Error("incomplete overwrite", "err", err)
			return
		}
	}

	// ---- 阶段 3：Close 触发最终持久化（对象存储=上传；本地=已就位）----
	if err = t.file.Close(); err != nil {
		t.Error("upload failed after retries", "err", err,
			"filePath", t.filePath, "streamPath", t.streamPath,
			"storageType", t.storageKey, "durationMs", t.durationMs)
		t.file = nil
		// 上传失败：MOOV 重写后的文件仍保留在 tempPath（storage.File 上传失败时
		// 不删除它）。移入 pending 目录并入库，供定时补传。
		if t.db != nil {
			if pendingPath, moveErr := storage.MoveToPendingDir(tempPath); moveErr == nil {
				tempOwned = false // 已移走，不需 defer 删除
				m7s.SaveFailedUpload(t.db, pendingPath, t.filePath, t.storageKey,
					t.streamPath, expectedSize, t.durationMs, metadata, err)
				t.Info("saved failed upload for retry",
					"pendingPath", pendingPath, "objectKey", t.filePath)
			} else {
				t.Error("move to pending dir failed", "err", moveErr)
			}
		}
		return
	}
	t.file = nil
	// 文件已完整持久化，此时才将记录写入数据库（延迟入库，确保 DB 与可播放文件一致）。
	if t.dbWrite != nil {
		t.dbWrite(&writeTrailerQueueTask)
	}
	return
}
```

**说明 / 与旧实现的差异：**
- 旧实现用 `tempCleanup` 同时表达「是否删 temp」和「temp 是否可用于恢复」两层语义，改写后只用 `tempOwned`（是否负责删 temp）单一语义，恢复逻辑改为「`t.db != nil` 即尝试」。
- 对象存储快路径下，`FinalizeFromTemp` 后 `tempPath` 归 `t.file`；`Close()` 上传成功时后端 `cleanup(true)` 删除它，失败时 `cleanup(false)` 保留它——所以阶段 3 失败分支 `MoveToPendingDir(tempPath)` 时文件确实存在。
- 本地快路径下 `FinalizeFromTemp` 已 rename 到目标路径，`Close()` 仅关句柄，几乎不会失败。
- `OnUploadFailed`（storage 层回调）当前全程未被赋值（Step 1 已验证），不会与本处补传重复。

- [ ] **Step 4: 编译验证（默认 + 本地，以及对象存储 tag）**

```bash
go build ./plugin/mp4/...
go build -tags s3 ./plugin/mp4/...
```

预期：通过。

- [ ] **Step 5: 跑 mp4 全测试无回归**

```bash
go test ./plugin/mp4/... -count=1
```

预期：原有测试全 PASS。

- [ ] **Step 6: 提交**

```bash
git add plugin/mp4/pkg/record.go
git commit -m "feat(mp4): writeTrailerTask 走 TempFileFinalizer 快路径, 消除 trailer 全量回拷

trailer 重写原本把媒体数据写两遍盘 (原文件→temp, temp→覆盖回原文件).
临时文件本身已是完整 moov-first MP4, 通过 storage.TempFileFinalizer 直接
移交: 对象存储作为上传源, 本地 rename 到目标路径. 每个录像磁盘写入量
2× 文件大小 → 1×. 未实现该接口的 File 回退到旧 io.Copy 路径."
```

---

## Task A5: 阶段 A 验收

- [ ] **Step 1: 单测全过**

```bash
go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...
go test -race -count=1 -tags s3 ./pkg/storage/...
```

期望全 PASS。

- [ ] **Step 2: 接口实现 grep 自检**

```bash
echo "=== TempFileFinalizer 编译期断言 (应 4 处) ==="
grep -rn 'var _ TempFileFinalizer' pkg/storage/

echo "=== record.go 走快路径 (应 1 处类型断言) ==="
grep -n 'TempFileFinalizer\|FinalizeFromTemp' plugin/mp4/pkg/record.go
```

预期：4 处断言（local/s3/oss/cos）+ record.go 1 处 `storage.TempFileFinalizer` 断言。

- [ ] **Step 3: 端到端冒烟（130 测试环境，对象存储路径）**

出镜像 → 部署到 130（复用 `2026-05-15-*` plan 里的部署流程）→ 31 路 5min 录制 → stop。

**量化验收：**
- 监控 stop 时 `disk_w_kbps` 峰值：修复前 baseline ≈ 1126 MB/s；阶段 A 后预期**显著下降**（每文件写入量减半，峰值与持续时间均下降）。记录实测值。
- `pending_uploads` 数量应为 0（沿用 `2026-05-15-storage-upload-detach-recorder-context` 的 cancel 修复）。
- ffprobe 31 路：PASS=31，时长 300±5 sec，moov 在文件头（`ffprobe -v trace` 看 moov 早于 mdat）。

- [ ] **Step 4: 阶段 A 完成确认**

阶段 A 已把每个录像的 trailer 磁盘写入从 2× 降到 1×。若 Step 3 实测峰值已满足业务要求，阶段 B 可视情况推迟；若仍需把峰值硬性压到目标值（如 300 MB/s），继续阶段 B。

---

# 阶段 B —— 方案 4：trailer 写盘限速

## Task B1: pkg/storage 加 throttledWriter 限速器

**Files:**
- Create: `pkg/storage/throttle.go`
- Create: `pkg/storage/throttle_test.go`
- Modify: `pkg/storage/upload_manager.go`

- [ ] **Step 1: 写失败测试**

文件 `pkg/storage/throttle_test.go`：

```go
package storage

import (
	"bytes"
	"testing"
	"time"
)

func TestThrottledWriter_PacesThroughput(t *testing.T) {
	var sink bytes.Buffer
	// 10 MB/s 限速，写 5 MB，应耗时约 0.5s
	w := newThrottledWriter(&sink, 10*1024*1024)
	data := make([]byte, 1024*1024) // 1 MB chunk

	start := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := w.Write(data); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	elapsed := time.Since(start)

	if elapsed < 350*time.Millisecond {
		t.Fatalf("throttle too fast: 5MB at 10MB/s took %v, want >=350ms", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("throttle too slow: 5MB at 10MB/s took %v, want <=2s", elapsed)
	}
	if sink.Len() != 5*1024*1024 {
		t.Fatalf("sink got %d bytes, want %d", sink.Len(), 5*1024*1024)
	}
}

func TestThrottledWriter_ZeroRateNoLimit(t *testing.T) {
	var sink bytes.Buffer
	w := newThrottledWriter(&sink, 0) // 0 = 不限速
	data := make([]byte, 8*1024*1024)

	start := time.Now()
	if _, err := w.Write(data); err != nil {
		t.Fatalf("write: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("zero-rate writer should not sleep, took %v", elapsed)
	}
	if sink.Len() != 8*1024*1024 {
		t.Fatalf("sink got %d bytes, want %d", sink.Len(), 8*1024*1024)
	}
}
```

- [ ] **Step 2: 跑测试，确认失败**

```bash
go test -run 'TestThrottledWriter' ./pkg/storage/...
```

预期：`undefined: newThrottledWriter`。

- [ ] **Step 3: 写实现**

文件 `pkg/storage/throttle.go`：

```go
package storage

import (
	"io"
	"time"
)

// trailerWriteBytesPerSec 是 trailer 写盘限速速率（字节/秒），0 = 不限速。
// 由 InitUploadManager 从 UploadConfig.TrailerWriteRateMBps 读入，运行时只读。
var trailerWriteBytesPerSec int64

// throttledWriter 把底层 writer 的累计写入速率限制在 bytesPerSec 以内，
// 用于把 record stop 时 trailer 重写的磁盘写入从尖峰压成平台。
// 采用累积式配速：每次 Write 后按「已写字节数 / 速率」算出应耗时间，
// 若实际耗时不足则 sleep 补齐——不会因单块过大而过冲。
type throttledWriter struct {
	w           io.Writer
	bytesPerSec int64
	start       time.Time
	written     int64
}

// newThrottledWriter 返回一个限速到 bytesPerSec 的 writer。
// bytesPerSec <= 0 时直接返回原 writer（不限速、零开销）。
func newThrottledWriter(w io.Writer, bytesPerSec int64) io.Writer {
	if bytesPerSec <= 0 {
		return w
	}
	return &throttledWriter{w: w, bytesPerSec: bytesPerSec}
}

func (t *throttledWriter) Write(p []byte) (int, error) {
	if t.start.IsZero() {
		t.start = time.Now()
	}
	n, err := t.w.Write(p)
	t.written += int64(n)
	if err != nil {
		return n, err
	}
	expected := time.Duration(float64(t.written) / float64(t.bytesPerSec) * float64(time.Second))
	if sleep := expected - time.Since(t.start); sleep > 0 {
		time.Sleep(sleep)
	}
	return n, err
}

// NewTrailerThrottledWriter 用全局 trailer 限速速率包装 w。
// 速率为 0（默认/未配置）时返回 w 本身，无任何开销。
func NewTrailerThrottledWriter(w io.Writer) io.Writer {
	return newThrottledWriter(w, trailerWriteBytesPerSec)
}
```

- [ ] **Step 4: 在 `UploadConfig` 加配置字段**

`pkg/storage/upload_manager.go` 中 `UploadConfig` 结构体加一个字段（加在 `PendingDir` 之前或之后均可）：

```go
	TrailerWriteRateMBps int `desc:"trailer 重写写盘限速 (MB/s), 控制 record stop 时磁盘 burst; 0=不限速 (默认)" default:"0"`
```

- [ ] **Step 5: 在 `InitUploadManager` 读入该字段**

`InitUploadManager` 函数体末尾（其它字段初始化之后）加：

```go
	if cfg.TrailerWriteRateMBps > 0 {
		trailerWriteBytesPerSec = int64(cfg.TrailerWriteRateMBps) * 1024 * 1024
	} else {
		trailerWriteBytesPerSec = 0
	}
```

- [ ] **Step 6: 跑测试，确认 PASS**

```bash
go test -race -run 'TestThrottledWriter' ./pkg/storage/...
go test -race -count=1 ./pkg/storage/...
```

预期：限速测试 PASS，且原有 storage 测试无回归。

- [ ] **Step 7: 提交**

```bash
git add pkg/storage/throttle.go pkg/storage/throttle_test.go pkg/storage/upload_manager.go
git commit -m "feat(storage): 加 trailer 写盘限速器 + TrailerWriteRateMBps 配置 (默认 0=不限速)"
```

---

## Task B2: record.go 用限速 writer 包住 trailer 临时文件写入

**Files:**
- Modify: `plugin/mp4/pkg/record.go`

- [ ] **Step 1: 确认当前 bufio 行**

```bash
grep -n 'bufio.NewWriterSize' plugin/mp4/pkg/record.go
```

预期：1 处，`bw := bufio.NewWriterSize(temp, 1<<20)`（Task A4 改写后的 `Run()` 内）。

- [ ] **Step 2: 用限速 writer 包住临时文件**

把 record.go 中

```go
	bw := bufio.NewWriterSize(temp, 1<<20) // 1 MB write buffer
```

替换为

```go
	// trailer 重写后唯一的大块磁盘写入是这笔「写临时文件」。
	// 用限速 writer 包住 temp（速率由 storage.TrailerWriteRateMBps 配置；
	// 未配置时 NewTrailerThrottledWriter 直接返回 temp，零开销）。
	bw := bufio.NewWriterSize(storage.NewTrailerThrottledWriter(temp), 1<<20)
```

注：`record.go` 已 import `m7s.live/v5/pkg/storage`，无需改 import。

- [ ] **Step 3: 编译验证**

```bash
go build ./plugin/mp4/...
go build -tags s3 ./plugin/mp4/...
```

预期：通过。

- [ ] **Step 4: 跑 mp4 全测试无回归**

```bash
go test ./plugin/mp4/... -count=1
```

预期：全 PASS（默认配置速率 0，不限速，行为与阶段 A 完成时一致）。

- [ ] **Step 5: 提交**

```bash
git add plugin/mp4/pkg/record.go
git commit -m "feat(mp4): trailer 临时文件写入接入 storage 限速器"
```

---

## Task B3: 配置示例文档

**Files:**
- Modify: `example/record-test/config.yaml`

- [ ] **Step 1: 找 storage 配置块**

```bash
grep -n 'storage:\|maxconcurrentuploads\|pendingdir' example/record-test/config.yaml
```

- [ ] **Step 2: 在 `storage:` 块下加注释**

在 `example/record-test/config.yaml` 的 `storage:` 段内（与 `maxconcurrentuploads` 等同级）加：

```yaml
    # trailer 重写写盘限速 (MB/s)。record stop 多路并发时, trailer 重写会产生
    # 磁盘写入 burst。0 = 不限速 (默认)。需要把峰值压到目标值时设具体数值,
    # 例如 SSD 上设 300 表示限到 ~300 MB/s。本机 SSD 顺序写上限实测约 1.1 GB/s。
    # trailerwriteratembps: 0
```

- [ ] **Step 3: 提交**

```bash
git add example/record-test/config.yaml
git commit -m "docs(storage): TrailerWriteRateMBps 配置项示例注释"
```

---

## Task B4: 阶段 B 验收

- [ ] **Step 1: 单测全过**

```bash
go test -race -count=1 ./pkg/storage/... ./plugin/mp4/...
```

期望全 PASS。

- [ ] **Step 2: grep 自检**

```bash
echo "=== 限速器定义 ==="
grep -n 'throttledWriter\|NewTrailerThrottledWriter\|trailerWriteBytesPerSec\|TrailerWriteRateMBps' pkg/storage/throttle.go pkg/storage/upload_manager.go

echo "=== record.go 接入 ==="
grep -n 'NewTrailerThrottledWriter' plugin/mp4/pkg/record.go
```

预期：throttle.go 含定义；upload_manager.go 含配置字段 + 初始化；record.go 1 处接入。

- [ ] **Step 3: 端到端冒烟（130 测试环境，开限速）**

部署带阶段 A+B 的镜像，`config.yaml` 设 `trailerwriteratembps: 300`。

确认日志含限速生效（可在 `InitUploadManager` 处临时加日志，或看 stop 耗时变长）。

跑 31 路 5min 录制 → stop，监控 `disk_w_kbps`：

**量化验收：**
- stop 时 `disk_w_kbps` 峰值 **< 320 MB/s**（目标 ~300 MB/s）
- `pending_uploads` = 0
- ffprobe 31 路：PASS=31，时长 300±5 sec
- stop 总耗时会比不限速时变长（属预期：总写入量 ÷ 限速速率），记录实测值确认在可接受范围

- [ ] **Step 4: 对照确认**

| 指标 | baseline（修复前） | 阶段 A 后 | 阶段 A+B 后 |
|---|---|---|---|
| 每文件 trailer 磁盘写 | 2× 文件大小 | 1× 文件大小 | 1× 文件大小 |
| `disk_w_kbps` 峰值 | ~1126 MB/s | 实测填入 | < 320 MB/s |
| 录制完整性 (ffprobe) | — | PASS=31 | PASS=31 |

- [ ] **Step 5: 上线前补充说明**

在 PR 描述里写明：
- 修复对象：record stop 时 trailer 重写的磁盘 IO burst
- 阶段 A：消除全量回拷，每文件写入量减半（默认即生效，无需配置）
- 阶段 B：可选限速，`trailerwriteratembps` 默认 0（不限速），需要硬性峰值上限时按硬件配置
- 关联说明：旧 plan `2026-05-15-trailer-flush-rate-limit.md` 的并发信号量经验证为死代码（trailer 队列单线程），本 plan 改用「降写入量 + 限速率」两个正确杠杆替代

---

## Self-Review 记录

**Spec coverage：**
- "消除全量回拷" → Task A1（接口）+ A2（local）+ A3（s3/oss/cos）+ A4（record.go 走快路径）✓
- "trailer 写盘限速" → Task B1（限速器 + 配置）+ B2（record.go 接入）✓
- "分阶段实施" → 阶段 A（Task A1-A5）与阶段 B（Task B1-B4）各自独立可发布、可验收 ✓
- "把峰值压到目标值" → Task B4 量化验收 `disk_w_kbps < 320 MB/s` ✓

**Placeholder scan：**
- 无 TBD / TODO。
- Task A3 Step 1 要求先 grep 确认 OSS/COS 的字段名/接收器名——这是**有意的前置校验**（s3.go 已确认是 `tempFile`/`filePath`，oss/cos 极可能一致，但要求执行者 grep 确认而非盲改），非占位符。
- Task A5 Step 3 / B4 Step 3 的镜像 tag、130 部署细节复用既有 plan 流程，非占位符。

**Type consistency：**
- `TempFileFinalizer` 接口方法 `FinalizeFromTemp(srcPath string) error` —— Task A1 定义，A2/A3 实现，A4 类型断言使用，全一致。
- `adoptUploadTempFile(old *os.File, oldPath, srcPath string) (*os.File, error)` —— Task A1 定义，A3 三后端调用，签名一致。
- `newThrottledWriter(w io.Writer, bytesPerSec int64) io.Writer` / `NewTrailerThrottledWriter(io.Writer) io.Writer` —— Task B1 定义，B2 使用，一致。
- `trailerWriteBytesPerSec int64` 包级变量 —— B1 定义并由 `InitUploadManager` 赋值，`NewTrailerThrottledWriter` 读取。
- `TrailerWriteRateMBps int` 配置字段 —— B1 在 `UploadConfig` 定义，B3 yaml 注释引用名一致（yaml 小写 `trailerwriteratembps`）。
- `tempOwned bool` —— A4 `Run()` 内局部变量，替代旧 `tempCleanup`，单一语义。

**关键风险与缓解：**
- **失败补传重复**：A4 Step 1 验证 `OnUploadFailed` 未被赋值，确认 trailer 失败只走 record.go 的 `MoveToPendingDir`，不会与 storage 层重复。
- **本地跨设备**：`/tmp` 与录像目录不同挂载点时 `os.Rename` 失败 → `copyFileContents` 回退（= 与旧 `io.Copy` 等价，无回归，只是该情况下不省写入）。同盘时 rename 零写入。
- **限速默认关闭**：`TrailerWriteRateMBps` 默认 0，阶段 B 合入后不改变任何现网行为，须显式配置才生效——分阶段安全。
- **限速使 stop 变慢**：属预期权衡，B4 Step 3 要求记录实测 stop 耗时确认可接受。

**与原 spec 的弱化：**
- 不优化「本地存储 + 跨设备 /tmp」场景的写入量（仅同盘 rename 受益）——对象存储路径（130 MinIO，实际生产路径）完全受益。
- 不动 trailer 队列单线程模型、不动旧 plan 的 [预留] 信号量字段——超出本 plan 范围。
- 未做方案 2（录制时预留 moov 空间，可消除重写本身）——那是更彻底但需改 muxer 的后续修复，本 plan 不含。

---

## 代码评审修复记录 (2026-05-16)

执行后经代码评审，发现并修复 1 个 Critical + 1 个关联缺陷（均由「快路径下 trailer
临时文件与 storage.File 内部临时文件成为同一文件」引入）：

1. **Critical：对象存储 `Close()` 的 Sync 失败分支删文件 → 丢数据。**
   `s3/oss/cos` 的 `Close()` 在 `tempFile.Sync()` 失败时原为 `cleanup(true)`，
   会 `os.Remove(filePath)`。快路径下 `filePath == tempPath`，于是 Sync 失败
   （如 stop burst 期间磁盘满）会删掉重写好的 trailer 文件，导致 record.go 的
   `MoveToPendingDir` 扑空、`SaveFailedUpload` 不执行、录像永久丢失。
   **修复**：Sync 失败分支改 `cleanup(false)`，与上传失败分支一致——失败一律保留文件。
   原 plan Task A4 Step 3 说明「失败时 cleanup(false) 保留它」仅对上传失败分支成立，
   遗漏了 Sync 失败分支，此处一并更正。

2. **关联：`FinalizeFromTemp` 失败后 record.go 仍 Close → 空句柄 nil panic + 未补传。**
   `FinalizeFromTemp` 失败后 `S3File.tempFile` 为 nil，err-defer 再调 `Close()` 会
   走到 `uploadTempFile()` 对 nil 句柄 `Seek` → panic；且该路径未把完整临时文件入
   pending。**修复**：record.go 阶段 2 失败时置 `t.file = nil`（跳过 err-defer 的
   Close），并经新增的 `recoverToPending` 闭包把 tempPath 移入 pending 目录补传。
   `recoverToPending` 同时被阶段 3 复用，统一「重写完成后任何失败 → 保留临时文件补传」。

3. 次要：回退路径 `io.Copy` 不经限速器（已在代码注释标明，当前为死路径）；
   `throttledWriter` 注明「限均值非瞬时峰值」「非并发安全」。

第二轮评审（无 Critical）补充修复：

4. `trailerWriteBytesPerSec` 改为 `atomic.Int64`：消除 `InitUploadManager` 写入与
   `NewTrailerThrottledWriter` 读取之间的形式化数据竞争（实际不并发，但 atomic 让其确定无误）。
5. 补充注释：`LocalFile.FinalizeFromTemp` 失败后 `f.File` 为已关闭句柄（调用方须丢弃）；
   `recoverToPending` 在 `t.db == nil` 时的提前返回意图。

**行为偏差说明**：旧 `Run()` 在阶段 1 出错时（如 `bw.Flush` 失败）会保留**不完整的**
临时文件「供手动恢复」；新 `Run()` 统一让 defer 删除它。这是有意的改进——半截 MP4
无恢复价值，且此时原始录像数据仍在 `t.file` 内、会经 err-defer 的 `Close()` 正常上传。

第二轮评审中标记为「预存、不在本 PR 范围」未处理项（均非本次改动引入）：
- `pkg.ErrSkip` 分支：`io.CopyN` 的读端是 storage.File、写端是 bufio，二者都不产生
  `ErrSkip`，该分支不可达，是 A4 之前就有的防御性死代码。
- `S3File.Close` 中 `OnUploadFailed` 回调的 `fileSize` 恒为 0：`OnUploadFailed` 全仓
  从未注册，整段为死代码。
