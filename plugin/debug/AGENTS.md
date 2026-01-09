# Debug Plugin Guidelines

性能分析、监控和调试工具集。提供CPU/内存/阻塞分析、任务追踪、实时指标采集。

## 核心功能

| 功能 | 实现文件 | 用途 |
|------|---------|------|
| **Profile采集** | `profile.go`, `pkg/profile/` | CPU/内存/阻塞/互斥锁分析 |
| **实时监控** | `chart.go` | WebSocket推送系统指标 |
| **任务历史** | `pkg/task_history_model.go` | 持久化任务执行记录 |
| **环境检查** | `envcheck.go` | 启动时诊断系统配置 |
| **pprof集成** | `index.go` | 标准Go pprof端点 |

## 快速开始

### 基础配置
```yaml
debug:
  enablechart: true         # 启用图表功能
  enabletaskhistory: false  # 启用任务历史(需DB)
  profileduration: 10s      # profile采集时长
  profile: ""               # 启动时自动采集CPU profile文件
```

### 常用调试端点

| 端点 | 功能 |
|------|------|
| `/debug/pprof/` | pprof索引页 |
| `/debug/pprof/heap` | 内存分配 |
| `/debug/pprof/goroutine` | 协程栈 |
| `/debug/pprof/block` | 阻塞分析 |
| `/debug/pprof/mutex` | 互斥锁竞争 |
| `/debug/api/profile` | 触发profile采集(gRPC) |

## Profile采集

### 采集类型

```go
// CPU Profile (10秒采样)
POST /debug/api/profile
{
  "type": "cpu",
  "duration": "10s"
}

// 内存 Profile
POST /debug/api/profile
{
  "type": "heap"
}

// 阻塞 Profile
POST /debug/api/profile
{
  "type": "block"
}

// 互斥锁 Profile
POST /debug/api/profile
{
  "type": "mutex"
}
```

### 缓存机制
- **首次采集**: 自动缓存CPU profile数据
- **后续请求**: 直接返回缓存(避免重复采集开销)
- **缓存刷新**: 重启服务后失效
- **实现**: `cpuProfileData` + `cpuProfileOnce` + `cpuProfileLock`

### 分析工具

```bash
# 1. 获取profile文件
curl http://localhost:8080/debug/pprof/profile?seconds=30 > cpu.prof

# 2. 本地分析
go tool pprof cpu.prof
# 交互命令: top, list, web

# 3. Web UI分析
go tool pprof -http=:8081 cpu.prof
```

## 实时监控

### WebSocket图表

```javascript
// 连接监控WebSocket
const ws = new WebSocket('ws://localhost:8080/debug/chart');

ws.onmessage = (event) => {
  const metrics = JSON.parse(event.data);
  // metrics: {cpu, memory, goroutines, timestamp, ...}
};
```

### 指标采集

- **采集间隔**: 1秒
- **推送协议**: JSON over WebSocket
- **数据结构**: `pkg/chart.go` 的 `MetricsData`

### 实现细节

```go
// chart.go
type server struct {
  task.ChannelTask[*Conn]
}

func (s *server) Run() {
  ticker := time.NewTicker(time.Second)
  for {
    select {
    case <-ticker.C:
      // 采集系统指标
      metrics := collectMetrics()
      // 广播到所有WebSocket连接
      s.BroadcastJSON(metrics)
    }
  }
}
```

## 任务历史追踪

### 启用条件
- `enabletaskhistory: true`
- 配置数据库 (SQLite/MySQL/PostgreSQL)

### 数据模型

```go
type TaskHistory struct {
  SessionID   uint      // 会话ID
  TaskType    string    // 任务类型
  Description string    // 任务描述
  StartTime   time.Time
  EndTime     *time.Time
  Duration    int64     // 毫秒
  Status      string    // running/completed/failed
  ErrorMsg    string
}
```

### 查询接口

```bash
# 查询某会话的所有任务
GET /debug/api/tasks?sessionId=123

# 查询失败任务
GET /debug/api/tasks?status=failed
```

## 环境检查

### 启动诊断 (`envcheck.go`)

自动检测并报告:
- **CPU核心数**: `runtime.NumCPU()`
- **Go版本**: `runtime.Version()`
- **操作系统**: `runtime.GOOS`
- **CGO状态**: `CGO_ENABLED`
- **构建标签**: 编译时传入的tags
- **端口监听**: 验证配置的端口可用性

### 诊断输出

```
[INFO] Environment Check:
  ✓ CPU Cores: 8
  ✓ Go Version: go1.24.10
  ✓ OS: darwin
  ✓ CGO: enabled
  ✓ Build Tags: sqlite,fasthttp
  ✗ Port 8080: already in use
```

## GRF集成 (GoRef)

### 内存泄漏检测

```yaml
debug:
  grfout: "grf.out"  # goref输出文件
```

**工作原理**:
- 使用 `github.com/cloudwego/goref` 跟踪对象引用
- 定期dump未释放的对象到 `grf.out`
- 需要编译标签: `-tags=goref`

**分析泄漏**:
```bash
# 查看GRF报告
cat grf.out

# 按引用计数排序
sort -rn -k2 grf.out | head -20
```

## 调试最佳实践

### 1. 性能瓶颈定位

```bash
# Step 1: 采集30秒CPU profile
curl http://localhost:8080/debug/pprof/profile?seconds=30 > cpu.prof

# Step 2: 分析热点函数
go tool pprof -top cpu.prof

# Step 3: 查看火焰图
go tool pprof -http=:8081 cpu.prof
```

### 2. 内存泄漏排查

```bash
# 采集两个时间点的heap
curl http://localhost:8080/debug/pprof/heap > heap1.prof
# 等待5分钟
curl http://localhost:8080/debug/pprof/heap > heap2.prof

# 对比差异
go tool pprof -base heap1.prof heap2.prof
```

### 3. 协程泄漏检查

```bash
# 查看协程数量
curl http://localhost:8080/debug/pprof/goroutine?debug=1

# 分析协程栈
go tool pprof http://localhost:8080/debug/pprof/goroutine
```

### 4. 阻塞分析

```bash
# 确保启用了阻塞分析 (plugin已自动启用)
# runtime.SetBlockProfileRate(1)

# 采集阻塞profile
curl http://localhost:8080/debug/pprof/block > block.prof

# 分析阻塞点
go tool pprof -top block.prof
```

## 常见问题

| 问题 | 原因 | 解决 |
|------|------|------|
| pprof端点404 | 插件未启用 | 检查配置 `debug: enable: true` |
| profile文件为空 | 采集时间过短 | 增加 `?seconds=30` 参数 |
| 图表WebSocket断开 | 网络超时/服务重启 | 客户端实现自动重连 |
| 任务历史不记录 | DB未配置 | 启用 `sqlite/mysql/postgres` 标签编译 |
| GRF无输出 | 未使用goref编译 | 添加 `-tags=goref` 重新编译 |

## 高级技巧

### 自定义指标

```go
// 在你的代码中注册自定义Prometheus指标
import "github.com/prometheus/client_golang/prometheus"

var myCounter = prometheus.NewCounter(
  prometheus.CounterOpts{
    Name: "my_custom_metric",
    Help: "My custom metric",
  },
)

func init() {
  prometheus.MustRegister(myCounter)
}

// 访问 /api/metrics 查看
```

### 动态调整日志级别

```go
// 运行时修改日志级别(需实现API)
POST /debug/api/loglevel
{
  "level": "debug"  // trace/debug/info/warn/error
}
```

### 远程调试(Delve)

```go
// index.go 中的 debugger.New(conf) 支持
// 启动时启用 DAP (Debug Adapter Protocol)
// VSCode可以远程attach
```

## 性能影响

| 功能 | 性能开销 | 建议 |
|------|---------|------|
| Chart监控 | ~0.1% CPU | 生产环境可启用 |
| 任务历史 | ~0.5% CPU (写DB) | 仅调试时启用 |
| Block Profile | ~1-2% CPU | 短期启用,定位问题后关闭 |
| CPU Profile采集 | ~5% CPU (采集期间) | 按需触发,非常驻 |

## 相关文档

- `pkg/profile/README.md` - Profile采集实现细节
- [pprof官方文档](https://golang.org/pkg/net/http/pprof/)
- [Delve调试器](https://github.com/go-delve/delve)
