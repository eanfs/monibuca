package storage

import "fmt"

// IOWarnings returns warnings for untuned IO settings (English, for log
// searches). It only inspects its argument, never global state, so it is easy
// to unit test.
func IOWarnings(cfg UploadConfig) []string {
	var out []string
	if cfg.UploadReadRateMBps <= 0 {
		out = append(out, fmt.Sprintf("upload.readratembps=%d is not a positive rate (unlimited): concurrent uploads can saturate disk reads and starve recording writes", cfg.UploadReadRateMBps))
	}
	if cfg.TrailerWriteRateMBps <= 0 {
		out = append(out, fmt.Sprintf("upload.trailerwriteratembps=%d is not a positive rate (unlimited): trailer rewrites can burst the disk", cfg.TrailerWriteRateMBps))
	}
	if cfg.MaxQueuedUploads <= 0 {
		out = append(out, fmt.Sprintf("upload.maxqueueduploads=%d is not a positive limit (unbounded): a finalize storm can pile up hundreds of in-memory tasks", cfg.MaxQueuedUploads))
	}
	if cfg.MaxConcurrentUploads > 4 {
		out = append(out, fmt.Sprintf("upload.maxconcurrentuploads=%d exceeds the recommended maximum 4", cfg.MaxConcurrentUploads))
	}
	return out
}
