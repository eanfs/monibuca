package mp4

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// newRenameTestTask 造一个带已提交 sidecar 的 trailer 任务。
func newRenameTestTask(t *testing.T, indexPath, mediaPath string) *writeTrailerTask {
	t.Helper()
	w, err := CreateIndex(indexPath, IndexHeader{Version: 1, MediaPath: mediaPath})
	if err != nil {
		t.Fatalf("CreateIndex: %v", err)
	}
	if err = w.Append(IndexRecord{TrackID: 1, Size: 4, Offset: 48}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err = w.Commit(20, 1); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err = w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	task := &writeTrailerTask{indexPath: indexPath}
	task.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	return task
}

// Important 2：RENAME 必须在 FinalizeFromTemp **之前**写好。finalize 会删掉
// 旧的暂存文件，若此时被 kill，sidecar 的 MediaPath 就指向一个已删除的路径，
// 真正的产物（trailer 临时文件）不在恢复扫描的候选里。预写后至少有一条
// 指向真实产物的线索。
func TestIndexRenamePrewrittenBeforeFinalize(t *testing.T) {
	dir := t.TempDir()
	oldMedia := filepath.Join(dir, "s3writer_old.tmp")
	artifact := filepath.Join(dir, "trailer.mp4")
	for _, p := range []string{oldMedia, artifact} {
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	task := newRenameTestTask(t, oldMedia+IndexExtension, oldMedia)

	// 模拟 Run 在 finalize 之前预写 RENAME，然后被杀（finalize 从未执行）。
	if err := task.recordIndexMediaPath(artifact); err != nil {
		t.Fatalf("recordIndexMediaPath: %v", err)
	}

	got, err := ReadIndex(oldMedia + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != artifact {
		t.Fatalf("MediaPath = %q, want pre-recorded artifact %q", got.MediaPath, artifact)
	}
	if _, err = os.Stat(got.MediaPath); err != nil {
		t.Fatalf("recorded media path must exist after a crash: %v", err)
	}
}

// 本地存储：FinalizeFromTemp 把临时文件 rename 到目标路径，预写的 RENAME
// 指向临时文件已失效，收尾必须把它校正回目标路径。
func TestIndexRenameCorrectedForLocalFinalize(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rec.mp4")
	artifact := filepath.Join(dir, "trailer.mp4")
	if err := os.WriteFile(dest, []byte("old"), 0o644); err != nil {
		t.Fatalf("write dest: %v", err)
	}
	if err := os.WriteFile(artifact, []byte("new"), 0o644); err != nil {
		t.Fatalf("write artifact: %v", err)
	}
	fd, err := os.OpenFile(dest, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	file := &storage.LocalFile{File: fd}
	t.Cleanup(func() { file.Close() })

	task := newRenameTestTask(t, dest+IndexExtension, dest)
	task.file = file

	preRecorded := ""
	if err = task.recordIndexMediaPath(artifact); err != nil {
		t.Fatalf("recordIndexMediaPath: %v", err)
	}
	preRecorded = artifact
	if err = file.FinalizeFromTemp(artifact); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	task.syncIndexMediaPath(preRecorded)

	got, err := ReadIndex(dest + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != dest {
		t.Fatalf("MediaPath = %q, want %q after local finalize", got.MediaPath, dest)
	}
	if _, err = os.Stat(got.MediaPath); err != nil {
		t.Fatalf("recorded media path must exist: %v", err)
	}
	if !got.Committed || len(got.Records) != 1 {
		t.Fatalf("rename must not disturb existing records: %+v", got)
	}
}

// 对象存储：finalize 后本地路径就是预写的临时文件，不得再追加一条重复记录。
func TestIndexRenameNotDuplicatedForObjectStorageFinalize(t *testing.T) {
	dir := t.TempDir()
	oldMedia := filepath.Join(dir, "s3writer_old.tmp")
	artifact := filepath.Join(dir, "trailer.mp4")
	for _, p := range []string{oldMedia, artifact} {
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	fd, err := os.Open(artifact)
	if err != nil {
		t.Fatalf("open artifact: %v", err)
	}
	file := &pathChangingFile{fd: fd}
	t.Cleanup(func() { fd.Close() })

	task := newRenameTestTask(t, oldMedia+IndexExtension, oldMedia)
	task.file = file

	if err = task.recordIndexMediaPath(artifact); err != nil {
		t.Fatalf("recordIndexMediaPath: %v", err)
	}
	if err = file.FinalizeFromTemp(artifact); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	sizeBefore := sidecarSize(t, oldMedia+IndexExtension)
	task.syncIndexMediaPath(artifact)
	if sizeAfter := sidecarSize(t, oldMedia+IndexExtension); sizeAfter != sizeBefore {
		t.Fatalf("sidecar grew %d -> %d: pre-recorded path must not be appended twice",
			sizeBefore, sizeAfter)
	}
	got, err := ReadIndex(oldMedia + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != artifact {
		t.Fatalf("MediaPath = %q, want %q", got.MediaPath, artifact)
	}
}

// finalize 失败时回滚 RENAME：指向仍然存在的产物（优先完整的临时文件，
// 它没了才退回旧路径），保证恢复扫描总能找到媒体。
func TestIndexRenameRollbackPicksExistingArtifact(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rec.mp4")
	artifact := filepath.Join(dir, "trailer.mp4")
	for _, p := range []string{dest, artifact} {
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	task := newRenameTestTask(t, dest+IndexExtension, dest)
	// 模拟 runInsertRangeFastPath 这类“尚未预写”的场景：先预写指向产物。
	if err := task.recordIndexMediaPath(artifact); err != nil {
		t.Fatalf("recordIndexMediaPath: %v", err)
	}

	task.rollbackIndexMediaPath(artifact, dest)
	got, err := ReadIndex(dest + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != artifact {
		t.Fatalf("MediaPath = %q, want existing artifact %q", got.MediaPath, artifact)
	}

	// 临时文件已消失（如跨设备拷贝后 reopen 失败）：退回目标路径。
	if err = os.Remove(artifact); err != nil {
		t.Fatalf("remove artifact: %v", err)
	}
	task.rollbackIndexMediaPath(artifact, dest)
	got, err = ReadIndex(dest + IndexExtension)
	if err != nil {
		t.Fatalf("ReadIndex: %v", err)
	}
	if got.MediaPath != dest {
		t.Fatalf("MediaPath = %q, want fallback %q", got.MediaPath, dest)
	}
	if _, err = os.Stat(got.MediaPath); err != nil {
		t.Fatalf("rollback must point at an existing file: %v", err)
	}
}

// sidecar 已被补传登记清理时回滚是 no-op（不得重建出一个指向 pending 的孤儿）。
func TestIndexRenameRollbackSkippedWhenSidecarGone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "rec.mp4")
	artifact := filepath.Join(dir, "trailer.mp4")
	for _, p := range []string{dest, artifact} {
		if err := os.WriteFile(p, []byte("media"), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	task := newRenameTestTask(t, dest+IndexExtension, dest)
	if err := RemoveIndex(dest + IndexExtension); err != nil {
		t.Fatalf("remove sidecar: %v", err)
	}

	task.rollbackIndexMediaPath(artifact, dest)
	if _, err := os.Stat(dest + IndexExtension); !os.IsNotExist(err) {
		t.Fatalf("rollback must not recreate a removed sidecar: %v", err)
	}
}

func sidecarSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat sidecar: %v", err)
	}
	return info.Size()
}
