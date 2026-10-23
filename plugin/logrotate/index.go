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
	// defaultRotateSize / defaultRotateMaxFiles 是 Size / MaxFiles 的零值回退值,
	// 必须与 struct tag 的 default 保持一致(TestClampDefaultsMatchTags 钉住)。
	// 零值不能透传给 rotoslog: MaxFileSize(0) 会让每次写入都轮转,
	// MaxRotatedFiles(0) 会在轮转后立刻删除刚生成的文件。
	defaultRotateSize     uint64 = 20971520
	defaultRotateMaxFiles uint64 = 750
	// staleLogSampleLimit 是"上一份配置遗留日志"告警里列出的文件名上限,
	// 避免目录里有成百上千个旧文件时刷屏; total 字段仍给出完整数量。
	staleLogSampleLimit = 10
)

type LogRotatePlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	Path       string `default:"./logs" desc:"日志文件存放目录"`
	Size       uint64 `default:"20971520" desc:"单个日志文件的大小上限，单位：字节；磁盘占用上限 = Size × MaxFiles"`
	Days       int    `default:"30" desc:"日志文件保留天数，按文件修改时间计算；<=0 表示不做时间清理"`
	Formatter  string `default:"2006-01-02T15-04" desc:"日志文件名时间格式（Go time layout），默认分钟级以避免同一小时内多次轮转互相覆盖；留空会拒绝启动"`
	FilePrefix string `default:"m7s-" desc:"日志文件名前缀，用于隔离本插件文件并限定清理范围"`
	MaxFiles   uint64 `default:"750" desc:"最大轮转日志文件数量；磁盘占用上限 = Size × MaxFiles"`
	Level      string `default:"info" desc:"日志级别"`
	handler    slog.Handler
}

var _ = m7s.InstallPlugin[LogRotatePlugin](m7s.PluginMeta{
	RegisterGRPCHandler: pb.RegisterApiHandler,
	ServiceDesc:         &pb.Api_ServiceDesc,
})

// resolveRotateParams 校验并夹紧 rotoslog 参数。
//
// Size / MaxFiles 的零值在 rotoslog 里并非“使用默认值”而是字面意义的 0：
// MaxFileSize(0) 使 `written > 0` 恒真（每写一条就轮转一次），
// MaxRotatedFiles(0) 使轮转后立刻删除刚生成的文件。这两种“配置漏填”
// 过去是静默零值，这里回退到 default tag 的值并告警。
//
// Formatter 为空则无法命名轮转文件，也无法界定保留期清理范围：直接失败，
// 不做任何静默回退（改错配置应当立刻暴露，而不是悄悄换一套文件名）。
func (config *LogRotatePlugin) resolveRotateParams() (size, maxFiles uint64, err error) {
	if config.Formatter == "" {
		return 0, 0, errors.New("logrotate: formatter must not be empty (a Go time layout is required for rotated file names)")
	}
	size, maxFiles = config.Size, config.MaxFiles
	if size == 0 {
		size = defaultRotateSize
		config.Warn("log size is zero; using default", "size", size)
	}
	if maxFiles == 0 {
		maxFiles = defaultRotateMaxFiles
		config.Warn("max files is zero; using default", "maxFiles", maxFiles)
	}
	return size, maxFiles, nil
}

func (config *LogRotatePlugin) Start() (err error) {
	size, maxFiles, err := config.resolveRotateParams()
	if err != nil {
		return err
	}
	// 只提示、绝不删除：文件名不带当前前缀说明它不属于本插件（可能是旧的
	// fileprefix/formatter 配置留下的取证日志，也可能是别的进程的文件）。
	// 按前缀隔离原则删除会误伤，因此交给运维手工判断。
	config.warnAboutStaleLogs()
	builder := func(w io.Writer, opts *slog.HandlerOptions) slog.Handler {
		return console.NewHandler(w, &console.HandlerOptions{NoColor: true, Level: pkg.ParseLevel(config.Level), TimeFormat: "2006-01-02 15:04:05.000"})
	}
	config.handler, err = rotoslog.NewHandler(
		rotoslog.LogHandlerBuilder(builder),
		rotoslog.LogDir(config.Path),
		rotoslog.FilePrefix(config.FilePrefix),
		rotoslog.MaxFileSize(size),
		rotoslog.DateTimeLayout(config.Formatter),
		rotoslog.MaxRotatedFiles(maxFiles),
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

// warnAboutStaleLogs 在启动时一次性提示“上一份配置遗留的日志”。判定指纹是
// 名称不带当前 FilePrefix、但去掉扩展名后能被当前 Formatter 解析的 *.log：
// 这几乎只可能来自曾经的 fileprefix/formatter 配置（或 prefix 为空时的旧版本）。
// 只列不删——它们不在本插件的前缀隔离范围内，清理必须由运维决定。
func (config *LogRotatePlugin) warnAboutStaleLogs() {
	stale, err := foreignLogFiles(config.Path, config.FilePrefix, config.Formatter)
	if err != nil {
		config.Warn("scan for logs left by a previous logrotate config failed", "dir", config.Path, "error", err)
		return
	}
	if len(stale) == 0 {
		return
	}
	sample := stale
	if len(sample) > staleLogSampleLimit {
		sample = sample[:staleLogSampleLimit]
	}
	config.Warn("found log files that do not match the current fileprefix/formatter; they are left untouched (prefix isolation), clean them up manually",
		"dir", config.Path,
		"filePrefix", config.FilePrefix,
		"formatter", config.Formatter,
		"total", len(stale),
		"sample", sample)
}

// foreignLogFiles 返回 dir 下“像是上一份配置遗留”的日志文件名（已按名称排序）：
// 扩展名 .log、名称不带当前 prefix、且去掉扩展名后能被 layout 解析的普通文件。
//
// prefix 或 layout 为空时无法区分本插件与其他进程的文件，直接返回空：
// strings.HasPrefix(name, "") 恒真，按空前缀“匹配”会列出一整个目录。
// 目录不存在视为无遗留文件。
func foreignLogFiles(dir, prefix, layout string) ([]string, error) {
	if prefix == "" || layout == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var stale []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, prefix) || !entry.Type().IsRegular() {
			continue
		}
		if _, ok := rotatedLogStamp(name, layout); !ok {
			continue
		}
		stale = append(stale, name)
	}
	return stale, nil
}

// rotatedLogStamp 判断文件名是否属于「轮转日志」命名约定：以 .log 结尾且去掉
// 扩展名后的时间戳能被 layout 解析。命中的文件名在轮转/清理/遗留检测之间共用
// 同一口径，避免三处判定漂移。
func rotatedLogStamp(name, layout string) (string, bool) {
	if !strings.HasSuffix(name, logFileExt) {
		return "", false
	}
	stamp := strings.TrimSuffix(name, logFileExt)
	if _, err := time.Parse(layout, stamp); err != nil {
		return "", false
	}
	return stamp, true
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
	// 显式把 FilePrefix 配成空串时,HasPrefix(name, "") 恒真,会把目录内
	// 形如 2026-10-22T20-37.log 的其它进程文件也当作本插件日志清理。
	// 这种情况禁用清理并告警,绝不按"无前缀=全目录"处理。
	if config.FilePrefix == "" {
		config.Warn("log retention prune disabled: empty FilePrefix would match every file", "dir", config.Path, "days", config.Days)
		return 0, nil
	}
	return pruneOldLogs(config.Path, config.FilePrefix, config.Formatter, config.Days, now)
}

// pruneOldLogs 删除 dir 下符合本插件命名、且修改时间早于 now-Days*24h 的轮转文件。
// 语义:
//   - days <= 0 表示禁用清理,直接返回;
//   - prefix 为空表示无法界定本插件文件,禁用清理(保留全部文件);
//   - 只处理普通文件,子目录/符号链接一律跳过;
//   - 只处理 <prefix><timestamp><.log> 且 timestamp 能被 layout 解析的文件,
//     当前写入文件(<prefix>current.log)与其它进程的文件永不删除;
//   - 目录不存在视为无文件可清理(不报错)。
func pruneOldLogs(dir, prefix, layout string, days int, now time.Time) (int, error) {
	if days <= 0 || layout == "" || prefix == "" {
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
		if !strings.HasPrefix(name, prefix) {
			// 非本插件命名的文件(其它进程日志等)不归本插件清理。
			continue
		}
		if _, ok := rotatedLogStamp(strings.TrimPrefix(name, prefix), layout); !ok {
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
