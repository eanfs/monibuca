package mp4

import (
	"errors"
	"log/slog"
)

// RecoveryUpload 是恢复产物移交给上传队列的契约。
//
// Header 来自 sidecar，保留 streamPath/filePath/storageType 等元数据；
// MediaPath 是恢复所用的源媒体（本地 .mp4 或对象存储暂存 .tmp）；
// OutputPath 是已原子 rename、可直接上传的 moov-first MP4。
type RecoveryUpload struct {
	Header     IndexHeader
	Records    []IndexRecord
	MediaPath  string
	OutputPath string
}

// ErrRecoveryUploadUnavailable 表示恢复上传尚未实现（Task 6）。
//
// 恢复任务把「已入队」当作删除源暂存文件的前提。Task 6 落地前无法确认
// 产物真的会被上传，因此占位实现必须返回可判定的错误，让调用方保留源文件
// 与 .recovering 标记，下次 RunOnce 继续重试；绝不能静默返回 nil，
// 否则唯一的源文件会被删除而产物从未上传。
var ErrRecoveryUploadUnavailable = errors.New("mp4: recovery upload not implemented yet")

// EnqueueRecoveryUpload 是 Task 6 完成前的占位移交实现。
//
// Task 6 需在此实现：按 Header.StorageType 解析目标存储（GetStorageForType）、
// 复用 finalizeUploadTask 的上传/入库路径、登记 pending 语义，并在成功入队
// （或上传成功）后返回 nil。返回 nil 之前调用方不会删除源暂存文件。
func EnqueueRecoveryUpload(upload RecoveryUpload) error {
	slog.Default().Warn("recovery upload unavailable; keeping recovered source for retry",
		"file", upload.OutputPath,
		"source", upload.MediaPath,
		"streamPath", upload.Header.StreamPath,
		"storageType", upload.Header.StorageType)
	return ErrRecoveryUploadUnavailable
}
