package plugin_mp4

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"gorm.io/gorm"
	"m7s.live/v5"
	"m7s.live/v5/pkg/task"
	mp4 "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// RecordRecoveryTask 从录像文件中恢复数据库记录的任务
type RecordRecoveryTask struct {
	task.TickTask
	DB     *gorm.DB
	plugin *MP4Plugin
}

// GetTickInterval 设置任务执行间隔
func (t *RecordRecoveryTask) GetTickInterval() time.Duration {
	return 24 * time.Hour // 默认每天执行一次
}

// Tick 执行任务
func (t *RecordRecoveryTask) Tick(any) {
	t.Info("Starting record recovery task")
	t.recoverRecordsFromFiles()
}

// recoverRecordsFromFiles 从文件系统中恢复录像记录
func (t *RecordRecoveryTask) recoverRecordsFromFiles() {
	// 获取所有录像目录
	var recordDirs []string
	if len(t.plugin.GetCommonConf().OnPub.Record) > 0 {
		for _, conf := range t.plugin.GetCommonConf().OnPub.Record {
			dirPath := filepath.Dir(conf.FilePath)
			recordDirs = append(recordDirs, dirPath)
		}
	}
	if t.plugin.EventRecordFilePath != "" {
		dirPath := filepath.Dir(t.plugin.EventRecordFilePath)
		recordDirs = append(recordDirs, dirPath)
	}

	// 遍历所有录像目录
	for _, dir := range recordDirs {
		t.scanDirectory(dir)
	}
}

// scanDirectory 扫描目录中的MP4文件
func (t *RecordRecoveryTask) scanDirectory(dir string) {
	t.Info("Scanning directory for MP4 files", "directory", dir)

	// 递归遍历目录
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			t.Error("Error accessing path", "path", path, "error", err)
			return nil // 继续遍历
		}

		// 跳过目录
		if info.IsDir() {
			return nil
		}

		// 只处理MP4文件
		if !strings.HasSuffix(strings.ToLower(path), ".mp4") {
			return nil
		}

		// 检查文件是否已经有记录
		var count int64
		t.DB.Model(&m7s.RecordStream{}).Where("file_path = ?", path).Count(&count)
		if count > 0 {
			// 已有记录，跳过
			return nil
		}

		// 解析MP4文件并创建记录
		t.recoverRecordFromFile(path)
		return nil
	})

	if err != nil {
		t.Error("Error walking directory", "directory", dir, "error", err)
	}
}

// recoverRecordFromFile 从MP4文件中恢复记录
func (t *RecordRecoveryTask) recoverRecordFromFile(filePath string) {
	t.Info("Recovering record from file", "file", filePath)

	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		t.Error("Failed to open MP4 file", "file", filePath, "error", err)
		return
	}
	defer file.Close()

	// 创建解析器
	demuxer := mp4.NewDemuxer(file)
	err = demuxer.Demux()
	if err != nil {
		t.Error("Failed to demux MP4 file", "file", filePath, "error", err)
		return
	}

	// 提取文件信息
	fileInfo, err := file.Stat()
	if err != nil {
		t.Error("Failed to get file info", "file", filePath, "error", err)
		return
	}

	// 尝试从MP4文件中提取流路径，如果没有则从文件名和路径推断
	streamPath := extractStreamPathFromMP4(demuxer)
	if streamPath == "" {
		streamPath = inferStreamPathFromFilePath(filePath)
	}

	// 创建记录
	record := m7s.RecordStream{
		FilePath:   filePath,
		StreamPath: streamPath,
		Type:       "mp4",
		Mode:       m7s.RecordModeAuto, // 默认为自动录制模式
		EventLevel: m7s.EventLevelLow,  // 默认为低级别事件
	}

	// 设置开始和结束时间
	record.StartTime = fileInfo.ModTime().Add(-estimateDurationFromFile(demuxer))
	record.EndTime = fileInfo.ModTime()

	// 提取编解码器信息
	for _, track := range demuxer.Tracks {
		forcc := box.GetCodecNameWithCodecId(track.Cid)
		if track.Cid.IsAudio() {
			record.AudioCodec = string(forcc[:])
		} else if track.Cid.IsVideo() {
			record.VideoCodec = string(forcc[:])
		}
	}

	// 保存记录到数据库
	err = t.DB.Create(&record).Error
	if err != nil {
		t.Error("Failed to save record to database", "file", filePath, "error", err)
		return
	}

	t.Info("Successfully recovered record", "file", filePath, "streamPath", streamPath)
}

// extractStreamPathFromMP4 从MP4文件中提取流路径
func extractStreamPathFromMP4(demuxer *mp4.Demuxer) string {
	// 尝试从MP4文件的用户数据中提取流路径
	moov := demuxer.GetMoovBox()
	if moov != nil && moov.UDTA != nil {
		for _, entry := range moov.UDTA.Entries {
			if streamPathBox, ok := entry.(*box.StreamPathBox); ok {
				return streamPathBox.StreamPath
			}
		}
	}
	return ""
}

// inferStreamPathFromFilePath 从文件路径推断流路径
func inferStreamPathFromFilePath(filePath string) string {
	// 从文件路径中提取可能的流路径
	// 这里使用简单的启发式方法，实际应用中可能需要更复杂的逻辑
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)

	// 如果文件名是时间戳，尝试从父目录获取流名称
	if _, err := time.Parse("20060102150405", name); err == nil || isNumeric(name) {
		dir := filepath.Base(filepath.Dir(filePath))
		if dir != "" && dir != "." {
			return dir
		}
	}

	return name
}

// isNumeric 检查字符串是否为数字
func isNumeric(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return len(s) > 0
}

// estimateDurationFromFile 估计文件的持续时间
func estimateDurationFromFile(demuxer *mp4.Demuxer) time.Duration {
	var maxDuration uint32

	for _, track := range demuxer.Tracks {
		if len(track.Samplelist) > 0 {
			lastSample := track.Samplelist[len(track.Samplelist)-1]
			durationMs := lastSample.Timestamp * 1000 / uint32(track.Timescale)
			if durationMs > maxDuration {
				maxDuration = durationMs
			}
		}
	}

	return time.Duration(maxDuration) * time.Millisecond
}
