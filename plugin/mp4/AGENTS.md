# MP4 Plugin Guidelines

MP4容器格式录制、点播、录像管理。支持fMP4流式录制、后处理优化、事件录像、存储管理。

## 核心功能

| 功能 | 实现文件 | 说明 |
|------|---------|------|
| **MP4录制** | `pkg/recorder.go` | 实时流转MP4文件 |
| **fMP4点播** | `pkg/puller.go` | MP4文件转m7s流 |
| **后处理** | `recovery.go` | MOOV box前置优化 |
| **事件录像** | `pkg/event_recorder.go` | 前后缓冲录制 |
| **存储管理** | `pkg/storage_management.go` | 磁盘占用控制+自动清理 |
| **录像提取** | `api_extract.go` | GOP级别精确裁剪 |

## 文件结构

### MP4 Box结构 (ISO BMFF)

```
[ftyp] 文件类型
[moov] 元数据容器
  [mvhd] 视频头
  [trak] 音频轨道
    [mdia] 媒体信息
      [mdhd] 媒体头
      [hdlr] 处理器
      [minf] 媒体信息容器
        [stbl] 样本表
          [stsd] 样本描述
          [stts] 时间戳映射
          [stsc] 样本到chunk映射
          [stsz] 样本大小表
          [stco] chunk偏移表
  [trak] 视频轨道
[mdat] 媒体数据
```

**参考**: `box_structure.md` 详细说明

### fMP4特点

- **流式写入**: 不需要预知总时长
- **MOOV前置**: 播放器可立即seek
- **分片(Fragment)**: 每个GOP一个`moof`+`mdat`
- **Web友好**: HLS/DASH标准格式

## 录制流程

### 标准录制

```go
// 配置录制器
recorder := mp4pkg.NewRecorder()
recorder.StartTime = startTime  // 可选:指定开始时间
recorder.Folder = "/recordings"
recorder.FileName = "stream"

// 启动录制
err := recorder.Start()
// 自动订阅流并写入MP4
```

### 事件录像

```yaml
mp4:
  beforeduration: 30s  # 事件前缓冲
  afterduration: 30s   # 事件后延续
  eventrecordfilepath: "/events"
```

**触发事件录像**:
```bash
POST /mp4/api/event/start
{
  "streamPath": "live/camera1",
  "eventType": "motion_detect"
}
```

**实现**:
- 使用m7s的 `RingReader` 回溯历史帧
- 事件触发时立即捕获前30秒数据
- 事件结束后继续录制30秒

### 录像回调

```go
// 录制完成后触发
func (r *MP4Recorder) OnComplete() {
  // 1. 执行MOOV前置优化
  RecoveryMP4(r.FilePath)
  
  // 2. 触发外部存储上传 (如S3)
  if s3Enabled {
    s3plugin.TriggerUpload(r.FilePath, deleteAfter)
  }
}
```

## 点播播放

### 本地文件点播

```bash
# 格式: /mp4/<streamPath>.mp4
# streamPath需映射到实际文件路径

GET /mp4/live/recording1.mp4
```

**配置映射**:
```yaml
mp4:
  pull:
    streampath:
      live/recording1: /data/recordings/2024/01/recording1.mp4
```

### URL点播

```yaml
mp4:
  pull:
    url: https://example.com/video.mp4
```

### 录像裁剪

```bash
# 按时间范围提取
GET /mp4/extract/compressed/<streamPath>?start=2024-01-01T10:00:00&end=2024-01-01T10:05:00

# 按GOP提取 (精确到关键帧)
GET /mp4/extract/gop/<streamPath>?startGop=0&endGop=10
```

**实现**: `api_extract.go`
- 读取STBL表定位I帧位置
- 重建MOOV box调整时间戳
- 写入新MP4文件(仅包含指定范围)

## 后处理优化

### MOOV前置

**问题**: 流式录制时MOOV在文件末尾,导致:
- 无法web快速加载(需下载全文件)
- 播放器无法seek

**解决**: `recovery.go` 实现

```bash
# 自动优化
POST /mp4/api/recovery
{
  "filePath": "/recordings/stream.mp4"
}
```

**原理**:
```
优化前: [ftyp][mdat (数据)][moov (元数据)]
优化后: [ftyp][moov (元数据)][mdat (数据)]
```

**步骤**:
1. 读取文件末尾MOOV box
2. 创建临时文件写入: ftyp + moov + 调整后的mdat
3. 原子替换原文件

### 异常恢复

```yaml
mp4:
  autorecovery: true  # 自动修复损坏的MP4
```

**修复类型**:
- 缺失MOOV: 从mdat重建样本表
- 时间戳错误: 重新计算stts/ctts
- Chunk偏移错误: 重建stco/co64

## 存储管理

### 磁盘占用控制

```yaml
mp4:
  diskmaxpercent: 90      # 硬盘使用上限%
  overwritepercent: 80    # 触发清理阈值%
  recordfileexpiredays: 7 # 自动删除天数
```

**清理策略**:
1. **优先级**: 最旧文件 → 体积最大文件
2. **保留规则**: 保留最近1小时录像
3. **迁移选项**: 可配置移动到冷存储而非删除

### 存储任务

```go
// pkg/storage_management.go
type StorageManagementTask struct {
  task.TickTask  // 每5分钟检查一次
  DB *gorm.DB
}

func (t *StorageManagementTask) Run() {
  usage := getDiskUsage()
  if usage > t.OverwritePercent {
    // 删除或迁移最旧文件
    deleteOldestFiles(targetSize)
  }
}
```

## Box操作

### 自定义Box

```go
// pkg/box/custom.go
type MyCustomBox struct {
  box.FullBox
  Data []byte
}

func (b *MyCustomBox) Marshal(w io.Writer) {
  b.FullBox.Marshal(w)
  binary.Write(w, binary.BigEndian, b.Data)
}
```

### 解析MP4

```go
import "m7s.live/v5/plugin/mp4/pkg/box"

// 读取所有box
file, _ := os.Open("video.mp4")
boxes, _ := box.ReadAll(file)

for _, b := range boxes {
  switch v := b.(type) {
  case *box.FtypBox:
    fmt.Println("File type:", v.MajorBrand)
  case *box.MoovBox:
    // 访问tracks
    for _, trak := range v.Traks {
      fmt.Println("Track ID:", trak.Tkhd.TrackID)
    }
  }
}
```

## API接口

### 录像管理

```bash
# 录像列表
GET /mp4/api/list?streamPath=live/camera1

# 开始录制
POST /mp4/api/start
{
  "streamPath": "live/camera1",
  "folder": "/recordings",
  "fileName": "test"
}

# 停止录制
POST /mp4/api/stop
{
  "streamPath": "live/camera1"
}

# 删除录像
DELETE /mp4/api/delete?file=/recordings/test.mp4
```

### 事件录像

```bash
# 触发事件录像
POST /mp4/api/event/start
{
  "streamPath": "live/camera1",
  "eventType": "alarm",
  "duration": 60  # 总时长(秒)
}
```

### 截图

```bash
# 提取某一帧作为JPEG
GET /mp4/api/snap/<streamPath>?time=2024-01-01T10:00:00
```

## 性能优化

### 写入优化

- **缓冲策略**: 每GOP写入一次(减少磁盘I/O)
- **预分配**: `ftyp`+`moov` 预留空间
- **批量写入**: 音视频数据合并写入mdat

### 读取优化

- **索引缓存**: STBL表加载到内存
- **Range请求**: 支持HTTP Range实现seek
- **GOP对齐**: 跳转时定位最近的I帧

### 内存控制

```yaml
mp4:
  # 单个recorder最大内存(默认自动)
  maxmemory: 100MB
```

## 常见问题

| 问题 | 原因 | 解决 |
|------|------|------|
| 录像无法播放 | MOOV在文件末尾 | 启用 `autorecovery: true` |
| 文件体积异常大 | 未清理或GOP过大 | 检查设备I帧间隔 |
| 录像时间戳跳变 | 源流DTS/PTS错误 | 启用时间戳修正 |
| 磁盘占满导致服务停止 | 未配置清理策略 | 设置 `overwritepercent` |
| 提取录像卡顿 | GOP未对齐 | 使用 `/extract/gop/` 接口 |

## 测试

```bash
# 录制测试流
ffmpeg -re -i test.mp4 -c copy -f rtmp rtmp://localhost/live/test

# 查看录像列表
curl http://localhost:8080/mp4/api/list

# 播放录像
ffplay http://localhost:8080/mp4/live/test.mp4
```

## 相关文档

- `box_structure.md` - MP4 box详细结构
- `README_CN.md` - 中文使用说明
- [ISO/IEC 14496-12](https://mpeg.chiariglione.org/standards/mpeg-4/iso-base-media-file-format) - BMFF标准
