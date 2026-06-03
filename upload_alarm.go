package m7s

import (
	"time"

	"gorm.io/gorm"
)

// uploadAlarmDedupWindow：同 alarmType + 同 filePath 的告警去重窗口。
// 补传调度器 / pending 巡检按周期运行，无去重会反复写同一条告警刷屏。
const uploadAlarmDedupWindow = 10 * time.Minute

// RaiseUploadAlarm 写一条上传相关告警到 alarm_info 表，带去重：
// 去重窗口内已存在同 alarmType + 同 filePath 的告警则跳过本次写入。
// best-effort：db 为 nil 或写入失败均静默返回，不阻塞主流程。
func RaiseUploadAlarm(db *gorm.DB, alarmType int, alarmName, streamPath, filePath, desc string) {
	if db == nil {
		return
	}
	var cnt int64
	if err := db.Model(&AlarmInfo{}).
		Where("alarm_type = ? AND file_path = ? AND created_at >= ?",
			alarmType, filePath, time.Now().Add(-uploadAlarmDedupWindow)).
		Count(&cnt).Error; err == nil && cnt > 0 {
		return
	}
	if len(desc) > 500 {
		desc = desc[:500]
	}
	if len(alarmName) > 255 {
		alarmName = alarmName[:255]
	}
	db.Create(&AlarmInfo{
		StreamPath: streamPath,
		AlarmName:  alarmName,
		AlarmDesc:  desc,
		AlarmType:  alarmType,
		FilePath:   filePath,
	})
}
