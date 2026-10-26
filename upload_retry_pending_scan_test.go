package m7s

import (
	"log/slog"
	"path/filepath"
	"testing"

	"m7s.live/v5/pkg/storage"
)

// countLogs 统计给定消息的日志条数。
func (h *retryLogCapture) countLogs(msg string) int {
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

// newPendingScanFailScheduler 造一个 pending 目录不可用的调度器。
//
// 路径里带 NUL 字节：os.Lstat 稳定返回 EINVAL，WalkDir 必然报错且
// os.IsNotExist 为 false —— 与现场「目录不可读 / 挂载点异常」走同一条
// 错误分支，又不依赖权限（root/CI 容器下 chmod 000 无效）。
func newPendingScanFailScheduler(t *testing.T) (*UploadRetryScheduler, *retryLogCapture) {
	t.Helper()
	broken := filepath.Join(t.TempDir(), "pending\x00unreadable")
	// 必须配置上限：未配置时调度器会跳过整轮水位巡检（判定恒为 false），
	// 本测试要验证的是「配置了上限、巡检失败」这条路径。
	storage.InitUploadManager(storage.UploadConfig{PendingDir: broken, PendingMaxSizeMB: 1})

	db := newUploadTestDB(t)
	server := &Server{}
	server.DB = db
	server.activateStorage(&runtimeTestStorage{key: string(storage.StorageTypeLocal)},
		StorageStatus{ActiveType: string(storage.StorageTypeLocal)})

	h := &retryLogCapture{}
	scheduler := &UploadRetryScheduler{s: server}
	scheduler.Logger = slog.New(h)
	return scheduler, h
}

// pending 水位扫描失败过去被 `usageErr == nil` 静默吞掉：目录不可读时告警
// 永久失效且没有任何日志。必须记 Warn，同一个错误只告警一次（避免每 5 分钟
// 刷屏），恢复后重新提示。
func TestUploadRetrySchedulerWarnsOnPendingScanFailure(t *testing.T) {
	scheduler, logs := newPendingScanFailScheduler(t)

	scheduler.Tick(nil)

	entry, ok := logs.find("pending dir usage scan failed")
	if !ok {
		t.Fatalf("pending usage scan failure must be logged; captured=%+v", logs.logs)
	}
	if entry.Level != slog.LevelWarn {
		t.Fatalf("level = %v, want Warn", entry.Level)
	}
	if entry.Attrs["error"] == nil && entry.Attrs["err"] == nil {
		t.Fatalf("log must carry the scan error: %+v", entry.Attrs)
	}

	scheduler.Tick(nil)
	if n := logs.countLogs("pending dir usage scan failed"); n != 1 {
		t.Fatalf("same error logged %d times, want 1 (dedupe)", n)
	}
}
