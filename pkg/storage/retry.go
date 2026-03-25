package storage

import (
	"context"
	"fmt"
	"log"
	"time"
)

// RetryConfig 上传重试配置
type RetryConfig struct {
	MaxRetries    int
	RetryInterval time.Duration
}

// WithDefaults 返回填充默认值后的副本
func (rc RetryConfig) WithDefaults() RetryConfig {
	if rc.MaxRetries <= 0 {
		rc.MaxRetries = 3
	}
	if rc.RetryInterval <= 0 {
		rc.RetryInterval = 5 * time.Second
	}
	return rc
}

// UploadWithRetry 带指数退避的上传重试。
// ctx 用于支持外部取消（如 shutdown），resetFn 在每次重试前调用（如重置文件指针），可为 nil。
func UploadWithRetry(ctx context.Context, config RetryConfig, storageType string, objectKey string, resetFn func() error, uploadFn func() error) error {
	config = config.WithDefaults()

	var lastErr error
	for attempt := 0; attempt <= config.MaxRetries; attempt++ {
		if attempt > 0 {
			delay := config.RetryInterval * time.Duration(1<<uint(attempt-1)) // 指数退避: 5s, 10s, 20s, ...
			log.Printf("[%s] upload retry %d/%d after %v: %s", storageType, attempt, config.MaxRetries, delay, objectKey)
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return fmt.Errorf("upload cancelled during retry wait: %w", ctx.Err())
			}
			if resetFn != nil {
				if err := resetFn(); err != nil {
					return fmt.Errorf("reset before retry failed: %w", err)
				}
			}
		}

		if err := uploadFn(); err != nil {
			lastErr = err
			log.Printf("[%s] upload attempt %d failed: %s, error: %v", storageType, attempt+1, objectKey, err)
			continue
		}
		return nil
	}
	return fmt.Errorf("upload failed after %d attempts (%d retries): %w", config.MaxRetries+1, config.MaxRetries, lastErr)
}
