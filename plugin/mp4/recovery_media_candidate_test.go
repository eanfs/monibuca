package plugin_mp4

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
)

// writeRecoverySidecarWithRename 重写 sidecar：样本记录 + commit + 最后一条 RENAME。
// RENAME 排在 commit 之后与生产端一致（appendIndexRename 总是追加）。
func writeRecoverySidecarWithRename(t *testing.T, sidecar string, header mp4pkg.IndexHeader,
	records []mp4pkg.IndexRecord, renamePath string) {
	t.Helper()
	w, err := mp4pkg.CreateIndex(sidecar, header)
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	for _, record := range records {
		if err = w.Append(record); err != nil {
			t.Fatalf("append index record: %v", err)
		}
	}
	if err = w.Commit(80, uint64(len(records))); err != nil {
		t.Fatalf("commit index: %v", err)
	}
	if err = w.Rename(renamePath); err != nil {
		t.Fatalf("append rename record: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}
}

// Important 2（回归）：trailer 在 FinalizeFromTemp 之前预写的 RENAME 指向
// os.CreateTemp("", "*.mp4")——对象存储配置了非默认 tempdir 时该路径在恢复
// 白名单之外。最后一条 RENAME 越界不代表媒体不可恢复：sidecar 派生的暂存文件
// 仍在白名单内。候选定位必须按白名单逐个筛选、越界就落到下一个候选，而不是
// 取「第一个存在的路径」再整体拒绝（那样这整个窗口永久不可恢复）。
func TestRecoveryFallsBackToDerivedMediaWhenRenameOutsideWhitelist(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	mediaPath, sidecar, records := buildRecoveryFixture(t, dir, "s3writer_prewrite.tmp", payload)

	// 预写的 RENAME 指向白名单外的 trailer 临时文件（真实存在，模拟窗口内）。
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "trailer.mp4")
	if err := os.WriteFile(outside, []byte("trailer-temp"), 0o644); err != nil {
		t.Fatalf("write outside artifact: %v", err)
	}
	idx, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	writeRecoverySidecarWithRename(t, sidecar, idx.Header, records, outside)
	ageRecoveryFiles(t, mediaPath, sidecar)

	var items []RecoveryItem
	recovery := &IndexRecoveryTask{EnqueueUpload: func(item RecoveryItem) error {
		items = append(items, item)
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Recovered != 1 || stats.Failed != 0 {
		t.Fatalf("stats = %+v, want recovered=1 (derived candidate must be used)", stats)
	}
	if len(items) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(items))
	}
	output := filepath.Join(dir, "s3writer_prewrite.mp4")
	if items[0].OutputPath != canonPath(t, output) {
		t.Fatalf("OutputPath = %q, want %q (derived from the located media, not the out-of-whitelist rename)",
			items[0].OutputPath, canonPath(t, output))
	}
	assertRecoveredPlayable(t, output, 2)
	assertRecoveredPayload(t, output, payload)

	// 白名单外的文件绝不能被读写：内容原样保留。
	if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "trailer-temp" {
		t.Fatalf("out-of-whitelist candidate was touched: %q, %v", got, readErr)
	}
}

// Important 2（回归，本地存储）：本地录像目录不是 /tmp 时，预写的 RENAME 同样
// 落在白名单外。原媒体已有完整 moov（C1 场景：产物已可播），恢复必须走派生候选
// 直接入队，而不是因为越界候选整体拒绝。行为与「RENAME 未预写」时完全一致。
func TestRecoveryUsesDerivedMediaForLocalRecordingWhenRenameOutsideWhitelist(t *testing.T) {
	dir := t.TempDir()
	mediaPath := filepath.Join(dir, "rec.mp4")
	want := writeCompletedLocalMP4(t, mediaPath, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	sidecar := mediaPath + mp4pkg.IndexExtension
	writeRecoverySidecarWithRename(t, sidecar, mp4pkg.IndexHeader{
		Version:     1,
		StreamPath:  "live/test",
		MediaPath:   mediaPath,
		FilePath:    mediaPath,
		TargetName:  "rec.mp4",
		StorageType: "local",
	}, nil, filepath.Join(t.TempDir(), "trailer.mp4"))
	ageRecoveryFiles(t, mediaPath, sidecar)

	var items []RecoveryItem
	recovery := &IndexRecoveryTask{EnqueueUpload: func(item RecoveryItem) error {
		items = append(items, item)
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if stats.Skipped != 1 || stats.Failed != 0 || stats.Recovered != 0 {
		t.Fatalf("stats = %+v, want skipped=1 (already complete, C1)", stats)
	}
	if len(items) != 1 {
		t.Fatalf("enqueued = %d, want 1", len(items))
	}
	if items[0].MediaPath != canonPath(t, mediaPath) || items[0].OutputPath != canonPath(t, mediaPath) {
		t.Fatalf("enqueued media = %q output = %q, want the derived original %q",
			items[0].MediaPath, items[0].OutputPath, canonPath(t, mediaPath))
	}
	if got, readErr := os.ReadFile(mediaPath); readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("complete media must be left untouched: %v", readErr)
	}
}

// Important 2（信任边界）：所有存在的候选都在白名单外（recorded 越界、派生候选
// 是指向白名单外的符号链接）时必须继续拒绝恢复——逐个筛选只改变「要不要落到
// 下一个候选」，不改变「越界候选永不使用」。
func TestRecoveryRefusesMediaWhenAllCandidatesOutsideWhitelist(t *testing.T) {
	dir := t.TempDir()
	payload := []byte{0x0A, 0x0B, 0x0C, 0x0D}
	mediaPath, sidecar, records := buildRecoveryFixture(t, dir, "s3writer_denied.tmp", payload)

	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "trailer.mp4")
	if err := os.WriteFile(outside, []byte("outside-media"), 0o644); err != nil {
		t.Fatalf("write outside artifact: %v", err)
	}
	idx, err := mp4pkg.ReadIndex(sidecar)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	// 派生候选（sidecar 同目录）改成指向白名单外的符号链接：存在但不可用。
	if err = os.Remove(mediaPath); err != nil {
		t.Fatalf("remove media: %v", err)
	}
	if err = os.Symlink(outside, mediaPath); err != nil {
		t.Fatalf("symlink media: %v", err)
	}
	writeRecoverySidecarWithRename(t, sidecar, idx.Header, records, outside)
	ageRecoveryFiles(t, sidecar)

	enqueued := 0
	recovery := &IndexRecoveryTask{EnqueueUpload: func(RecoveryItem) error {
		enqueued++
		return nil
	}}
	stats, err := recovery.RunOnce([]string{dir})
	if err == nil {
		t.Fatal("RunOnce must report the refused recovery")
	}
	if stats.Recovered != 0 || stats.Failed != 1 {
		t.Fatalf("stats = %+v, want failed=1", stats)
	}
	if len(stats.Errors) != 1 || !errors.Is(stats.Errors[0], errRecoveryPathDenied) {
		t.Fatalf("stats.Errors = %v, want errRecoveryPathDenied", stats.Errors)
	}
	if enqueued != 0 {
		t.Fatalf("out-of-whitelist media must not be enqueued, got %d", enqueued)
	}
	if _, statErr := os.Stat(sidecar); statErr != nil {
		t.Fatalf("sidecar must be kept for manual handling: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "s3writer_denied.mp4")); !os.IsNotExist(statErr) {
		t.Fatal("refused recovery must not produce an output mp4")
	}
	if got, readErr := os.ReadFile(outside); readErr != nil || string(got) != "outside-media" {
		t.Fatalf("out-of-whitelist candidate was touched: %q, %v", got, readErr)
	}
}
