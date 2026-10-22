package plugin_logrotate

import (
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alchemy/rotoslog"
	task "github.com/eanfs/gotask"
	"github.com/phsym/console-slog"

	"m7s.live/v5"
	"m7s.live/v5/pkg"
	"m7s.live/v5/plugin/logrotate/pb"
)

const (
	// defaultFormatter 日志文件名时间格式。默认精确到分钟而不是小时:
	// rotoslog 轮转用 os.Rename 且目标名只由 formatter 决定,同一格式时刻内
	// 第二次轮转会改名覆盖第一次(静默丢日志)。按小时命名时高写入量下每小时
	// 必然覆盖,分钟级把该风险降到分钟粒度。
	defaultFormatter = "2006-01-02T15-04"
	// defaultFilePrefix 本插件日志专用前缀。rotoslog 的 MaxRotatedFiles 计数与
	// 本插件的每日清理都只应作用于本插件文件——前缀为空时(rotoslog 默认)
	// strings.HasPrefix(name, "") 恒真,同目录下其它进程日志/子目录都会被计数
	// 并可能被当作最旧文件删除。
	defaultFilePrefix = "m7s-"
	// logFileExt / currentFileSuffix 与 rotoslog 的默认 FileExt / CurrentFileSuffix
	// 保持一致(本插件未覆盖这两个选项)。
	logFileExt        = ".log"
	currentFileSuffix = "current"
	// pruneInterval 每日执行一次保留期清理。
	pruneInterval = 24 * time.Hour
)

type LogRotatePlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	Path       string `default:"./logs" desc:"日志文件存放目录"`
	Size       uint64 `default:"20971520" desc:"日志文件大小，单位：字节"`
	Days       int    `default:"30" desc:"日志文件保留天数，按文件修改时间计算；<=0 表示不做时间清理"`
	Formatter  string `default:"2006-01-02T15-04" desc:"日志文件名时间格式（Go time layout），默认分钟级以避免同一小时内多次轮转互相覆盖"`
	FilePrefix string `default:"m7s-" desc:"日志文件名前缀，用于隔离本插件文件并限定清理范围"`
	MaxFiles   uint64 `default:"750" desc:"最大日志文件数量"`
	Level      string `default:"info" desc:"日志级别"`
	handler    slog.Handler
}

var _ = m7s.InstallPlugin[LogRotatePlugin](m7s.PluginMeta{
	RegisterGRPCHandler: pb.RegisterApiHandler,
	ServiceDesc:         &pb.Api_ServiceDesc,
})

func (config *LogRotatePlugin) Start() (err error) {
	builder := func(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
		return console.NewHandler(w, &console.HandlerOptions{NoColor: true, Level: pkg.ParseLevel(config.Level), TimeFormat: "2006-01-02 15:04:05.000"})
	}
	config.handler, err = rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(builder),
		rotoslog.LogDir(config.Path),
		rotoslog.FilePrefix(config.FilePrefix),
		rotoslog.MaxFileSize(config.Size),
		rotoslog.DateTimeLayout(config.Formatter),
		rotoslog.MaxRotatedFiles(config.MaxFiles),
	)
	if err != nil {
		return err
	}
	config.Server.LogHandler.Add(config.handler)

	if config.Days <= 0 {
		config.Warn("log retention prune disabled: days <= 0", "days", config.Days, "dir", config.Path)
		return nil
	}
	// 必须在 AddTask 之前注册任何 OnStart 回调(本项目 gotask 的已知陷阱);
	// 每日清理任务不使用 OnStart,启动时的一次清理写在 Start 里。
	config.AddTask(&logPruneTask{plugin: config})
	return nil
}

// logPruneTask 按 Days 删除过期的轮转日志。走任务系统(TickTask),不使用裸 goroutine;
// 启动时先清理一次,之后每 24h 一次。重复执行安全:只删除 mtime 早于保留期、
// 文件名匹配 Formatter 且带本插件前缀的普通文件。
type logPruneTask struct {
	task.TickTask
	plugin *LogRotatePlugin
}

func (t *logPruneTask) GetTickInterval() time.Duration {
	return pruneInterval
}

func (t *logPruneTask) Start() error {
	if err := t.TickTask.Start(); err != nil {
		return err
	}
	t.prune()
	return nil
}

func (t *logPruneTask) Tick(any) {
	t.prune()
}

func (t *logPruneTask) prune() {
	removed, err := t.plugin.pruneOldLogs(time.Now())
	if err != nil {
		t.Warn("log retention prune finished with errors", "removed", removed, "days", t.plugin.Days, "dir", t.plugin.Path, "error", err)
		return
	}
	if removed > 0 {
		t.Info("log retention prune removed expired files", "removed", removed, "days", t.plugin.Days, "dir", t.plugin.Path)
	}
}

func (config *LogRotatePlugin) pruneOldLogs(now time.Time) (int, error) {
	return pruneOldLogs(config.Path, config.FilePrefix, config.Formatter, config.Days, now)
}

// pruneOldLogs 删除 dir 下符合本插件命名、且修改时间早于 now-Days*24h 的轮转文件。
// 语义:
//   - days <= 0 表示禁用清理,直接返回;
//   - 只处理普通文件,子目录/符号链接一律跳过;
//   - 只处理 <prefix><timestamp><.log> 且 timestamp 能被 layout 解析的文件,
//     当前写入文件(<prefix>current.log)与其它进程的文件永不删除;
//   - 目录不存在视为无文件可清理(不报错)。
func pruneOldLogs(dir, prefix, layout string, days int, now time.Time) (int, error) {
	if days <= 0 || layout == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}

	cutoff := now.Add(-time.Duration(days) * 24 * time.Hour)
	currentName := prefix + currentFileSuffix + logFileExt
	removed := 0
	var errs []error
	for _, entry := range entries {
		name := entry.Name()
		// 正在写入的 current 文件永远保留(它也匹配前缀与扩展名)。
		if name == currentName {
			continue
		}
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, logFileExt) {
			// 非本插件命名的文件(其它进程日志等)不归本插件清理。
			continue
		}
		stamp := strings.TrimSuffix(strings.TrimPrefix(name, prefix), logFileExt)
		if _, err := time.Parse(layout, stamp); err != nil {
			// 前缀相同但时间戳不符合本插件 Formatter,同样不碰。
			continue
		}
		info, err := entry.Info()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
