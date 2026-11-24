package storage

import (
	"testing"
	"time"
)

// Issue #39：延迟入库的 DB 写超时必须是可配置的，且默认从旧的 10s 提高到
// 30s —— 磁盘高负载时同批 INSERT 实测 3.3–14.0s，10s 会让已上传成功的录像
// 永久缺失 record_streams 索引。未配置/配置为 0 时回落到默认值。
func TestRecordDBWriteTimeoutDefaultAndOverride(t *testing.T) {
	t.Cleanup(func() { recordDBWriteTimeout = DefaultRecordDBWriteTimeout })

	if DefaultRecordDBWriteTimeout != 30*time.Second {
		t.Fatalf("default record db write timeout = %v, want 30s", DefaultRecordDBWriteTimeout)
	}

	InitUploadManager(UploadConfig{PendingDir: t.TempDir()})
	if got := RecordDBWriteTimeout(); got != DefaultRecordDBWriteTimeout {
		t.Fatalf("unconfigured timeout = %v, want default %v", got, DefaultRecordDBWriteTimeout)
	}

	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), RecordDBWriteTimeout: 45 * time.Second})
	if got := RecordDBWriteTimeout(); got != 45*time.Second {
		t.Fatalf("configured timeout = %v, want 45s", got)
	}

	// 显式配置 0 表示「沿用默认」，不能被当成无限等待。
	InitUploadManager(UploadConfig{PendingDir: t.TempDir(), RecordDBWriteTimeout: 0})
	if got := RecordDBWriteTimeout(); got != DefaultRecordDBWriteTimeout {
		t.Fatalf("zero timeout = %v, want default %v", got, DefaultRecordDBWriteTimeout)
	}
}
