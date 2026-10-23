package plugin_mp4

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"m7s.live/v5/pkg/db"
)

// 录像恢复运维 API（经 MP4Plugin.RegisterHandler 注册，实际路径带 /mp4 前缀）：
//
//	GET  /mp4/api/recovery/list        分页列出待恢复/恢复中/已失败索引与最近恢复结果
//	POST /mp4/api/recovery/run         手动触发一轮扫描/恢复，可指定目录或单个索引文件
//	GET  /mp4/api/recovery/status/{id} 查询某次运行的状态与结果
//
// 设计约束：
//   - 运行本身由 task 系统执行，handler 只登记任务并立即返回 id，绝不启动裸 goroutine。
//   - 扫描目录必须落在允许列表内（record.recovery.dirs 或默认目录），
//     符号链接逃逸/路径穿越一律 403。
//   - 鉴权开启（admin.enablelogin）时要求 admin 角色；仓库没有可复用的
//     HTTP admin 中间件，这里沿用 pkg/auth 的 Bearer token 校验 + DB 角色查询。
//   - 未开启登录时 run 仍要求本地回环来源，或在 record.recovery.allowunauthenticated
//     中显式放行：恢复管线会读索引内容并改写文件，不能对任意网络来源开放。
//   - list 的分页默认 50、上限 200；目录遍历有硬上限，避免把超大目录全读入内存。

const (
	recoveryListDefaultPageSize = 50
	recoveryListMaxPageSize     = 200
	// recoveryListScanLimit 是单次 list 扫描的条目硬上限，超出时 truncated=true。
	recoveryListScanLimit = 10000
	// recoveryRunHistoryLimit 保留的最近运行记录数（超出淘汰最旧的已完成运行）。
	recoveryRunHistoryLimit = 100
	// recoveryRecentHistoryLimit 保留的最近恢复结果数。
	recoveryRecentHistoryLimit = 200
	// recoveryRunRequestBodyLimit 限制 run 请求体大小。
	recoveryRunRequestBodyLimit = 64 << 10
)

const (
	recoveryRunRunning   = "running"
	recoveryRunCompleted = "completed"
	recoveryRunFailed    = "failed"
)

// recoveryListEntry 是 list 返回的单个索引文件。
type recoveryListEntry struct {
	Category   string    `json:"category"`
	Path       string    `json:"path"`
	MediaPath  string    `json:"mediaPath"`
	OutputPath string    `json:"outputPath,omitempty"`
	Size       int64     `json:"size"`
	ModTime    time.Time `json:"mtime"`
}

// recoveryRecentEntry 是最近一次成功恢复的结果（索引已清理，只剩产物信息）。
type recoveryRecentEntry struct {
	File      string    `json:"file"`
	At        time.Time `json:"at"`
	Samples   int       `json:"samples,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// recoveryStatsView 是 RecoveryStats 的可 JSON 化视图（error 转字符串）。
type recoveryStatsView struct {
	Scanned   int      `json:"scanned"`
	Recovered int      `json:"recovered"`
	Failed    int      `json:"failed"`
	Skipped   int      `json:"skipped"`
	BareTmp   int      `json:"bareTmp"`
	Processed int      `json:"processed"`
	Errors    []string `json:"errors,omitempty"`
}

func newRecoveryStatsView(stats RecoveryStats) recoveryStatsView {
	view := recoveryStatsView{
		Scanned:   stats.Scanned,
		Recovered: stats.Recovered,
		Failed:    stats.Failed,
		Skipped:   stats.Skipped,
		BareTmp:   stats.BareTmp,
		Processed: stats.Recovered + stats.Failed + stats.Skipped,
	}
	for _, err := range stats.Errors {
		if err != nil {
			view.Errors = append(view.Errors, err.Error())
		}
	}
	return view
}

// ---------------------------------------------------------------- HTTP 视图

type recoveryRunTargetView struct {
	Dirs []string `json:"dirs,omitempty"`
	File string   `json:"file,omitempty"`
}

type recoveryRunView struct {
	Code       int                   `json:"code"`
	ID         string                `json:"id"`
	Status     string                `json:"status"`
	Target     recoveryRunTargetView `json:"target"`
	StartedAt  time.Time             `json:"startedAt"`
	FinishedAt *time.Time            `json:"finishedAt,omitempty"`
	Progress   recoveryStatsView     `json:"progress"`
	Result     *recoveryStatsView    `json:"result,omitempty"`
	Error      string                `json:"error,omitempty"`
}

type recoveryRunRequest struct {
	Dir  string `json:"dir"`
	File string `json:"file"`
}

type recoveryListView struct {
	Code      int  `json:"code"`
	Page      int  `json:"page"`
	Size      int  `json:"size"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Counts    struct {
		Pending    int `json:"pending"`
		Recovering int `json:"recovering"`
		Failed     int `json:"failed"`
	} `json:"counts"`
	Data struct {
		Pending    []recoveryListEntry   `json:"pending"`
		Recovering []recoveryListEntry   `json:"recovering"`
		Failed     []recoveryListEntry   `json:"failed"`
		Recent     []recoveryRecentEntry `json:"recent"`
	} `json:"data"`
	Errors []string `json:"errors,omitempty"`
}

// ---------------------------------------------------------------- handlers

func (p *MP4Plugin) handleRecoveryList(w http.ResponseWriter, r *http.Request) {
	if !p.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	page, size, err := parseRecoveryPagination(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	recovery := p.recoveryTask.Load()
	if recovery == nil {
		http.Error(w, "recovery task not initialized", http.StatusServiceUnavailable)
		return
	}

	dirs, dirErrs := recovery.resolveDirs(nil)
	entries, truncated, scanErrs := scanRecoveryEntries(dirs, recoveryListScanLimit)

	view := recoveryListView{Code: 0, Page: page, Size: size, Truncated: truncated}
	view.Data.Pending = []recoveryListEntry{}
	view.Data.Recovering = []recoveryListEntry{}
	view.Data.Failed = []recoveryListEntry{}
	view.Data.Recent = p.recoveryStateStore().recentSnapshot(size)

	for _, entry := range entries {
		switch entry.Category {
		case "pending":
			view.Counts.Pending++
		case "recovering":
			view.Counts.Recovering++
		case "failed":
			view.Counts.Failed++
		}
	}
	view.Total = len(entries)

	start := (page - 1) * size
	for _, entry := range pageSlice(entries, start, size) {
		switch entry.Category {
		case "pending":
			view.Data.Pending = append(view.Data.Pending, entry)
		case "recovering":
			view.Data.Recovering = append(view.Data.Recovering, entry)
		case "failed":
			view.Data.Failed = append(view.Data.Failed, entry)
		}
	}

	for _, err := range append(dirErrs, scanErrs...) {
		if err != nil {
			view.Errors = append(view.Errors, err.Error())
		}
	}
	writeRecoveryJSON(w, http.StatusOK, view)
}

func (p *MP4Plugin) handleRecoveryRun(w http.ResponseWriter, r *http.Request) {
	if !p.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !p.requireRecoveryRunAuth(w, r) {
		return
	}
	recovery := p.recoveryTask.Load()
	if recovery == nil {
		http.Error(w, "recovery task not initialized", http.StatusServiceUnavailable)
		return
	}

	req, err := parseRecoveryRunRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	dirs, file, status, message := recovery.validateRunTarget(req.Dir, req.File)
	if status != 0 {
		http.Error(w, message, status)
		return
	}
	if !recovery.taskStarted() {
		// 未启动时 AddTask 会阻塞在无消费者的 event loop 上，必须先拒绝。
		http.Error(w, "recovery task not initialized", http.StatusServiceUnavailable)
		return
	}

	id, deduplicated, conflict, err := p.recoveryStateStore().startRun(recovery, dirs, file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conflict {
		writeRecoveryJSON(w, http.StatusConflict, map[string]any{
			"code":    http.StatusConflict,
			"id":      id,
			"message": "another recovery run is in progress",
		})
		return
	}
	statusCode := http.StatusAccepted
	if deduplicated {
		statusCode = http.StatusOK
	}
	writeRecoveryJSON(w, statusCode, map[string]any{
		"code":         0,
		"id":           id,
		"status":       recoveryRunRunning,
		"deduplicated": deduplicated,
	})
}

func (p *MP4Plugin) handleRecoveryStatus(w http.ResponseWriter, r *http.Request) {
	if !p.requireAdmin(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing run id", http.StatusBadRequest)
		return
	}
	run, ok := p.recoveryStateStore().getRun(id)
	if !ok {
		http.Error(w, "recovery run not found", http.StatusNotFound)
		return
	}
	writeRecoveryJSON(w, http.StatusOK, p.recoveryStateStore().runView(run))
}

// ---------------------------------------------------------------- 鉴权

// requireAdmin 在开启 admin 登录时要求 Bearer token 且用户角色为 admin。
// 仓库没有面向插件 HTTP handler 的 admin 中间件：pkg/auth.Middleware 只校验
// token 不校验角色，而 JWTClaims 不携带角色，因此这里额外查询 DB。
// 未开启登录（EnableLogin=false）或插件未绑定 Server 时按既有端点惯例放行。
func (p *MP4Plugin) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if p.Server == nil || !p.Server.ServerConfig.Admin.EnableLogin {
		return true
	}
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		http.Error(w, "missing authorization header", http.StatusUnauthorized)
		return false
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" {
		http.Error(w, "missing bearer token", http.StatusUnauthorized)
		return false
	}
	claims, err := p.Server.ValidateToken(token)
	if err != nil {
		http.Error(w, "invalid token", http.StatusUnauthorized)
		return false
	}
	// 角色不在 JWT 中，必须查库；DB 不可用时拒绝（fail closed）。
	if p.DB == nil {
		http.Error(w, "admin role check unavailable: database not enabled", http.StatusForbidden)
		return false
	}
	var user db.User
	if err = p.DB.Where("username = ?", claims.Username).First(&user).Error; err != nil {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	if user.Role != "admin" {
		http.Error(w, "admin role required", http.StatusForbidden)
		return false
	}
	return true
}

// requireRecoveryRunAuth 在未启用登录时仍限制 run 端点。
//
// 恢复管线会按 sidecar 内容读写文件并改写 DB，而 sidecar 可能是任意本机
// 进程投放的（例如全局可写的临时目录）。因此 admin.enablelogin=false 时
// 默认只接受本地回环请求；需要从远端触发时，运维必须显式开启
// record.recovery.allowunauthenticated。
func (p *MP4Plugin) requireRecoveryRunAuth(w http.ResponseWriter, r *http.Request) bool {
	if p.Server != nil && p.Server.ServerConfig.Admin.EnableLogin {
		// requireAdmin 已校验过 admin 角色。
		return true
	}
	if p.RecordConfig.Recovery.AllowUnauthenticated {
		return true
	}
	if isLoopbackRemoteAddr(r.RemoteAddr) {
		return true
	}
	p.Warn("rejected unauthenticated recovery run from non-loopback client",
		"remoteAddr", r.RemoteAddr,
		"hint", "set record.recovery.allowunauthenticated=true or admin.enablelogin=true")
	http.Error(w,
		"recovery run from a non-loopback client requires authentication; "+
			"set record.recovery.allowunauthenticated=true or admin.enablelogin=true",
		http.StatusForbidden)
	return false
}

// isLoopbackRemoteAddr 判断 HTTP 请求来源是否为本地回环地址。
// 只信任 RemoteAddr：X-Forwarded-For 等客户端可控头不能作为判据。
func isLoopbackRemoteAddr(remoteAddr string) bool {
	if remoteAddr == "" {
		return false
	}
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func writeRecoveryJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
