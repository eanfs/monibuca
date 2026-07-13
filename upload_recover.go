package m7s

import (
	"errors"
	"log/slog"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

// RecoverFailedUpload 把「上传失败但数据仍在本地」的录像文件移入 pending 暂存目录,
// 并登记补传任务(由定时补传调度拉起)。这是各录制插件(mp4/flv/hls)上传失败后的
// 统一兜底入口:走对象存储的录制文件不应因一次上传失败而静默丢失。
//
// 返回 pending 路径;未做任何移动时返回空串:
//   - db 为 nil(无库部署/测试):没有补传队列可登记,调用方按原逻辑处理临时文件;
//   - localPath 为空:无本地数据可保;
//   - storageType 为 local:文件已在最终路径,移走反而破坏,只记日志。
//
// 移动失败(含 pending 目录满)时返回 error,文件留在原地并已告警。
func RecoverFailedUpload(logger *slog.Logger, db *gorm.DB, localPath, objectKey, storageType, streamPath string,
	fileSize int64, durationMs uint32, metadata map[string]string, cause error) (string, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if db == nil || localPath == "" {
		return "", nil
	}
	if storageType == "local" {
		logger.Error("local record close failed, file kept in place",
			"err", cause, "localPath", localPath, "objectKey", objectKey)
		return "", nil
	}
	pendingPath, moveErr := storage.MoveToPendingDir(localPath)
	if moveErr != nil {
		logger.Error("move to pending dir failed", "err", moveErr,
			"localPath", localPath, "objectKey", objectKey)
		if errors.Is(moveErr, storage.ErrPendingDirFull) {
			RaiseUploadAlarm(db, config.AlarmDiskSpaceFull, "pending dir full", streamPath, objectKey,
				"pending 暂存目录已满,本录像无法暂存补传可能丢失: "+moveErr.Error())
		}
		return "", moveErr
	}
	SaveFailedUpload(db, pendingPath, objectKey, storageType, streamPath, fileSize, durationMs, metadata, cause)
	logger.Info("saved failed upload for retry", "pendingPath", pendingPath, "objectKey", objectKey)
	return pendingPath, nil
}
