//go:build s3

package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestS3File_CloseSyncFailurePreservesFile 回归测试（代码评审 Critical #1）:
// 经 FinalizeFromTemp 接管后, Close() 若在 Sync 阶段失败, 必须保留文件——
// 此时 filePath 即调用方写好的 trailer 临时文件, 删除它会导致上层 record.go
// 的 MoveToPendingDir 补传扑空, 录像永久丢失。
func TestS3File_CloseSyncFailurePreservesFile(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "rewritten.mp4")
	if err := os.WriteFile(srcPath, []byte("MOOV-FIRST-MP4"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	w := &S3File{}
	if err := w.FinalizeFromTemp(srcPath); err != nil {
		t.Fatalf("FinalizeFromTemp: %v", err)
	}
	// 关闭内部句柄, 使后续 Close() 的 tempFile.Sync() 必定失败
	w.tempFile.Close()

	if err := w.Close(); err == nil {
		t.Fatal("expected Close to fail on Sync of a closed handle")
	}
	// 关键断言: 文件必须仍存在, 否则上层 MoveToPendingDir 补传会丢数据
	if _, statErr := os.Stat(srcPath); os.IsNotExist(statErr) {
		t.Fatal("regression: trailer temp file deleted on Sync failure -> data loss")
	}
}
