package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestUploadWithRetry_CancelledParentCtxFails 是当前 bug 复现：
// 父 ctx cancel 后, UploadWithRetry 立刻退. 修复**前后都应保持**通过
// (这是 UploadWithRetry 的契约: 父 ctx cancel 时停止重试).
// 真正的 fix 在 uploadTempFile 层把 ctx 解耦, 不让 Recorder.Context 传给 UploadWithRetry.
func TestUploadWithRetry_CancelledParentCtxFails(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled

	rc := RetryConfig{MaxRetries: 3, RetryInterval: 10 * time.Millisecond}
	var attempts int32
	err := UploadWithRetry(ctx, rc, "test", "obj-key",
		nil,
		func() error {
			atomic.AddInt32(&attempts, 1)
			return errors.New("simulated transient")
		})

	// attempt 0 不走 retry wait, uploadFn 跑 1 次失败后, attempt 1 在 retry wait 处看到 ctx.Done 退出
	if err == nil {
		t.Fatalf("expected error when parent ctx cancelled, got nil")
	}
	a := atomic.LoadInt32(&attempts)
	if a > 1 {
		t.Fatalf("uploadFn should run at most once with cancelled ctx, ran %d times", a)
	}
}

// TestDetachedCtxIgnoresCancel 验证 context.WithoutCancel helper 行为:
// 即使父 ctx 已 cancel, derived ctx 不感知, Done() 不 close, Err() 为 nil.
// 这是 fix 的核心机制 — uploadTempFile 用此 helper 解耦 Recorder.Context.
func TestDetachedCtxIgnoresCancel(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	detached := context.WithoutCancel(parent)

	select {
	case <-detached.Done():
		t.Fatal("detached ctx should not be cancelled by parent")
	default:
		// OK
	}
	if detached.Err() != nil {
		t.Fatalf("detached ctx Err should be nil, got %v", detached.Err())
	}
}

// TestUploadWithRetry_DetachedCtxSucceeds 验证修复路径的关键:
// 用 WithoutCancel 包装一个已 cancel 的父 ctx 后, UploadWithRetry 能完成所有重试.
// 这模拟 uploadTempFile 修复后的行为.
func TestUploadWithRetry_DetachedCtxSucceeds(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel() // 模拟 Recorder.Context 已 cancel

	// fix 后的关键: 上传层用 WithoutCancel 解耦
	uploadCtx := context.WithoutCancel(parent)

	rc := RetryConfig{MaxRetries: 2, RetryInterval: 5 * time.Millisecond}
	var attempts int32
	err := UploadWithRetry(uploadCtx, rc, "test", "obj-key",
		nil,
		func() error {
			n := atomic.AddInt32(&attempts, 1)
			if n < 2 {
				return errors.New("transient, retry me")
			}
			return nil // 第 2 次成功
		})

	if err != nil {
		t.Fatalf("expected success with detached ctx, got: %v", err)
	}
	a := atomic.LoadInt32(&attempts)
	if a != 2 {
		t.Fatalf("expected exactly 2 attempts (1 fail + 1 success), got %d", a)
	}
}
