# Monibuca 视频录制代码逻辑分析

## 概述

Monibuca 的视频录制功能采用模块化、插件化的架构设计，通过抽象的存储层支持本地文件和云存储（S3/OSS/COS）。本文档详细分析视频录制的完整调用流程和核心逻辑。

## 核心组件

### 1. API 层
- **文件**: `plugin/mp4/api.go`
- **入口函数**: `StartRecord`, `StopRecord`
- **职责**: 接收 HTTP/gRPC 请求，参数验证，调用核心录制逻辑

### 2. 插件层
- **文件**: `plugin.go`
- **核心函数**: `Plugin.Record()`
- **职责**: 创建录制器实例，初始化 RecordJob

### 3. 录制器层
- **文件**: `recoder.go`, `plugin/mp4/pkg/record.go`
- **核心类型**: `DefaultRecorder`, `Recorder` (MP4专用)
- **职责**: 管理录制生命周期，处理音视频数据

### 4. 存储层
- **文件**: `pkg/storage/storage.go`, `pkg/storage/s3.go`, `pkg/storage/local.go`
- **核心接口**: `Storage`, `File`
- **职责**: 抽象文件操作，支持多种存储后端

## 完整调用流程

### 阶段 1: API 请求处理

```
HTTP/gRPC 请求
    ↓
MP4Plugin.StartRecord(ctx, req)
    ├─ 解析请求参数
    │   ├─ streamPath: 流路径
    │   ├─ filePath: 文件保存目录
    │   ├─ fileName: 文件名（可选）
    │   └─ fragment: 分片时长
    ├─ 检查录制是否已存在
    ├─ 构建 config.Record 配置
    └─ 获取或等待 Publisher
```

**关键代码** (`plugin/mp4/api.go:438-481`):
```go
func (p *MP4Plugin) StartRecord(ctx, req) {
    // 1. 参数解析
    filePath := req.FilePath
    fileName := req.FileName
    fragment := req.Fragment
    
    // 2. 检查重复录制
    if recordExists {
        return ErrRecordExists
    }
    
    // 3. 构建配置
    recordConf := config.Record{
        FilePath: filePath,
        FileName: fileName,
        Fragment: fragment,
    }
    
    // 4. 获取流发布者
    stream := p.Server.Streams.Get(streamPath)
    
    // 5. 启动录制
    job := p.Record(stream, recordConf, nil)
    job.WaitStarted()
}
```

### 阶段 2: 录制任务初始化

```
Plugin.Record(publisher, conf, subConf)
    ↓
创建 Recorder 实例
    ├─ NewRecorder(conf) → 返回 MP4 Recorder
    ├─ recorder.GetRecordJob().Init(...)
    │   ├─ 设置 Plugin、StreamPath、RecConf
    │   ├─ 配置订阅参数 (SubType = SubscribeTypeVod)
    │   └─ 注册钩子 (OnRecordStart, OnRecordEnd)
    └─ publisher.Using(job) → 绑定到发布者
```

**关键代码** (`plugin.go:719-724`):
```go
func (p *Plugin) Record(pub *Publisher, conf config.Record, subConf *config.Subscribe) *RecordJob {
    recorder := p.Meta.NewRecorder(conf)  // 创建 MP4 Recorder
    job := recorder.GetRecordJob().Init(recorder, p, pub.StreamPath, conf, subConf)
    pub.Using(job)  // 绑定到发布者
    return job
}
```

### 阶段 3: 订阅流数据

```
RecordJob.Start()
    ↓
RecordJob.Subscribe()
    ├─ Plugin.SubscribeWithConfig(streamPath, subConf)
    │   └─ 创建 Subscriber，订阅音视频数据
    └─ Recorder.Start()
        └─ DefaultRecorder.Start()
            └─ RecordJob.Subscribe()
```

**关键代码** (`recoder.go:64-66, 151-154`):
```go
func (r *DefaultRecorder) Start() error {
    return r.RecordJob.Subscribe()
}

func (p *RecordJob) Subscribe() error {
    p.Subscriber, err = p.Plugin.SubscribeWithConfig(
        p.recorder.GetTask().Context, 
        p.StreamPath, 
        *p.SubConf
    )
    return err
}
```

### 阶段 4: 创建录制文件

```
Recorder.createStream(startTime)
    ↓
DefaultRecorder.CreateStream(start, CustomFileName)
    ├─ 调用 CustomFileName 生成文件路径
    │   ├─ 如果 RecConf.FileName 存在 → 使用指定文件名
    │   └─ 否则 → 生成时间戳文件名 (2006-01-02-15-04-05_nanosecond.mp4)
    ├─ createStorage(storageConfig)
    │   ├─ 遍历 Storage 配置
    │   ├─ 调用 storage.CreateStorage(type, config)
    │   │   ├─ type="s3" → 创建 S3Storage
    │   │   ├─ type="oss" → 创建 OSSStorage
    │   │   ├─ type="cos" → 创建 COSStorage
    │   │   └─ type="local" → 创建 LocalStorage
    │   └─ 返回 Storage 实例
    ├─ storage.CreateFile(ctx, filePath)
    │   └─ 返回 File 接口实例
    └─ 保存 RecordStream 到数据库
```

**关键代码** (`recoder.go:68-106`):
```go
func (r *DefaultRecorder) CreateStream(start time.Time, customFileName func(*RecordJob) string) error {
    // 1. 生成文件路径
    filePath := customFileName(recordJob)
    
    // 2. 创建存储实例
    recordJob.storage = r.createStorage(recordJob.RecConf.Storage)
    
    // 3. 初始化 RecordStream
    r.Event.RecordStream = RecordStream{
        StartTime:    start,
        StreamPath:   sub.StreamPath,
        FilePath:     filePath,
        Type:         recordJob.RecConf.Type,
        StorageLevel: 1,
    }
    
    // 4. 保存到数据库
    if recordJob.Plugin.DB != nil {
        recordJob.Plugin.DB.Save(&r.Event.RecordStream)
    }
}
```

**CustomFileName 逻辑** (`plugin/mp4/pkg/record.go:137-140`):
```go
var CustomFileName = func(job *m7s.RecordJob) string {
    if fn := job.RecConf.FileName; fn != "" {
        // 使用指定文件名
        if !strings.HasSuffix(strings.ToLower(fn), ".mp4") {
            fn = fn + ".mp4"
        }
        return filepath.Join(job.RecConf.FilePath, fn)
    }
    // 生成时间戳文件名
    now := time.Now()
    return filepath.Join(job.RecConf.FilePath, 
        fmt.Sprintf("%s_%09d.mp4", now.Local().Format("2006-01-02-15-04-05"), now.Nanosecond()))
}
```

### 阶段 5: 音视频数据写入

```
Recorder.Run()
    ↓
订阅音视频帧
    ├─ PlayBlock(subscriber, onAudio, onVideo)
    ├─ onVideo(frame) → Muxer.WriteVideo(frame)
    │   ├─ 编码 H.264/H.265 数据
    │   ├─ 写入 mdat box
    │   └─ 更新 sample 列表
    ├─ onAudio(frame) → Muxer.WriteAudio(frame)
    │   ├─ 编码 AAC 数据
    │   ├─ 写入 mdat box
    │   └─ 更新 sample 列表
    └─ File.Write(data) → 写入存储
        ├─ S3File: 写入本地临时文件
        └─ LocalFile: 直接写入磁盘
```

**MP4 封装流程**:
1. 初始化 Muxer，写入 `ftyp` box
2. 预留 `moov` box 空间（后续回填）
3. 持续写入 `mdat` box（音视频数据）
4. 录制结束时，生成 `moov` box 并回填

### 阶段 6: 录制结束与文件上传

```
StopRecord / 流断开
    ↓
Recorder.Dispose()
    ↓
Recorder.writeTailer(endTime)
    ├─ WriteTail(endTime, writeTrailerQueueTask)
    │   ├─ 更新 RecordStream.EndTime
    │   └─ 保存到数据库
    └─ writeTrailerQueueTask.AddTask(writeTrailerTask)
        ↓
writeTrailerTask.Start()
    ├─ Muxer.WriteTrailer(file)
    │   ├─ 生成 moov box（包含所有 sample 元数据）
    │   ├─ 创建临时文件
    │   ├─ 写入 ftyp + moov
    │   ├─ 复制 mdat 数据
    │   └─ 替换原文件（Fast Start 格式）
    └─ File.Close()
        ├─ S3File.Close()
        │   ├─ Sync() → uploadTempFile()
        │   │   └─ s3manager.Uploader.Upload(tempFile)
        │   └─ 删除本地临时文件
        └─ LocalFile.Close()
            └─ 直接关闭文件
```

**关键代码** (`plugin/mp4/pkg/record.go:128-135`):
```go
func (r *Recorder) writeTailer(end time.Time) {
    r.WriteTail(end, &writeTrailerQueueTask)
    writeTrailerQueueTask.AddTask(&writeTrailerTask{
        muxer:    r.muxer,
        file:     r.file,
        filePath: r.Event.FilePath,
    }, r.Logger)
}
```

**S3 上传逻辑** (`pkg/storage/s3.go:295-331`):
```go
func (w *S3File) Sync() error {
    if w.tempFile != nil {
        w.tempFile.Sync()  // 同步到磁盘
    }
    return w.uploadTempFile()  // 上传到 S3
}

func (w *S3File) Close() error {
    w.Sync()  // 触发上传
    if w.tempFile != nil {
        w.tempFile.Close()
    }
    os.Remove(w.filePath)  // 清理临时文件
    return nil
}
```

## 存储抽象层设计

### Storage 接口
```go
type Storage interface {
    CreateFile(ctx, path) (File, error)
    Delete(ctx, path) error
    Exists(ctx, path) (bool, error)
    GetSize(ctx, path) (int64, error)
    GetURL(ctx, path) (string, error)
    List(ctx, prefix) ([]FileInfo, error)
    Close() error
}
```

### File 接口
```go
type File interface {
    Writer  // Write, WriteAt, Sync, Seek
    Reader  // Read, ReadAt, Seek
    Stat() (os.FileInfo, error)
    Name() string
    Close() error
}
```

### S3 存储实现要点

1. **本地缓冲机制**: 
   - MP4 封装需要随机读写（回填 moov box）
   - S3 不支持随机写，因此使用本地临时文件作为缓冲
   - 所有 Write/WriteAt 操作先写入临时文件

2. **延迟上传**:
   - 录制过程中，数据仅写入本地临时文件
   - 调用 `Close()` 时才触发上传
   - 使用 `s3manager.Uploader` 进行分块上传

3. **Fast Start 优化**:
   - `writeTrailerTask` 将 moov box 移动到文件头部
   - 确保视频可以边下载边播放

## 分片录制逻辑

当配置 `Fragment` 参数时，录制会自动分片：

```
Recorder.Run()
    ↓
每隔 Fragment 时长
    ├─ 关闭当前文件
    ├─ writeTailer(currentTime)
    └─ createStream(currentTime)
        └─ 创建新文件继续录制
```

分片文件命名规则：
- 无 FileName: `2006-01-02-15-04-05_000000001.mp4`, `2006-01-02-15-04-05_000000002.mp4`
- 有 FileName: `custom_name.mp4`, `custom_name_1.mp4`, `custom_name_2.mp4`

## 数据库记录

### RecordStream 表结构
```go
type RecordStream struct {
    ID           uint
    StartTime    time.Time
    EndTime      time.Time
    Duration     uint32
    Filename     string       // 文件名
    Type         string       // mp4/flv/hls
    FilePath     string       // 完整路径
    StreamPath   string       // 流路径
    AudioCodec   string
    VideoCodec   string
    RecordLevel  EventLevel   // high/low
    StorageLevel int          // 1=主存储, 2=次级存储
}
```

## 错误处理与重试

1. **订阅失败**: 自动重试，间隔 1 秒
2. **存储创建失败**: 降级到本地存储
3. **写入失败**: 停止录制，记录错误日志
4. **上传失败**: 保留本地临时文件，返回错误

## 性能优化

1. **异步写入**: writeTrailerTask 在独立队列中执行，不阻塞主流程
2. **内存池**: Muxer 使用对象池复用 buffer
3. **分块上传**: S3 使用 s3manager 进行并发分块上传
4. **Fast Start**: moov box 前置，支持流式播放

## 配置示例

### 本地存储
```yaml
mp4:
  record:
    filePath: /data/records
    fileName: custom_video  # 可选
    fragment: 1h
    storage:
      local: /data/records
```

### S3 存储
```yaml
mp4:
  record:
    filePath: videos
    fragment: 1h
    storage:
      s3:
        endpoint: s3.amazonaws.com
        region: us-east-1
        accessKeyID: YOUR_ACCESS_KEY
        secretAccessKey: YOUR_SECRET_KEY
        bucket: my-bucket
        pathPrefix: recordings/
```

## API 调用示例

### 启动录制
```bash
curl -X POST http://localhost:8080/mp4/api/start/live/stream1 \
  -H "Content-Type: application/json" \
  -d '{
    "filePath": "/data/records",
    "fileName": "my_video",
    "fragment": "1h"
  }'
```

### 停止录制
```bash
curl -X POST http://localhost:8080/mp4/api/stop/live/stream1
```

## 总结

Monibuca 的视频录制架构具有以下特点：

1. **模块化设计**: API、插件、录制器、存储层职责清晰
2. **存储抽象**: 统一接口支持本地和云存储
3. **异步处理**: writeTrailerTask 异步处理 MP4 优化
4. **灵活配置**: 支持自定义文件名、分片时长、存储后端
5. **健壮性**: 完善的错误处理和重试机制

这种设计使得系统易于扩展，可以方便地添加新的存储后端或录制格式。
