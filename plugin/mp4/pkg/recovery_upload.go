package mp4

import (
	"log/slog"

	task "github.com/eanfs/gotask"
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

// recoveryUploadTask 在独立上传队列中执行恢复产物的最终持久化。
//
// Task 6 将在这里实现存储解析（GetStorageForType(header.StorageType)）与
// pending 登记语义。Task 5 只负责把文件成功移交给队列；在此之前恢复任务
// 不得删除源暂存文件。
type recoveryUploadTask struct {
	task.Task
	upload RecoveryUpload
}

func (u *recoveryUploadTask) Go() error {
	// Task 6 的落点：解析目标存储、复用 finalizeUploadTask 的上传/入库路径。
	// 当前仅确认移交，便于恢复任务在端到端测试中观察与幂等重放。
	u.Info("recovery output handed to upload queue",
		"file", u.upload.OutputPath,
		"source", u.upload.MediaPath,
		"streamPath", u.upload.Header.StreamPath,
		"storageType", u.upload.Header.StorageType)
	return nil
}

// EnqueueRecoveryUpload 把恢复产物追加到现有上传队列 uploadQueueTask。
// 返回 nil 表示已成功入队（异步执行）；调用方此后才可清理源暂存文件与 sidecar。
func EnqueueRecoveryUpload(upload RecoveryUpload) error {
	uploadQueueTask.AddTask(&recoveryUploadTask{upload: upload}, slog.Default())
	return nil
}
