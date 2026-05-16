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
