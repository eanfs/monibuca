package m7s

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"gorm.io/gorm"
	"m7s.live/v5/pkg/config"
	"m7s.live/v5/pkg/storage"
)

// RecoverFailedUpload 把「上传失败但数据仍在本地」的录像文件登记进补传队列
// (由定时补传调度拉起)。这是各录制插件(mp4/flv/hls)上传失败后的统一兜底入口:
// 走对象存储的录制文件不应因一次上传失败而静默丢失。
//
// 登记位置的优先级：
//  1. 移入 pending 暂存目录后登记（正常路径）；
//  2. pending 目录不可用（已满/跨盘 copy 失败）时，**原地**按 localPath 登记——
//     补传读取的是本地文件，并不依赖 pending 目录，移不走不该等于放弃；
//  3. 两种情况都登记失败时把媒体退回原路径（见下）。
//
// 返回 (registeredPath, err):
//   - registeredPath 非空表示登记已确认，媒体就在该路径（pending 目录，或
//     原地登记时的原路径）；与 localPath 相同即表示媒体**没有**离开原路径。
//   - err 非空表示**登记未确认**，调用方必须保留 sidecar 且不得删除媒体：
//     媒体已退回原路径（回滚成功时），或仍留在 localPath（移动本身失败）。
//     回滚失败时媒体留在 pending 目录，Error 日志里带 pendingPath。
//
// 未做任何登记、也没有需要保留的媒体时返回 ("", nil):
//   - db 为 nil(无库部署/测试):没有补传队列可登记,调用方按原逻辑处理临时文件;
//   - localPath 为空:无本地数据可保;
//   - storageType 为 local:文件已在最终路径,移走反而破坏,只记日志。
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
			// 告警文案不能写「可能丢失」：下面会把媒体原地登记，补传仍会执行。
			RaiseUploadAlarm(db, config.AlarmDiskSpaceFull, "pending dir full", streamPath, objectKey,
				"pending 暂存目录已满,本录像改为按原路径登记补传(需尽快清理暂存目录): "+moveErr.Error())
		}
		if regErr := registerFailedUpload(logger, db, localPath, objectKey, storageType, streamPath,
			fileSize, durationMs, metadata, cause); regErr != nil {
			logger.Error("failed upload left in place without retry task",
				"error", regErr, "cause", cause, "localPath", localPath, "objectKey", objectKey)
			return "", fmt.Errorf("move to pending dir and register in place both failed: %w",
				errors.Join(moveErr, regErr))
		}
		logger.Warn("failed upload registered in place: pending dir unavailable",
			"cause", cause, "localPath", localPath, "objectKey", objectKey, "streamPath", streamPath)
		return localPath, nil
	}
	if regErr := registerFailedUpload(logger, db, pendingPath, objectKey, storageType, streamPath,
		fileSize, durationMs, metadata, cause); regErr != nil {
		// 登记失败必须把媒体送回原路径：
		//   - 留在 pending 的文件既没有 DB 行、补传调度找不到，又会永久占用
		//     pending 容量（水位检查只看文件数/字节数，不看是否已登记）；
		//   - 退回原路径后媒体紧邻 sidecar，下次恢复扫描能重新找到并登记。
		if restoreErr := restoreMediaFromPending(pendingPath, localPath); restoreErr != nil {
			logger.Error("restore media from pending after failed registration failed",
				"error", restoreErr, "pendingPath", pendingPath, "localPath", localPath, "objectKey", objectKey)
		} else {
			logger.Error("failed upload registration failed; media restored to original path",
				"error", regErr, "cause", cause, "localPath", localPath, "objectKey", objectKey, "streamPath", streamPath)
		}
		return "", regErr
	}
	logger.Info("saved failed upload for retry", "pendingPath", pendingPath, "objectKey", objectKey)
	return pendingPath, nil
}

// registerFailedUpload 写入 upload_tasks 并回查确认。返回 nil 表示登记确认。
//
// 回查是硬性要求：INSERT 的返回值历史上被丢弃过，查询失败同样按「未确认」
// 处理，绝不因为「大概写进去了」就允许调用方删除 sidecar 与媒体。
func registerFailedUpload(logger *slog.Logger, db *gorm.DB, localPath, objectKey, storageType, streamPath string,
	fileSize int64, durationMs uint32, metadata map[string]string, cause error) error {
	if err := SaveFailedUpload(db, localPath, objectKey, storageType, streamPath,
		fileSize, durationMs, metadata, cause); err != nil {
		logger.Error("failed upload registration failed; media kept for retry",
			"error", err, "cause", cause, "localPath", localPath, "objectKey", objectKey, "streamPath", streamPath)
		return fmt.Errorf("register failed upload %s: %w", localPath, err)
	}
	confirmed, confirmErr := ConfirmPendingRegistration(db, localPath, objectKey)
	if confirmErr != nil {
		logger.Error("failed upload registration unconfirmed: lookup failed; media kept for retry",
			"error", confirmErr, "cause", cause, "localPath", localPath,
			"objectKey", objectKey, "streamPath", streamPath)
		return fmt.Errorf("confirm failed upload registration %s: %w", localPath, confirmErr)
	}
	if !confirmed {
		logger.Error("failed upload registration unconfirmed: upload_tasks row not found; media kept for retry",
			"cause", cause, "localPath", localPath, "objectKey", objectKey, "streamPath", streamPath)
		return fmt.Errorf("failed upload registration %s not confirmed", localPath)
	}
	return nil
}

// restoreMediaFromPending 把已移入 pending 的媒体退回原路径。
//
// 绝不覆盖原路径上已存在的文件：固定 fileName + 分片时该路径可能已被新录像
// 占用，覆盖会毁掉正在写的数据。此时返回错误，调用方保留 pending 副本并告警。
func restoreMediaFromPending(pendingPath, originalPath string) error {
	if pendingPath == "" || pendingPath == originalPath {
		return nil
	}
	if _, err := os.Stat(originalPath); err == nil {
		return fmt.Errorf("original path %s is occupied; refusing to overwrite", originalPath)
	} else if !os.IsNotExist(err) {
		return err
	}
	return storage.MoveLocalFile(pendingPath, originalPath)
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
