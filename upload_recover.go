package m7s

import (
	"errors"
	"fmt"
	"log/slog"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

// RecoverFailedUpload 把「上传失败但数据仍在本地」的录像文件移入 pending 暂存目录,
// 并登记补传任务(由定时补传调度拉起)。这是各录制插件(mp4/flv/hls)上传失败后的
// 统一兜底入口:走对象存储的录制文件不应因一次上传失败而静默丢失。
//
// 返回 (pendingPath, err):
//   - pendingPath 非空表示媒体已被移入 pending 暂存目录（文件已不在原路径）；
//   - err 非空表示**登记未确认**，调用方必须保留 sidecar 且不得删除媒体。
//     pendingPath 同时非空时媒体安全地留在 pending 目录里，等下次恢复扫描重试登记；
//     pendingPath 为空时移动本身失败，媒体仍在原路径。
//
// 未做任何移动时返回 ("", nil):
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
	if saveErr := SaveFailedUpload(db, pendingPath, objectKey, storageType, streamPath,
		fileSize, durationMs, metadata, cause); saveErr != nil {
		logger.Error("failed upload registration failed; media kept for retry",
			"error", saveErr, "cause", cause, "localPath", localPath,
			"pendingPath", pendingPath, "objectKey", objectKey, "streamPath", streamPath)
		return pendingPath, fmt.Errorf("register failed upload %s: %w", pendingPath, saveErr)
	}
	// 登记必须回查确认：INSERT 的返回值历史上被丢弃过，这里以库里的实际行
	// 为准。查询失败同样按「未确认」处理，绝不因为「大概写进去了」就允许
	// 调用方删除 sidecar。
	confirmed, confirmErr := ConfirmPendingRegistration(db, pendingPath, objectKey)
	if confirmErr != nil {
		logger.Error("failed upload registration unconfirmed: lookup failed; media kept for retry",
			"error", confirmErr, "cause", cause, "pendingPath", pendingPath,
			"objectKey", objectKey, "streamPath", streamPath)
		return pendingPath, fmt.Errorf("confirm failed upload registration %s: %w", pendingPath, confirmErr)
	}
	if !confirmed {
		logger.Error("failed upload registration unconfirmed: upload_tasks row not found; media kept for retry",
			"cause", cause, "pendingPath", pendingPath, "objectKey", objectKey, "streamPath", streamPath)
		return pendingPath, fmt.Errorf("failed upload registration %s not confirmed", pendingPath)
	}
	logger.Info("saved failed upload for retry", "pendingPath", pendingPath, "objectKey", objectKey)
	return pendingPath, nil
}

// ConfirmPendingRegistration 确认一条失败上传真的落在了 upload_tasks 里。
//
// 判据是 local_path + object_key 同时命中：固定 fileName + 分片时多个产物
// 复用同一对象键，只看 object_key 会把另一份产物当作已登记。objectKey 为空
// 时退化为只按 local_path 匹配。
//
// 返回 (false, err) 表示**结果不可确定**（查询失败），调用方必须按
// 「未登记」处理——保留媒体与 sidecar，不做任何删除，也不要重复登记/上传。
func ConfirmPendingRegistration(db *gorm.DB, localPath, objectKey string) (bool, error) {
	if db == nil || localPath == "" {
		return false, nil
	}
	query := db.Model(&UploadTask{}).Where("local_path = ?", localPath)
	if objectKey != "" {
		query = query.Where("object_key = ?", objectKey)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return false, fmt.Errorf("query upload_tasks for %s: %w", localPath, err)
	}
	return count > 0, nil
}
