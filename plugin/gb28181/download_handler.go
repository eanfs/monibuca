package plugin_gb28181pro

import (
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"strconv"

	"m7s.live/v5/pkg/storage"
	gb28181 "m7s.live/v5/plugin/gb28181/pkg"
)

// handleDownloadFile 处理文件下载请求
// URL: /gb28181/download?downloadId=xxx
func (gb *GB28181Plugin) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	// 获取 downloadId 参数
	downloadId := r.URL.Query().Get("downloadId")
	if downloadId == "" {
		http.Error(w, "downloadId parameter is required", http.StatusBadRequest)
		return
	}

	// 检查数据库
	if gb.DB == nil {
		http.Error(w, "Database not available", http.StatusInternalServerError)
		gb.Error("数据库未初始化")
		return
	}

	// 从 gb28181_record 表查询文件路径
	var record gb28181.GB28181Record
	if err := gb.DB.Where("download_id = ? AND status = ?", downloadId, "completed").First(&record).Error; err != nil {
		// 检查是否是正在进行的下载任务
		if dialog, exists := gb.downloadDialogs.Get(downloadId); exists {
			http.Error(w, "Download in progress", http.StatusAccepted)
			gb.Info("下载任务进行中",
				"downloadId", downloadId,
				"status", dialog.Status,
				"progress", dialog.Progress)
			return
		}

		http.Error(w, "Download record not found or not completed", http.StatusNotFound)
		gb.Warn("下载记录不存在或未完成",
			"downloadId", downloadId,
			"error", err)
		return
	}

	st := gb.Server.GetStorage()
	if st == nil {
		http.Error(w, storage.ErrStorageNotAvailable.Error(), http.StatusServiceUnavailable)
		return
	}
	gb.serveStoredRecordFile(st, w, r, &record)
}

func (gb *GB28181Plugin) serveStoredRecordFile(st storage.Storage, w http.ResponseWriter, r *http.Request, record *gb28181.GB28181Record) {
	file, err := st.OpenFile(r.Context(), record.FilePath)
	if err != nil {
		http.Error(w, "Failed to open file", http.StatusInternalServerError)
		gb.Warn("open stored record failed", "recordId", record.DownloadId, "objectKey", record.FilePath)
		return
	}
	defer func() {
		if err := file.Close(); err != nil {
			gb.Warn("close stored record failed", "recordId", record.DownloadId, "objectKey", record.FilePath)
		}
	}()

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		status := http.StatusInternalServerError
		message := "Failed to initialize file"
		errorCategory := "initializeFailed"
		if errors.Is(err, storage.ErrFileNotFound) || errors.Is(err, fs.ErrNotExist) {
			status = http.StatusNotFound
			message = "File not found"
			errorCategory = "notFound"
		}
		http.Error(w, message, status)
		gb.Warn("initialize stored record failed",
			"recordId", record.DownloadId,
			"objectKey", record.FilePath,
			"errorCategory", errorCategory)
		return
	}

	fileInfo, err := file.Stat()
	if err != nil {
		http.Error(w, "Failed to stat file", http.StatusInternalServerError)
		gb.Warn("stat stored record failed", "recordId", record.DownloadId, "objectKey", record.FilePath)
		return
	}
	if fileInfo.IsDir() {
		http.Error(w, "Path is a directory", http.StatusBadRequest)
		gb.Warn("stored record is a directory", "recordId", record.DownloadId, "objectKey", record.FilePath)
		return
	}

	filename := filepath.Base(record.FilePath)
	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	w.Header().Set("Content-Length", strconv.FormatInt(fileInfo.Size(), 10))
	w.Header().Set("Accept-Ranges", "bytes")
	http.ServeContent(w, r, filename, fileInfo.ModTime(), file)
	gb.Info("stored record downloaded", "recordId", record.DownloadId, "objectKey", record.FilePath)
}
