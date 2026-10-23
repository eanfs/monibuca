package mp4

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// NewRecoveryUpload 是恢复产物的唯一入口校验：错误意味着调用方必须保留源
// 媒体与 sidecar。空 storageType 绝不能被猜成 local（会删掉未上传的对象存储
// 录像）；相对路径则依赖进程工作目录，恢复任务与上传队列的目录语义可能不同。
func TestNewRecoveryUploadValidatesContract(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")

	cases := []struct {
		name    string
		mutate  func(*RecoveryUpload)
		wantSub string
	}{
		{
			name:    "empty storage type",
			mutate:  func(u *RecoveryUpload) { u.Header.StorageType = "   " },
			wantSub: "storageType",
		},
		{
			name:    "empty output path",
			mutate:  func(u *RecoveryUpload) { u.OutputPath = "" },
			wantSub: "output path",
		},
		{
			name:    "relative output path",
			mutate:  func(u *RecoveryUpload) { u.OutputPath = "relative/recovered.mp4" },
			wantSub: "output path",
		},
		{
			name:    "empty sidecar path",
			mutate:  func(u *RecoveryUpload) { u.SidecarPath = "" },
			wantSub: "sidecar path",
		},
		{
			name:    "relative sidecar path",
			mutate:  func(u *RecoveryUpload) { u.SidecarPath = "relative/recovered.mp4" + IndexExtension },
			wantSub: "sidecar path",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			upload := fixture.upload()
			c.mutate(&upload)
			if _, err := NewRecoveryUpload(upload); err == nil {
				t.Fatalf("NewRecoveryUpload accepted %+v", upload)
			} else if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("error = %v, want it to mention %q", err, c.wantSub)
			}
		})
	}

	// 合法输入必须通过，且 storageType 的空白被规范化，避免下游 GetKey 比较失败。
	upload := fixture.upload()
	upload.Header.StorageType = "  s3  "
	got, err := NewRecoveryUpload(upload)
	if err != nil {
		t.Fatalf("NewRecoveryUpload(valid): %v", err)
	}
	if got.Header.StorageType != "s3" {
		t.Errorf("storageType = %q, want it trimmed to %q", got.Header.StorageType, "s3")
	}
}

// 所有入队入口都必须先过 NewRecoveryUpload：无效移交不得触发后端解析/上传，
// 也不得清理任何文件。
func TestEnqueueRecoveryUploadValidatesContractBeforeResolver(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "s3")
	bad := fixture.upload()
	bad.OutputPath = "relative/recovered.mp4"

	resolved := 0
	queued := 0
	err := EnqueueRecoveryUploadWithOptions(bad, RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) {
			resolved++
			return &fakeRecoveryStorage{key: "s3"}, nil
		},
		enqueue: func(*finalizeUploadTask, *slog.Logger) error {
			queued++
			return nil
		},
	})
	if err == nil {
		t.Fatal("relative OutputPath must be rejected")
	}
	if resolved != 0 {
		t.Fatalf("resolver called %d times for an invalid upload, want 0", resolved)
	}
	if queued != 0 {
		t.Fatalf("queued tasks = %d, want 0", queued)
	}
	fixture.assertRetained(t)
}

// local 分支没有上传动作，但产物必须真实存在：sidecar 描述的 OutputPath 若已被
// 外部删除/移动，回写 record_streams 会留下指向不存在文件的记录，且删掉
// sidecar 就永久失去这条恢复线索。os.Stat 作为纵深防御必须拦在这里。
func TestEnqueueRecoveryUploadLocalMissingOutputKeepsSidecar(t *testing.T) {
	fixture := newRecoveryUploadFixture(t, "local")
	if err := os.Remove(fixture.output); err != nil {
		t.Fatalf("remove output: %v", err)
	}

	err := EnqueueRecoveryUploadWithOptions(fixture.upload(), RecoveryUploadOptions{
		Resolver: func(string) (storage.Storage, error) {
			return &fakeRecoveryStorage{key: "local"}, nil
		},
	})
	if err == nil {
		t.Fatal("missing local OutputPath must return an error")
	}
	if !strings.Contains(err.Error(), "stat") {
		t.Errorf("error = %v, want it to mention the failed stat", err)
	}
	if _, statErr := os.Stat(filepath.Clean(fixture.sidecar)); statErr != nil {
		t.Fatalf("sidecar must be retained when the local output is missing: %v", statErr)
	}
}
