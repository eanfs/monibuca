# Plugin Development Guide

## 1. Prerequisites

### Development Tools
- Visual Studio Code
- Goland
- Cursor
- CodeBuddy
- Trae
- Qoder
- Claude Code
- Kiro
- Windsurf

### Install gRPC
```shell
$ go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
$ go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### Install gRPC-Gateway
```shell
$ go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
$ go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
$ go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
```

### Project Setup
- Create a Go project, e.g., `MyPlugin`
- Create a `pkg` directory for exportable code
- Create a `pb` directory for gRPC proto files
- Create an `example` directory for testing the plugin

> You can also create a directory `xxx` directly in the monibuca project's plugin folder to store your plugin code

## 2. Create a Plugin

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
- `MyPlugin` struct is the plugin definition, `Foo` is a plugin property that can be configured in the configuration file
- Must embed `m7s.Plugin` struct to provide basic plugin functionality
- `m7s.InstallPlugin[MyPlugin](...)` registers the plugin so it can be loaded by monibuca

### Provide Default Configuration
Example:
```go
const defaultConfig = m7s.DefaultYaml(`tcp:
  listenaddr: :5554`)

var _ = m7s.InstallPlugin[MyPlugin](m7s.PluginMeta{
    DefaultYaml: defaultConfig,
})
```

## 3. Implement Event Callbacks (Optional)

### Initialization Callback
```go
func (config *MyPlugin) Start() (err error) {
    // Initialize things
    return
}
```
Used for plugin initialization after configuration is loaded. Return an error if initialization fails, and the plugin will be disabled.

### TCP Request Callback
```go
func (config *MyPlugin) OnTCPConnect(conn *net.TCPConn) task.ITask {
    
}
```
Called when receiving TCP connection requests if TCP listening port is configured.

### UDP Request Callback
```go
func (config *MyPlugin) OnUDPConnect(conn *net.UDPConn) task.ITask {

}
```
Called when receiving UDP connection requests if UDP listening port is configured.

### QUIC Request Callback
```go
func (config *MyPlugin) OnQUICConnect(quic.Connection) task.ITask {

}
```
Called when receiving QUIC connection requests if QUIC listening port is configured.

## 4. HTTP Interface Callbacks

### Legacy v4 Callback Style
```go
func (config *MyPlugin) API_test1(rw http.ResponseWriter, r *http.Request) {
    // do something
}
```
Accessible via `http://ip:port/myplugin/api/test1`

### Route Mapping Configuration
This method supports parameterized routing:
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

## 5. Implement Push/Pull Clients

### Implement Push Client
Push client needs to implement IPusher interface and pass the creation method to InstallPlugin.
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

### Implement Pull Client
Pull client needs to implement IPuller interface and pass the creation method to InstallPlugin.
The following Puller inherits from m7s.HTTPFilePuller for basic file and HTTP pulling. You need to override the Start method for specific pulling logic:
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

## 6. Implement gRPC Service

### Create `myplugin.proto` in `pb` Directory
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

### Generate gRPC Code
Add to VSCode task.json:
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
}
```
Or run command in pb directory:
```shell
protoc -I. -I$ProjectFileDir$/pb --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative --grpc-gateway_out=. --grpc-gateway_opt=paths=source_relative  myplugin.proto
```
Replace `$ProjectFileDir$` with the directory containing global pb files.

### Implement gRPC Service
Create api.go:
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

### Register gRPC Service
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

### Additional RESTful Endpoints
Same as v4:
```go
func (config *MyPlugin) API_test1(rw http.ResponseWriter, r *http.Request) {
    // do something
}
```
Accessible via GET request to `/myplugin/api/test1`

## 7. Publishing Streams

```go
publisher, err := p.Publish(ctx, streamPath)
```
The `ctx` parameter is required, `streamPath` parameter is required.

### Writing Audio/Video Data

The old `WriteAudio` and `WriteVideo` methods have been replaced with a more structured writer pattern using generics:

#### **Create Writers**
```go
// Audio writer
audioWriter := m7s.NewPublishAudioWriter[*AudioFrame](publisher, allocator)

// Video writer  
videoWriter := m7s.NewPublishVideoWriter[*VideoFrame](publisher, allocator)

// Combined audio/video writer
writer := m7s.NewPublisherWriter[*AudioFrame, *VideoFrame](publisher, allocator)
```

#### **Write Frames**
```go
// Set timestamp and write audio frame
writer.AudioFrame.SetTS32(timestamp)
err := writer.NextAudio()

// Set timestamp and write video frame
writer.VideoFrame.SetTS32(timestamp)
err := writer.NextVideo()
```

#### **Write Custom Data**
```go
// For custom data frames
err := publisher.WriteData(data IDataFrame)
```

### Define Audio/Video Data
If existing audio/video data formats don't meet your needs, you can define custom formats by implementing this interface:
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
> Define separate types for audio and video

The methods serve the following purposes:
- GetSample: Gets the Sample object containing codec context and raw data
- GetSize: Gets the size of audio/video data
- CheckCodecChange: Checks if the codec has changed
- Demux: Demuxes audio/video data to raw format for use by other formats
- Mux: Muxes from original format to custom audio/video data format
- Recycle: Recycles resources, automatically implemented when embedding RecyclableMemory
- String: Prints audio/video data information

### Memory Management
The new pattern includes built-in memory management:
- `util.ScalableMemoryAllocator` - For efficient memory allocation
- Frame recycling through `Recycle()` method
- Automatic memory pool management

## 8. Subscribing to Streams
```go
var suber *m7s.Subscriber
suber, err = p.Subscribe(ctx,streamPath)
go m7s.PlayBlock(suber, handleAudio, handleVideo)
```
Note that handleAudio and handleVideo are callback functions you need to implement. They take an audio/video format type as input and return an error. If the error is not nil, the subscription is terminated.

## 9. Working with H26xFrame for Raw Stream Data

### 9.1 Understanding H26xFrame Structure

The `H26xFrame` struct is used for handling H.264/H.265 raw stream data:

```go
type H26xFrame struct {
    pkg.Sample
}
```

Key characteristics:
- Inherits from `pkg.Sample` - contains codec context, memory management, and timing
- Uses `Raw.(*pkg.Nalus)` to store NALU (Network Abstraction Layer Unit) data
- Supports both H.264 (AVC) and H.265 (HEVC) formats
- Uses efficient memory allocators for zero-copy operations

### 9.2 Creating H26xFrame for Publishing

```go
import (
    "m7s.live/v5"
    "m7s.live/v5/pkg/format"
    "m7s.live/v5/pkg/util"
    "time"
)

// Create publisher with H26xFrame support
func publishRawH264Stream(streamPath string, h264Frames [][]byte) error {
    // Get publisher
    publisher, err := p.Publish(streamPath)
    if err != nil {
        return err
    }
    
    // Create memory allocator
    allocator := util.NewScalableMemoryAllocator(1 << util.MinPowerOf2)
    defer allocator.Recycle()
    
    // Create writer for H26xFrame
    writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
    
    // Set up H264 codec context
    writer.VideoFrame.ICodecCtx = &format.H264{}
    
    // Publish multiple frames
    // Note: This is a demonstration of multi-frame writing. In actual scenarios, 
    // frames should be written gradually as they are received from the video source.
    startTime := time.Now()
    for i, frameData := range h264Frames {
        // Create H26xFrame for each frame
        frame := writer.VideoFrame
        
        // Set timestamp with proper interval
        frame.Timestamp = startTime.Add(time.Duration(i) * time.Second / 30) // 30 FPS
        
        // Write NALU data
        nalus := frame.GetNalus()
        // if frameData is a single NALU, otherwise need to loop
        p := nalus.GetNextPointer()
        mem := frame.NextN(len(frameData))
        copy(mem, frameData)
        p.PushOne(mem)	
        // Publish frame
        if err := writer.NextVideo(); err != nil {
            return err
        }
    }
    
    return nil
}

// Example usage with continuous streaming
func continuousH264Publishing(streamPath string, frameSource <-chan []byte, stopChan <-chan struct{}) error {
    // Get publisher
    publisher, err := p.Publish(streamPath)
    if err != nil {
        return err
    }
    defer publisher.Dispose()
    
    // Create memory allocator
    allocator := util.NewScalableMemoryAllocator(1 << util.MinPowerOf2)
    defer allocator.Recycle()
    
    // Create writer for H26xFrame
    writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
    
    // Set up H264 codec context
    writer.VideoFrame.ICodecCtx = &format.H264{}
    
    startTime := time.Now()
    frameCount := 0
    
    for {
        select {
        case frameData := <-frameSource:
            // Create H26xFrame for each frame
            frame := writer.VideoFrame
            
            // Set timestamp with proper interval
            frame.Timestamp = startTime.Add(time.Duration(frameCount) * time.Second / 30) // 30 FPS
            
            // Write NALU data
            nalus := frame.GetNalus()
            mem := frame.NextN(len(frameData))
            copy(mem, frameData)
            
            // Publish frame
            if err := writer.NextVideo(); err != nil {
                return err
            }
            
            frameCount++
            
        case <-stopChan:
            // Stop publishing
            return nil
        }
    }
}
```

### 9.3 Processing H26xFrame (Transform Pattern)

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
    // Copy frame metadata
    copyVideo := t.Writer.VideoFrame
    copyVideo.ICodecCtx = video.ICodecCtx
    *copyVideo.BaseSample = *video.BaseSample
    nalus := copyVideo.GetNalus()
    
    // Process each NALU unit
    for nalu := range video.Raw.(*pkg.Nalus).RangePoint {
        p := nalus.GetNextPointer()
        mem := copyVideo.NextN(nalu.Size)
        nalu.CopyTo(mem)
        
        // Example: Filter or modify specific NALU types
        if video.FourCC() == codec.FourCC_H264 {
            switch codec.ParseH264NALUType(mem[0]) {
            case codec.NALU_IDR_Picture, codec.NALU_Non_IDR_Picture:
                // Process video frame NALUs
                // Example: Apply transformations, filters, etc.
            case codec.NALU_SPS, codec.NALU_PPS:
                // Process parameter set NALUs
            }
        } else if video.FourCC() == codec.FourCC_H265 {
            switch codec.ParseH265NALUType(mem[0]) {
            case h265parser.NAL_UNIT_CODED_SLICE_IDR_W_RADL:
                // Process H.265 IDR frames
            }
        }
        
        // Push processed NALU
        p.PushOne(mem)
    }
    
    return t.Writer.NextVideo()
}
```

### 9.4 Common NALU Types for H.264/H.265

#### H.264 NALU Types
```go
const (
    NALU_Non_IDR_Picture = 1  // Non-IDR picture (P-frames)
    NALU_IDR_Picture     = 5  // IDR picture (I-frames)
    NALU_SEI             = 6  // Supplemental enhancement information
    NALU_SPS             = 7  // Sequence parameter set
    NALU_PPS             = 8  // Picture parameter set
)

// Parse NALU type from first byte
naluType := codec.ParseH264NALUType(mem[0])
```

#### H.265 NALU Types
```go
// Parse H.265 NALU type from first byte
naluType := codec.ParseH265NALUType(mem[0])
```

### 9.5 Memory Management Best Practices

```go
// Use memory allocators for efficient operations
allocator := util.NewScalableMemoryAllocator(1 << 20) // 1MB initial size
defer allocator.Recycle()

// When processing multiple frames, reuse the same allocator
writer := m7s.NewPublisherWriter[*format.RawAudio, *format.H26xFrame](publisher, allocator)
```

### 9.6 Error Handling and Validation

```go
func processFrame(video *format.H26xFrame) error {
    // Check codec changes
    if err := video.CheckCodecChange(); err != nil {
        return err
    }
    
    // Validate frame data
    if video.Raw == nil {
        return fmt.Errorf("empty frame data")
    }
    
    // Process NALUs safely
    nalus, ok := video.Raw.(*pkg.Nalus)
    if !ok {
        return fmt.Errorf("invalid NALUs format")
    }
    
    // Process frame...
    return nil
}
```

## 10. Prometheus Integration
Just implement the Collector interface, and the system will automatically collect metrics from all plugins:
```go
func (p *MyPlugin) Describe(ch chan<- *prometheus.Desc) {
  
}

func (p *MyPlugin) Collect(ch chan<- prometheus.Metric) {
  
}
``` 