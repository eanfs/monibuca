package storage

import "fmt"

// IOWarnings returns warnings for untuned IO settings (English, for log
// searches). It only inspects its argument, never global state, so it is easy
// to unit test.
func IOWarnings(cfg UploadConfig) []string {
	var out []string
	if cfg.UploadReadRateMBps <= 0 {
		out = append(out, "upload.readratembps is 0 (unlimited): concurrent uploads can saturate disk reads and starve recording writes")
	}
	if cfg.TrailerWriteRateMBps <= 0 {
		out = append(out, "upload.trailerwriteratembps is 0 (unlimited): trailer rewrites can burst the disk")
	}
	if cfg.MaxQueuedUploads <= 0 {
		out = append(out, "upload.maxqueueduploads is 0 (unbounded): a finalize storm can pile up hundreds of in-memory tasks")
	}
	if cfg.MaxConcurrentUploads > 4 {
		out = append(out, fmt.Sprintf("upload.maxconcurrentuploads=%d exceeds the recommended maximum 4", cfg.MaxConcurrentUploads))
	}
	return out
}
