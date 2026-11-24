package plugin_mp4

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// issue #42 复审：只有 sidecar 自身消失才降级为 skipped。媒体或恢复产物的
// ENOENT 是真实数据丢失信号（媒体被误删、挂载点掉线），必须仍算失败并保留
// sidecar，不能被一刀切的 fs.ErrNotExist 判据吞掉。
func TestRecoveryErrIsGoneOnlyCoversSidecar(t *testing.T) {
	cases := []struct {
		name string
		err  error
		gone bool
	}{
		{name: "media stat ENOENT", err: fmt.Errorf("stat media: %w", fs.ErrNotExist), gone: false},
		{name: "output stat ENOENT", err: fmt.Errorf("stat output: %w", fs.ErrNotExist), gone: false},
		{name: "bare sentinel", err: errRecoverySidecarGone, gone: true},
		{name: "wrapped sentinel", err: fmt.Errorf("read index: %w", errors.Join(fs.ErrNotExist, errRecoverySidecarGone)), gone: true},
		{name: "other error", err: errors.New("permission denied"), gone: false},
		{name: "nil", err: nil, gone: false},
	}
	for _, tc := range cases {
		if got := recoveryErrIsGone(tc.err); got != tc.gone {
			t.Errorf("%s: recoveryErrIsGone = %v, want %v", tc.name, got, tc.gone)
		}
	}
}

// buildCorruptRecoverySidecar 造一个确定性损坏的 sidecar：读取即判定损坏，
// 恢复流程会 markFailed 并计入 stats.Errors。
func buildCorruptRecoverySidecar(t *testing.T, dir, name string) string {
	t.Helper()
	mediaPath := filepath.Join(dir, name)
	if err := os.WriteFile(mediaPath, []byte{0xDE, 0xAD}, 0o644); err != nil {
		t.Fatalf("write media: %v", err)
	}
	sidecar := mediaPath + mp4pkg.IndexExtension
	if err := os.WriteFile(sidecar, []byte("not-an-index"), 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	ageRecoveryFiles(t, mediaPath, sidecar)
	return sidecar
}

// countCaptured 统计给定消息的日志条数。
func countCaptured(h *captureHandler, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, entry := range h.logs {
		if entry.Msg == msg {
			n++
		}
	}
	return n
}

// 启动扫描过去只在完成日志里记 errors=N，明细全部丢失。每个错误必须单独
// 输出（Error 级，带原始错误），否则现场无法定位是哪个 sidecar/目录失败。
func TestRecoveryScanLogsEachError(t *testing.T) {
	dir := t.TempDir()
	buildCorruptRecoverySidecar(t, dir, "s3writer_bad.tmp")

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	if _, err := recovery.RunOnce([]string{dir}); err == nil {
		t.Fatal("RunOnce must report the scan error")
	}

	entry := logs.mustFind(t, "index recovery error", slog.LevelError)
	if entry.Attrs["error"] == nil {
		t.Fatalf("per-error log must carry the error: %+v", entry.Attrs)
	}
	if got := entry.Attrs["total"]; got != int64(1) {
		t.Fatalf("total = %v, want 1", got)
	}
}

// issue #42：扫描目录快照（discoverSidecars）与上传成功后的正常清理
// （pkg.removeIndexAfterUpload）之间存在竞态——快照里还有的 sidecar 到处理时
// 已被删除。ENOENT 不是恢复故障：不得产生 Error 级 "index recovery error"，
// 不计入 errors 总数，只累计到 skipped 并以 Debug 级记录。
func TestRecoveryScanSkipsRemovedSidecar(t *testing.T) {
	dir := t.TempDir()
	// 刻意不创建该文件：模拟扫描取得快照后 sidecar 已被正常清理。
	missing := filepath.Join(dir, "s3writer_gone.tmp"+mp4pkg.IndexExtension)

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	outcome, _, err := recovery.recoverSidecar(missing, -1)
	if err != nil {
		t.Fatalf("ENOENT sidecar must not surface an error: %v", err)
	}
	if outcome != recoverySkipped {
		t.Fatalf("outcome = %v, want skipped", outcome)
	}

	stats := RecoveryStats{Scanned: 1}
	stats.recordOutcome(missing, outcome, err)
	recovery.logRecoveryErrors(stats.Errors, stats.Skipped)

	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", stats.Skipped)
	}
	if stats.Failed != 0 {
		t.Fatalf("failed = %d, want 0: a removed sidecar is not a failure", stats.Failed)
	}
	if len(stats.Errors) != 0 {
		t.Fatalf("errors = %v, want none", stats.Errors)
	}
	if n := countCaptured(logs, "index recovery error"); n != 0 {
		t.Fatalf("logged %d error entries for a removed sidecar, want 0", n)
	}
	entry := logs.mustFind(t, "index recovery skipped: sidecar removed after scan", slog.LevelDebug)
	if got := entry.Attrs["sidecar"]; got != missing {
		t.Fatalf("skipped log sidecar = %v, want %s", got, missing)
	}
}

// 真实错误必须继续以 Error 级输出，且与「已清理」的 ENOENT 分开计数：
// 二者混在同一轮扫描时，failed/errors 只反映真实错误，skipped 只反映已清理。
func TestRecoveryScanKeepsRealErrorsAndSkipsGoneSidecars(t *testing.T) {
	dir := t.TempDir()
	buildCorruptRecoverySidecar(t, dir, "s3writer_bad.tmp")
	missing := filepath.Join(dir, "s3writer_gone.tmp"+mp4pkg.IndexExtension)

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("real index corruption must still be reported as an error")
	}

	outcome, _, goneErr := recovery.recoverSidecar(missing, -1)
	if goneErr != nil {
		t.Fatalf("ENOENT sidecar must not surface an error: %v", goneErr)
	}
	stats.recordOutcome(missing, outcome, goneErr)

	if len(stats.Errors) != 1 {
		t.Fatalf("errors = %v, want exactly the real corruption error", stats.Errors)
	}
	if stats.Failed != 1 {
		t.Fatalf("failed = %d, want 1 (the corrupt sidecar)", stats.Failed)
	}
	if stats.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the removed sidecar)", stats.Skipped)
	}
	if n := countCaptured(logs, "index recovery error"); n != 1 {
		t.Fatalf("logged %d error entries, want 1 (only the real error)", n)
	}
}

// 明细日志必须有上限：侧目录塞满坏 sidecar 时不能把启动日志刷爆。
func TestRecoveryScanErrorLogCapped(t *testing.T) {
	dir := t.TempDir()
	const files = recoveryErrorLogLimit + 5
	for i := 0; i < files; i++ {
		buildCorruptRecoverySidecar(t, dir, fmt.Sprintf("s3writer_bad%02d.tmp", i))
	}

	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	if _, err := recovery.RunOnce([]string{dir}); err == nil {
		t.Fatal("RunOnce must report the scan errors")
	}

	if n := countCaptured(logs, "index recovery error"); n != recoveryErrorLogLimit {
		t.Fatalf("logged %d per-error entries, want %d", n, recoveryErrorLogLimit)
	}
	truncated := logs.mustFind(t, "index recovery errors truncated", slog.LevelError)
	if got := truncated.Attrs["total"]; got != int64(files) {
		t.Fatalf("truncated total = %v, want %d", got, files)
	}
}

// 截断只统计真实错误：已清理 sidecar 的 skipped 不占逐条日志配额，只以
// skipped=<n> 附在截断行上，原有 total 语义不变。
func TestRecoveryErrorTruncationCountsOnlyRealErrors(t *testing.T) {
	logs := &captureHandler{}
	recovery := &IndexRecoveryTask{}
	recovery.Logger = slog.New(logs)

	const realErrors = recoveryErrorLogLimit + 5
	const skipped = 3
	errs := make([]error, 0, realErrors)
	for i := 0; i < realErrors; i++ {
		errs = append(errs, fmt.Errorf("real recovery error %d", i))
	}
	recovery.logRecoveryErrors(errs, skipped)

	if n := countCaptured(logs, "index recovery error"); n != recoveryErrorLogLimit {
		t.Fatalf("logged %d per-error entries, want %d", n, recoveryErrorLogLimit)
	}
	truncated := logs.mustFind(t, "index recovery errors truncated", slog.LevelError)
	if got := truncated.Attrs["total"]; got != int64(realErrors) {
		t.Fatalf("truncated total = %v, want %d (skipped must not be counted)", got, realErrors)
	}
	if got := truncated.Attrs["skipped"]; got != int64(skipped) {
		t.Fatalf("truncated skipped = %v, want %d", got, skipped)
	}
}

// 复审 S1：recordOutcome 的记账兜底只识别 sidecar-gone 哨兵；普通 ENOENT
// （媒体/产物丢失）必须计入失败与错误列表，不能被吞成 skipped。
func TestRecordOutcomeClassifiesSidecarGoneOnly(t *testing.T) {
	gone := RecoveryStats{Scanned: 1}
	gone.recordOutcome("sidecar", recoveryFailed, fmt.Errorf("read index: %w", errRecoverySidecarGone))
	if gone.Skipped != 1 || gone.Failed != 0 || len(gone.Errors) != 0 {
		t.Fatalf("sidecar-gone must count as skipped only: %+v", gone)
	}

	mediaGone := RecoveryStats{Scanned: 1}
	mediaGone.recordOutcome("media", recoveryFailed, fmt.Errorf("stat media: %w", fs.ErrNotExist))
	if mediaGone.Failed != 1 || len(mediaGone.Errors) != 1 || mediaGone.Skipped != 0 {
		t.Fatalf("media ENOENT must stay a failure: %+v", mediaGone)
	}
}
