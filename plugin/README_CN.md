# 插件开发指南

## 1. 准备工作

### 开发工具
- Visual Studio Code
- Goland
- Cursor
- CodeBuddy
- Trae
- Qoder
- Claude Code
- Kiro
- Windsurf

### 安装gRPC
```shell
$ go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
$ go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### 安装gRPC-Gateway
```shell
$ go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
$ go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
$ go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### 创建工程
- 创建一个go 工程，例如：`MyPlugin`
- 创建目录`pkg`，用来存放可导出的代码
- 创建目录`pb`，用来存放gRPC的proto文件
- 创建目录`example`， 用来测试插件

> 也可以直接在 monibuca 项目的 plugin 中创建一个目录`xxx`, 用来存放插件代码
## 2. 创建插件

```go
package plugin_myplugin
import (
    "m7s.live/v5"
)

var _ = m7s.InstallPlugin[MyPlugin]()

type MyPlugin struct {
	m7s.Plugin
	Foo string
}

```
- MyPlugin 结构体就是插件定义，Foo 是插件的一个属性，可以在配置文件中配置。
- 必须嵌入`m7s.Plugin`结构体，这样插件就具备了插件的基本功能。
- `m7s.InstallPlugin[MyPlugin](...)` 用来注册插件，这样插件就可以被 monibuca 加载。
### 传入默认配置
例如：
```go
const defaultConfig = m7s.DefaultYaml(`tcp:
  listenaddr: :5554`)

var _ = m7s.InstallPlugin[MyPlugin](m7s.PluginMeta{
    DefaultYaml: defaultConfig,
})
```
## 3. 实现事件回调（可选）
### 初始化回调
```go
func (config *MyPlugin) Start() (err error) {
    // 初始化一些东西
    return
}
```
用于插件的初始化，此时插件的配置已经加载完成，可以在这里做一些初始化工作。返回错误则插件初始化失败，插件将进入禁用状态。

### 接受 TCP 请求回调

```go
func (config *MyPlugin) OnTCPConnect(conn *net.TCPConn) task.ITask {
	
}
```
当配置了 tcp 监听端口后，收到 tcp 连接请求时，会调用此回调。

### 接受 UDP 请求回调
```go
func (config *MyPlugin) OnUDPConnect(conn *net.UDPConn) task.ITask {

}
```
当配置了 udp 监听端口后，收到 udp 连接请求时，会调用此回调。

### 接受 QUIC 请求回调
```go
func (config *MyPlugin) OnQUICConnect(quic.Connection) task.ITask {

}
```
当配置了 quic 监听端口后，收到 quic 连接请求时，会调用此回调。

## 4. HTTP 接口回调
### 延续 v4 的回调
```go
func (config *MyPlugin) API_test1(rw http.ResponseWriter, r *http.Request) {
	        // do something
}
```
可以通过`http://ip:port/myplugin/api/test1`来访问`API_test1`方法。

### 通过配置映射表
这种方式可以实现带参数的路由，例如：
```go
func (config *MyPlugin) RegisterHandler() map[string]http.HandlerFunc {
	return map[string]http.HandlerFunc{
		"/test1/{streamPath...}": config.test1,
	}
}
func (config *MyPlugin) test1(rw http.ResponseWriter, r *http.Request) {
	        streamPath := r.PathValue("streamPath")
          // do something
}
```
## 5. 实现推拉流客户端

### 实现推流客户端
推流客户端需要实现 IPusher 接口，然后将创建 IPusher 的方法传入 InstallPlugin 中。
```go
type Pusher struct {
  task.Task
  pushJob m7s.PushJob
}

func (c *Pusher) GetPushJob() *m7s.PushJob {
	return &c.pushJob
}

func NewPusher(_ config.Push) m7s.IPusher {
	return &Pusher{}
}
var _ = m7s.InstallPlugin[MyPlugin](m7s.PluginMeta{
    NewPusher: NewPusher,
})

```

### 实现拉流客户端
拉流客户端需要实现 IPuller 接口，然后将创建 IPuller 的方法传入 InstallPlugin 中。
下面这个 Puller 继承了 m7s.HTTPFilePuller，可以实现基本的文件和 HTTP拉流。具体拉流逻辑需要覆盖 Start 方法。
```go
type Puller struct {
	m7s.HTTPFilePuller
}

func NewPuller(_ config.Pull) m7s.IPuller {
	return &Puller{}
}
var _ = m7s.InstallPlugin[MyPlugin](m7s.PluginMeta{
    NewPuller: NewPuller,
})
```

## 6. 实现gRPC服务
实现 gRPC 可以自动生成对应的 restFul 接口，方便调用。
### 在`pb`目录下创建`myplugin.proto`文件
```proto
syntax = "proto3";
import "google/api/annotations.proto";
import "google/protobuf/empty.proto";
package myplugin;
option go_package="m7s.live/v5/plugin/myplugin/pb";

service api {
    rpc MyMethod (MyRequest) returns (MyResponse) {
     option (google.api.http) = {
          post: "/myplugin/api/bar"
          body: "foo"
        };
    }
}
message MyRequest {
    string foo = 1;
}
message MyResponse {
    string bar = 1;
}
```
以上的定义只中包含了实现对应 restFul 的路由，可以通过 post 请求`/myplugin/api/bar`来调用`MyMethod`方法。
### 生成gRPC代码
- 可以使用 vscode 的 task.json中加入
```json
{
      "type": "shell",
      "label": "build pb myplugin",
      "command": "protoc",
      "args": [
        "-I.",
        "-I${workspaceRoot}/pb",
        "--go_out=.",
        "--go_opt=paths=source_relative",
        "--go-grpc_out=.",
        "--go-grpc_opt=paths=source_relative",
        "--grpc-gateway_out=.",
        "--grpc-gateway_opt=paths=source_relative",
        "myplugin.proto"
      ],
      "options": {
        "cwd": "${workspaceRoot}/plugin/myplugin/pb"
      }
    },
```
- 或者在 pb 目录下运行命令行:
```shell
protoc -I. -I$ProjectFileDir$/pb --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative --grpc-gateway_out=. --grpc-gateway_opt=paths=source_relative  myplugin.proto
```
把其中的 `$ProjectFileDir$` 替换成包含全局 pb 的目录，全局 pb 文件就在 monibuca 项目的 pb 目录下。

### 实现gRPC服务
创建 api.go 文件
```go
package plugin_myplugin
import (
    "context"
    "m7s.live/m7s/v5"
    "m7s.live/m7s/v5/plugin/myplugin/pb"
)

func (config *MyPlugin) MyMethod(ctx context.Context, req *pb.MyRequest) (*pb.MyResponse, error) {
    return &pb.MyResponse{Bar: req.Foo}, nil
}
```
### 注册gRPC服务
```go
package plugin_myplugin
import (
    "m7s.live/v5"
	"m7s.live/v5/plugin/myplugin/pb"
)

var _ = m7s.InstallPlugin[MyPlugin](m7s.PluginMeta{
	ServiceDesc:         &pb.Api_ServiceDesc,
	RegisterGRPCHandler: pb.RegisterApiHandler,
})

type MyPlugin struct {
	pb.UnimplementedApiServer
	m7s.Plugin
	Foo string
}

```
### 额外的 restFul 接口
和 v4 相同
```go
func (config *MyPlugin)  API_test1(rw http.ResponseWriter, r *http.Request) {
	        // do something
}
```
就可以通过 get 请求`/myplugin/api/test1`来调用`API_test1`方法。

## 7. 发布流

```go
publisher, err := p.Publish(ctx, streamPath)
```
`ctx` 参数是必需的，`streamPath` 参数是必需的。

### 写入音视频数据

旧的 `WriteAudio` 和 `WriteVideo` 方法已被更结构化的写入器模式取代，使用泛型实现：

#### **创建写入器**
```go
// 音频写入器
audioWriter := m7s.NewPublishAudioWriter[*AudioFrame](publisher, allocator)

// 视频写入器  
videoWriter := m7s.NewPublishVideoWriter[*VideoFrame](publisher, allocator)

// 组合音视频写入器
writer := m7s.NewPublisherWriter[*AudioFrame, *VideoFrame](publisher, allocator)
```

#### **写入帧**
```go
// 设置时间戳并写入音频帧
writer.AudioFrame.SetTS32(timestamp)
err := writer.NextAudio()

// 设置时间戳并写入视频帧
writer.VideoFrame.SetTS32(timestamp)
err := writer.NextVideo()
```

#### **写入自定义数据**
```go
// 对于自定义数据帧
err := publisher.WriteData(data IDataFrame)
```

### 定义音视频数据
如果现有的音视频数据格式无法满足需求，可以自定义音视频数据格式。
但需要满足转换格式的要求。即需要实现下面这个接口：
```go
IAVFrame interface {
    GetSample() *Sample
    GetSize() int
    CheckCodecChange() error
    Demux() error      // demux to raw format
    Mux(*Sample) error // mux from origin format
    Recycle()
    String() string
}
```
> 音频和视频需要定义两个不同的类型

其中各方法的作用如下：
- GetSample 方法用于获取音视频数据的Sample对象，包含编解码上下文和原始数据。
- GetSize 方法用于获取音视频数据的大小。
- CheckCodecChange 方法用于检查编解码器是否发生变化。
- Demux 方法用于解封装音视频数据到裸格式，用于给其他格式封装使用。
- Mux 方法用于从原始格式封装成自定义格式的音视频数据。
- Recycle 方法用于回收资源，会在嵌入 RecyclableMemory 时自动实现。
- String 方法用于打印音视频数据的信息。

### 内存管理
新的模式包含内置的内存管理：
- `gomem.ScalableMemoryAllocator` - 用于高效的内存分配
- 通过 `Recycle()` 方法进行帧回收
- 自动内存池管理

## 8. 订阅流
```go
var suber *m7s.Subscriber
suber, err = p.Subscribe(ctx,streamPath)
go m7s.PlayBlock(suber, handleAudio, handleVideo)
```
这里需要注意的是 handleAudio, handleVideo 是处理音视频数据的回调函数，需要自己实现。
handleAudio/Video 的入参是一个你需要接受到的音视频格式类型,返回 error，如果返回的 error 不是 nil，则订阅中止。

## 9. 使用 H26xFrame 处理裸流数据

### 9.1 理解 H26xFrame 结构

`H26xFrame` 结构体用于处理 H.264/H.265 裸流数据：

```go
type H26xFrame struct {
    pkg.Sample
}
```

主要特性：
- 继承自 `pkg.Sample` - 包含编解码上下文、内存管理和时间戳信息
- 使用 `Raw.(*pkg.Nalus)` 存储 NALU（网络抽象层单元）数据
- 支持 H.264 (AVC) 和 H.265 (HEVC) 格式
- 使用高效的内存分配器实现零拷贝操作

### 9.2 创建 H26xFrame 进行发布

```go
import (
    "m7s.live/v5"
    "m7s.live/v5/pkg/format"
    "m7s.live/v5/pkg/util"
    "time"
)

// 创建支持 H26xFrame 的发布器 - 多帧发布
func publishRawH264Stream(streamPath string, h264Frames [][]byte) error {
    // 获取发布器
    publisher, err := p.Publish(streamPath)
    if err != nil {
        return err
    }
    
    // 创建内存分配器
    allocator := gomem.NewScalableMemoryAllocator(1 << gomem.MinPowerOf2)
    defer allocator.Recycle()
    
    // 创建 H26xFrame 写入器
    writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
    
    // 设置 H264 编码器上下文
    writer.VideoFrame.ICodecCtx = &format.H264{}
    
    // 发布多帧
    // 注意：这只是演示一次写入多帧，实际情况是逐步写入的，即从视频源接收到一帧就写入一帧
    startTime := time.Now()
    for i, frameData := range h264Frames {
        // 为每帧创建 H26xFrame
        frame := writer.VideoFrame
        
        // 设置正确间隔的时间戳
        frame.Timestamp = startTime.Add(time.Duration(i) * time.Second / 30) // 30 FPS
        
        // 写入 NALU 数据
        nalus := frame.GetNalus() 
        // 假如 frameData 中只有一个 NALU，否则需要循环执行下面的代码
        p := nalus.GetNextPointer()
        mem := frame.NextN(len(frameData))
        copy(mem, frameData)
        p.PushOne(mem)	
        // 发布帧
        if err := writer.NextVideo(); err != nil {
            return err
        }
    }
    
    return nil
}

// 连续流发布示例
func continuousH264Publishing(streamPath string, frameSource <-chan []byte, stopChan <-chan struct{}) error {
    // 获取发布器
    publisher, err := p.Publish(streamPath)
    if err != nil {
        return err
    }
    defer publisher.Dispose()
    
    // 创建内存分配器
    allocator := gomem.NewScalableMemoryAllocator(1 << gomem.MinPowerOf2)
    defer allocator.Recycle()
    
    // 创建 H26xFrame 写入器
    writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
    
    // 设置 H264 编码器上下文
    writer.VideoFrame.ICodecCtx = &format.H264{}
    
    startTime := time.Now()
    frameCount := 0
    
    for {
        select {
        case frameData := <-frameSource:
            // 为每帧创建 H26xFrame
            frame := writer.VideoFrame
            
            // 设置正确间隔的时间戳
            frame.Timestamp = startTime.Add(time.Duration(frameCount) * time.Second / 30) // 30 FPS
            
            // 写入 NALU 数据
            nalus := frame.GetNalus()
            mem := frame.NextN(len(frameData))
            copy(mem, frameData)
            
            // 发布帧
            if err := writer.NextVideo(); err != nil {
                return err
            }
            
            frameCount++
            
        case <-stopChan:
            // 停止发布
            return nil
        }
    }
}
```

### 9.3 处理 H26xFrame（转换器模式）

```go
type MyTransform struct {
    m7s.DefaultTransformer
    Writer *m7s.PublishWriter[*format.RawAudio, *format.H26xFrame]
}

func (t *MyTransform) Go() {
    defer t.Dispose()
    
    for video := range t.Video {
        if err := t.processH26xFrame(video); err != nil {
            t.Error("process frame failed", "error", err)
            break
        }
    }
}

func (t *MyTransform) processH26xFrame(video *format.H26xFrame) error {
    // 复制帧元数据
    copyVideo := t.Writer.VideoFrame
    copyVideo.ICodecCtx = video.ICodecCtx
    *copyVideo.BaseSample = *video.BaseSample
    nalus := copyVideo.GetNalus()
    
    // 处理每个 NALU 单元
    for nalu := range video.Raw.(*pkg.Nalus).RangePoint {
        p := nalus.GetNextPointer()
        mem := copyVideo.NextN(nalu.Size)
        nalu.CopyTo(mem)
        
        // 示例：过滤或修改特定 NALU 类型
        if video.FourCC() == codec.FourCC_H264 {
            switch codec.ParseH264NALUType(mem[0]) {
            case codec.NALU_IDR_Picture, codec.NALU_Non_IDR_Picture:
                // 处理视频帧 NALU
                // 示例：应用转换、滤镜等
            case codec.NALU_SPS, codec.NALU_PPS:
                // 处理参数集 NALU
            }
        } else if video.FourCC() == codec.FourCC_H265 {
            switch codec.ParseH265NALUType(mem[0]) {
            case h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL:
                // 处理 H.265 IDR 帧
            }
        }
        
        // 推送处理后的 NALU
        p.PushOne(mem)
    }
    
    return t.Writer.NextVideo()
}
```

### 9.4 H.264/H.265 常见 NALU 类型

#### H.264 NALU 类型
```go
const (
    NALU_Non_IDR_Picture = 1  // 非 IDR 图像（P 帧）
    NALU_IDR_Picture     = 5  // IDR 图像（I 帧）
    NALU_SEI             = 6  // 补充增强信息
    NALU_SPS             = 7  // 序列参数集
    NALU_PPS             = 8  // 图像参数集
)

// 从第一个字节解析 NALU 类型
naluType := codec.ParseH264NALUType(mem[0])
```

#### H.265 NALU 类型
```go
// 从第一个字节解析 H.265 NALU 类型
naluType := codec.ParseH265NALUType(mem[0])
```

### 9.5 内存管理最佳实践

```go
// 使用内存分配器进行高效操作
allocator := gomem.NewScalableMemoryAllocator(1 << 20) // 1MB 初始大小
defer allocator.Recycle()

// 处理多帧时重用同一个分配器
writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
```

### 9.6 错误处理和验证

```go
func processFrame(video *format.H26xFrame) error {
    // 检查编解码器变化
    if err := video.CheckCodecChange(); err != nil {
        return err
    }
    
    // 验证帧数据
    if video.Raw == nil {
        return fmt.Errorf("empty frame data")
    }
    
    // 安全处理 NALU
    nalus, ok := video.Raw.(*pkg.Nalus)
    if !ok {
        return fmt.Errorf("invalid NALUs format")
    }
    
    // 处理帧...
    return nil
}
```

## 10. 接入 Prometheus
只需要实现 Collector 接口，系统会自动收集所有插件的指标信息。
```go
func (p *MyPlugin) Describe(ch chan<- *prometheus.Desc) {
  
}

func (p *MyPlugin) Collect(ch chan<- prometheus.Metric) {
  
}

## 插件合并说明

### Monitor 插件合并到 Debug 插件

从 v5 版本开始，Monitor 插件的功能已经合并到 Debug 插件中。这种合并简化了插件结构，并提供了更统一的调试和监控体验。

#### 功能变更

- Monitor 插件的所有功能现在可以通过 Debug 插件访问
- 任务监控 API 路径从 `/monitor/api/*` 变更为 `/debug/api/monitor/*`
- 数据模型和数据库结构保持不变
- Session 和 Task 的监控逻辑完全迁移到 Debug 插件

#### 使用方法

以前通过 Monitor 插件访问的 API 现在应该通过 Debug 插件访问：

```
# 旧路径
GET /monitor/api/session/list
GET /monitor/api/search/task/{sessionId}

# 新路径
GET /debug/api/monitor/session/list
GET /debug/api/monitor/task/{sessionId}
```

#### 配置变更

不再需要单独配置 Monitor 插件，只需配置 Debug 插件即可。Debug 插件会自动初始化监控功能。

```yaml
debug:
  enable: true
  # 其他 debug 配置项
```

```