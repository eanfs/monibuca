package plugin_mp4

import (
	"encoding/json"
	"net/http"
	"strconv"

	m7s "m7s.live/v5"
)

// 录像上传补传的运维 HTTP 端点，经 MP4Plugin.RegisterHandler 注册（见 index.go），
// 实际路径带 /mp4 前缀：
//
//	GET  /mp4/api/upload/exhausted     列出补传次数耗尽、已被系统放弃的任务
//	POST /mp4/api/upload/retry?id=N    重置指定任务，重新拉入补传循环
//
// 用于存储故障修复后，运维查看并一键重新拉起被放弃的录像上传。

func (p *MP4Plugin) handleListExhaustedUploads(w http.ResponseWriter, r *http.Request) {
	if p.DB == nil {
		http.Error(w, "database not enabled", http.StatusServiceUnavailable)
		return
	}
	tasks, err := m7s.QueryExhaustedUploads(p.DB, 200)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"code":  0,
		"count": len(tasks),
		"data":  tasks,
	})
}

func (p *MP4Plugin) handleRetryUpload(w http.ResponseWriter, r *http.Request) {
	if p.DB == nil {
		http.Error(w, "database not enabled", http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid or missing query param: id", http.StatusBadRequest)
		return
	}
	if err := m7s.ResetUploadForRetry(p.DB, uint(id)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"code":    0,
		"message": "upload task reset for retry",
	})
}
