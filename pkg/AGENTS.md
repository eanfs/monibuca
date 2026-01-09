# PKG Guidelines

共享框架工具包。提供编解码器、格式解析、配置系统、任务管理、存储抽象等核心功能。

## 目录结构

| 子包 | 职责 | 关键文件 |
|------|------|---------|
| `codec/` | 编解码器抽象 | `audio.go`, `video.go`, `h264.go`, `h265.go` |
| `format/` | 容器格式 | `ps/`, `flv/`, `mpegts/` |
| `config/` | 配置系统 | `config.go`, `pull.go`, `push.go` |
| `task/` | 异步任务框架 | `task.go`, `job.go`, `work.go` |
| `util/` | 通用工具 | `ring.go`, `pool.go`, `collection.go` |
| `storage/` | 存储抽象 | `local.go`, `cos.go`, `s3.go` |
| `db/` | 数据库集成 | `duckdb.go`, `mysql.go`, `postgres.go` |
| `auth/` | 认证授权 | `auth.go`, `jwt.go` |

## 核心组件

### Codec (编解码器)

```go
// pkg/codec/video.go
type VideoCodec interface {
  FourCC() FourCC        // 编码器ID (H264/H265/VP8...)
  Parse(data []byte) error  // 解析配置(SPS/PPS/VPS)
}

// 常用编解码器
const (
  FourCC_H264 = "avc1"
  FourCC_H265 = "hvc1"
  FourCC_VP8  = "vp08"
  FourCC_VP9  = "vp09"
  FourCC_AV1  = "av01"
)
```

**使用示例**:
```go
import "m7s.live/v5/pkg/codec"

// 解析H264配置
h264 := &codec.H264{}
err := h264.Parse(spsNalu)  // 提取宽高/帧率/profile
fmt.Println(h264.Width, h264.Height)
```

### Format (容器格式)

#### PS (Program Stream - GB28181常用)

```go
// pkg/format/ps/ps.go
type PSReader struct {
  OnPS   func(*PSPacket)  // PS包回调
  OnPES  func(*PESPacket) // PES包回调
}

// 解封装PS流
reader := &ps.PSReader{}
reader.OnPES = func(pes *PESPacket) {
  // pes.StreamID: 音频(0xC0-0xDF)/视频(0xE0-0xEF)
  // pes.PTS, pes.DTS: 时间戳
  // pes.Data: H264/AAC裸流
}
reader.Input(psData)
```

#### FLV

```go
// pkg/format/flv.go
type FLVFrame struct {
  Type      uint8  // 8=音频, 9=视频, 18=脚本
  Timestamp uint32
  Data      []byte
}

// FLV解析
flv.ParseTag(data, func(frame *FLVFrame) {
  // 处理音视频帧
})
```

### Config (配置系统)

#### 优先级顺序
```
动态修改 > 环境变量 > 配置文件 > 默认YAML > 全局配置
```

#### 插件配置定义

```go
// pkg/config/config.go
type PluginConfig struct {
  Enable  bool   `yaml:"enable" default:"true"`
  TCP     *TCP   `yaml:"tcp"`
  UDP     *UDP   `yaml:"udp"`
  Pull    Pull   `yaml:"pull"`
  Publish Publish `yaml:"publish"`
}

// 配置文件示例
// config.yaml:
// myplugin:
//   enable: true
//   tcp:
//     listenaddr: :8080
```

#### Pull/Push配置

```go
// pkg/config/pull.go
type Pull struct {
  URL         string        `yaml:"url"`
  StreamPath  string        `yaml:"streampath"`
  ReConnect   time.Duration `yaml:"reconnect" default:"5s"`
  PullOnSub   map[string]string `yaml:"pullonsub"`  // 自动拉流规则
}
```

### Task (任务系统)

**层级**:
```
Work (持久队列管理器)
  └── Job (任务容器+事件循环)
      └── Task (基础任务单元)
```

#### Task - 基础任务

```go
type MyTask struct {
  task.Task
  Data string
}

func (t *MyTask) Start() error {
  // 初始化阶段(仅执行一次)
  return nil
}

func (t *MyTask) Run() error {
  // 主执行阶段
  // 返回 task.ErrTaskComplete 表示成功完成
  return task.ErrTaskComplete
}

func (t *MyTask) Dispose() {
  // 清理阶段
}
```

#### Job - 任务容器

```go
type MyJob struct {
  task.Job  // 自动退出型容器
}

// 添加子任务
myJob.AddTask(&childTask, logger)
```

#### Work - 持久队列

```go
type UploadQueue struct {
  task.Work  // keepalive=true,永不退出
}

var uploadQueue UploadQueue

func init() {
  m7s.Servers.AddTask(&uploadQueue)
}

// 从任意位置提交任务
uploadQueue.AddTask(&UploadTask{File: "test.mp4"}, logger)
```

#### TickTask - 定时任务

```go
type CleanupTask struct {
  task.TickTask  // 内置ticker
}

func (t *CleanupTask) Run() error {
  // 每隔 interval 执行一次
  t.Interval = 5 * time.Minute
  deleteOldFiles()
  return nil  // 返回nil继续执行
}
```

#### ChannelTask - 信号驱动

```go
type ProcessorTask struct {
  task.ChannelTask[*DataItem]
}

func (t *ProcessorTask) Run() error {
  for item := range t.C {  // 阻塞等待数据
    process(item)
  }
  return nil
}

// 发送数据
task.C <- &DataItem{...}
```

### Util (工具集)

#### Ring Buffer (环形缓冲)

```go
// pkg/util/ring.go
ring := util.NewRing[*Frame](100)  // 100个槽位

// 写入
ring.Write(&frame)

// 读取(从指定位置开始)
reader := ring.NewReader(offset)
for frame := range reader.Read() {
  // 处理帧
}
```

#### Collection (并发集合)

```go
// pkg/util/collection.go
type MyItem struct {
  ID string
}

func (i *MyItem) GetKey() string { return i.ID }

var items util.Collection[string, *MyItem]

// 添加
items.Add(&MyItem{ID: "test"})

// 获取
item, ok := items.Get("test")

// 遍历
items.Range(func(key string, item *MyItem) {
  // 处理item
})
```

#### Pool (对象池)

```go
// pkg/util/pool.go
var bufferPool = util.NewPool(func() []byte {
  return make([]byte, 4096)
})

// 获取
buf := bufferPool.Get()
defer bufferPool.Put(buf)
```

### Storage (存储抽象)

```go
// pkg/storage/storage.go
type Storage interface {
  Write(path string, data io.Reader) error
  Read(path string) (io.ReadCloser, error)
  Delete(path string) error
  List(prefix string) ([]FileInfo, error)
}

// 本地存储
local := &LocalStorage{Root: "/data"}

// S3兼容存储
s3 := &S3Storage{
  Endpoint:  "s3.amazonaws.com",
  Bucket:    "my-bucket",
  AccessKey: "...",
  SecretKey: "...",
}
```

### DB (数据库)

```go
// pkg/db/db.go
import "gorm.io/gorm"

// 自动选择数据库(根据build tags)
db, err := db.Open(config.Database{
  DSN: "test.db",  // SQLite
  // DSN: "user:pass@tcp(127.0.0.1:3306)/db",  // MySQL
})

// 使用gorm
db.AutoMigrate(&MyModel{})
db.Create(&MyModel{...})
```

## 常用模式

### 解析视频流

```go
import (
  "m7s.live/v5/pkg/codec"
  "m7s.live/v5/pkg/format"
)

// H264 Annex-B 转 AVCC
annexbData := []byte{0x00, 0x00, 0x00, 0x01, ...}
avccData := format.AnnexBToAVCC(annexbData)

// 解析NALU类型
naluType := codec.ParseH264NALUType(data[0])
switch naluType {
case codec.NALU_SPS:
  // SPS处理
case codec.NALU_PPS:
  // PPS处理
case codec.NALU_IDR_Picture:
  // I帧处理
}
```

### 配置文件合并

```go
// pkg/config/config.go
type MyPluginConfig struct {
  config.PluginConfig `yaml:",inline"`
  CustomField string `yaml:"customfield"`
}

// 加载时自动合并全局配置
cfg := &MyPluginConfig{}
config.LoadPluginConfig("myplugin", cfg)
```

### 异步任务协作

```go
// 插件A: 创建持久队列
var processQueue task.Work
m7s.Servers.AddTask(&processQueue)

// 插件B: 提交任务到队列
processQueue.AddTask(&MyTask{...}, logger)

// Task: 实现处理逻辑
type MyTask struct {
  task.Task
}

func (t *MyTask) Run() error {
  // 执行耗时操作
  return task.ErrTaskComplete
}
```

## 性能考量

### 零拷贝

- 使用 `[]byte` slice避免内存复制
- `io.Copy` 利用 `WriterTo`/`ReaderFrom`
- Ring buffer直接引用源数据

### 内存池

```go
// 避免频繁分配
var pool = sync.Pool{
  New: func() any { return make([]byte, 4096) },
}
```

### 并发集合

- `Collection` 使用 `sync.Map` (读多写少场景)
- 避免遍历时长时间持锁

## 测试

```go
// pkg/codec/h264_test.go
func TestH264Parse(t *testing.T) {
  sps := []byte{0x67, ...}
  h264 := &H264{}
  err := h264.Parse(sps)
  assert.NoError(t, err)
  assert.Equal(t, 1920, h264.Width)
}
```

## 避坑指南

| 问题 | 原因 | 解决 |
|------|------|------|
| Task不执行 | 未添加到Work/Job | 确保 `AddTask()` 调用 |
| Config不生效 | 字段未导出 | 确保首字母大写 |
| Ring溢出 | 写入速度>读取速度 | 增大容量或丢弃旧帧 |
| 数据库连接失败 | 缺少build tag | 编译时添加 `-tags sqlite` |
| NALU解析错误 | Annex-B vs AVCC格式混淆 | 使用 `AnnexBToAVCC` 转换 |

## 扩展点

### 自定义编解码器

```go
// 实现 VideoCodec 接口
type MyCodec struct {
  codec.BaseCodec
}

func (c *MyCodec) FourCC() codec.FourCC {
  return "mycd"
}

func (c *MyCodec) Parse(data []byte) error {
  // 解析配置
  return nil
}

// 注册
codec.RegisterVideoCodec("mycd", func() codec.VideoCodec {
  return &MyCodec{}
})
```

### 自定义存储后端

```go
type MyStorage struct {}

func (s *MyStorage) Write(path string, data io.Reader) error {
  // 实现写入
  return nil
}

// 实现完整 Storage 接口
```

## 相关文档

- `pkg/task/README.md` - 任务系统详解
- `pkg/codec/README.md` - 编解码器开发指南
- `doc/arch/task.md` - 任务架构设计
