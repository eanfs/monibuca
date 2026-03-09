# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Monibuca is a high-performance streaming server framework written in Go. It's designed to be a modular, scalable platform for real-time audio/video streaming with support for multiple protocols including RTMP, RTSP, HLS, WebRTC, GB28181, and more. Key features include:

- 🚀 High Performance - Lock-free design, partial manual memory management, multi-core computing
- ⚡ Low Latency - Zero-wait forwarding, sub-second latency across the entire chain
- 📦 Modular - Load on demand, unlimited extensibility via plugins
- 🔧 Flexible - Highly configurable to meet various streaming scenarios
- 💪 Scalable - Supports distributed deployment, easily handles large-scale scenarios
- 🔍 Debug Friendly - Built-in debug plugin, real-time performance monitoring and analysis

## Development Commands

### Building and Running

**Basic Run (with SQLite):**
```bash
cd example/default
go run -tags sqlite main.go
```

**Build Tags:**
- `sqlite` - Enable SQLite database support
- `sqliteCGO` - Enable SQLite with CGO
- `mysql` - Enable MySQL database support
- `postgres` - Enable PostgreSQL database support
- `duckdb` - Enable DuckDB database support
- `disable_rm` - Disable memory pool
- `fasthttp` - Use fasthttp instead of net/http
- `taskpanic` - Enable panics for testing
- `enable_buddy` - Enable buddy memory pre-allocation

**Protocol Buffer Generation:**
```bash
# Generate all proto files
sh scripts/protoc.sh

# Generate specific plugin proto
sh scripts/protoc.sh plugin_name
```

Windows:
```powershell
.\scripts\protoc.bat
.\scripts\protoc.bat plugin_name
```

**Release Building:**
```bash
# Uses goreleaser configuration
goreleaser build

# Build directly with custom tags
go build -tags "sqlite mysql postgres" -o monibuca main.go
```

**Testing:**
```bash
# Run all tests
go test ./...

# Run tests with coverage
go test -cover ./...

# Run tests for specific package
go test -v ./plugin/rtmp

# Run tests with race detector
go test -race ./...
```

## Architecture Overview

### Project Structure

```
/
├── example/              # Example configurations and entry points
│   ├── default/          # Default configuration (main entry point)
│   ├── custom/           # Custom configuration example
│   └── multiple/         # Multiple server instance example
├── plugin/               # Built-in plugins
│   ├── rtmp/             # RTMP protocol support
│   ├── rtsp/             # RTSP protocol support
│   ├── webrtc/           # WebRTC protocol support
│   ├── hls/              # HLS protocol support
│   ├── flv/              # FLV protocol support
│   ├── mp4/              # MP4 recording with post-processing
│   ├── gb28181/          # GB28181 Chinese surveillance standard
│   ├── srt/              # SRT protocol support
│   ├── debug/            # Debug and profiling tools
│   ├── cluster/          # Cluster management
│   └── ... (other plugins)
├── pkg/                  # Core packages
│   ├── config/           # Configuration system
│   ├── db/               # Database integration (SQLite, MySQL, PostgreSQL, DuckDB)
│   ├── util/             # Utility functions
│   ├── codec/            # Audio/video codec handling
│   ├── format/           # Media format parsing (PS, TS, etc.)
│   └── storage/          # Storage abstraction layer
├── pb/                   # Protocol buffer definitions
├── server.go             # Server core implementation
├── plugin.go             # Plugin system implementation
├── publisher.go          # Publisher (input stream) management
├── subscriber.go         # Subscriber (output stream) management
└── recoder.go            # Recording management
```

### Core Components

**Server (`server.go`):** Main server instance that manages plugins, streams, and configurations. Implements the central event loop and lifecycle management. Key responsibilities:
- Plugin initialization and management
- Stream lifecycle management
- HTTP/gRPC API serving
- Database integration
- Configuration loading and merging

**Plugin System (`plugin.go`):** Modular architecture where functionality is provided through plugins. Each plugin embeds `Plugin` struct and can optionally implement:
- `ITCPPlugin` - TCP server protocol handler
- `IUDPPlugin` - UDP server protocol handler
- `IQUICPlugin` - QUIC server protocol handler
- `IRegisterHandler` - Custom HTTP endpoint registration
- `IPublishHookPlugin` - Stream publishing hooks
- `ISubscribeHookPlugin` - Stream subscription hooks

Plugins are registered using `InstallPlugin[YourPluginType](meta)` and auto-discovered via imports.

**Configuration System (`pkg/config/`):** Hierarchical configuration system with priority order: dynamic modifications > environment variables > config files > default YAML > global config > defaults. Supports:
- Structured configuration with struct tags
- Environment variable overrides (uppercase plugin name prefix)
- YAML-based config files
- Runtime config updates via API

**Task System (github.com/langhuihui/gotask):** Advanced asynchronous task management system with multiple layers:
- **Task:** Basic unit of work with lifecycle management (Start/Run/Dispose)
- **Job:** Container that manages multiple child tasks and provides event loops
- **Work:** Special type of Job that acts as a persistent queue manager (keepalive=true)
- **TickTask:** Periodic task execution

The server uses `task.RootManager` to manage the entire task hierarchy.

### Task System Deep Dive

#### Task Hierarchy and Lifecycle
```
Work (Queue Manager)
  └── Job (Container with Event Loop)
      └── Task (Basic Work Unit)
          ├── Start() - Initialization phase
          ├── Run() - Main execution phase
          └── Dispose() - Cleanup phase
```

#### Queue-based Asynchronous Processing
The Task system supports sophisticated queue-based processing patterns:

1. **Work as Queue Manager:** Work instances stay alive indefinitely and manage queues of tasks
2. **Task Queuing:** Use `workInstance.AddTask(task, logger)` to queue tasks
3. **Automatic Lifecycle:** Tasks are automatically started, executed, and disposed
4. **Error Handling:** Built-in retry mechanisms and error propagation

**Example Pattern (from S3 plugin):**
```go
type UploadQueueTask struct {
    task.Work  // Persistent queue manager
}

type FileUploadTask struct {
    task.Task  // Individual work item
    // ... task-specific fields
}

// Initialize queue manager (typically in init())
var uploadQueueTask UploadQueueTask
m7s.Servers.AddTask(&uploadQueueTask)

// Queue individual tasks
uploadQueueTask.AddTask(&FileUploadTask{...}, logger)
```

#### Cross-Plugin Task Cooperation
Tasks can coordinate across different plugins through:

1. **Global Instance Pattern:** Plugins expose global instances for cross-plugin access
2. **Event-based Triggers:** One plugin triggers tasks in another plugin
3. **Shared Queue Managers:** Multiple plugins can use the same Work instance

**Example (MP4 → S3 Integration):**
```go
// In MP4 plugin: trigger S3 upload after recording completes
s3plugin.TriggerUpload(filePath, deleteAfter)

// S3 plugin receives trigger and queues upload task
func TriggerUpload(filePath string, deleteAfter bool) {
    if s3PluginInstance != nil {
        s3PluginInstance.QueueUpload(filePath, objectKey, deleteAfter)
    }
}
```

### Key Interfaces

**Publisher:** Handles incoming media streams and manages track information
**Subscriber:** Handles outgoing media streams to clients  
**Puller:** Pulls streams from external sources
**Pusher:** Pushes streams to external destinations
**Transformer:** Processes/transcodes media streams
**Recorder:** Records streams to storage

### Stream Processing Flow

1. **Publisher** receives media data and creates tracks
2. **Tracks** handle audio/video data with specific codecs
3. **Subscribers** attach to publishers to receive media
4. **Transformers** can process streams between publishers and subscribers
5. **Plugins** provide protocol-specific implementations

### Post-Recording Workflow

Monibuca implements a sophisticated post-recording processing pipeline:

1. **Recording Completion:** MP4 recorder finishes writing stream data
2. **Trailer Writing:** Asynchronous task moves MOOV box to file beginning for web compatibility
3. **File Optimization:** Temporary file operations ensure atomic updates
4. **External Storage Integration:** Automatic upload to S3-compatible services
5. **Cleanup:** Optional local file deletion after successful upload

This workflow uses queue-based task processing to avoid blocking the main recording pipeline.

## Plugin Development

### Creating a Plugin

1. Define your plugin struct with embedded `Plugin`:
```go
type MyPlugin struct {
    Plugin
    // Custom configuration fields
    MyConfig string `default:"value" desc:"Description of config"`
}
```

2. Define plugin metadata using `PluginMeta`:
```go
var MyPluginMeta = PluginMeta{
    Name:        "MyPlugin",
    Version:     "1.0.0",
    DefaultYaml: DefaultYaml(`# My plugin default config`),
    NewPuller:   NewMyPuller,  // Optional
    NewPusher:   NewMyPusher,  // Optional
    NewRecorder: NewMyRecorder, // Optional
    // ... other fields
}
```

3. Register with `InstallPlugin[MyPlugin](MyPluginMeta)` in `init()`

4. Optionally implement protocol-specific interfaces:
   - `ITCPPlugin` for TCP servers: `OnTCPConnect(conn net.Conn) task.ITask`
   - `IUDPPlugin` for UDP servers: `OnUDPConnect(conn *net.UDPConn) task.ITask`
   - `IQUICPlugin` for QUIC servers: `OnQUICConnect(*quic.Conn) task.ITask`
   - `IRegisterHandler` for HTTP endpoints: `RegisterHandler() map[string]http.HandlerFunc`
   - `IPublishHookPlugin` - `OnPublish(pub *Publisher)`
   - `ISubscribeHookPlugin` - `OnSubscribe(streamPath string, args url.Values)`

### Plugin Lifecycle

1. **Init:** Configuration parsing and initialization (called by framework)
2. **Start:** Network listeners and task registration (implement in your plugin)
3. **Run:** Active operation (event loop managed by task system)
4. **Dispose:** Cleanup and shutdown (implement `OnDispose` hook if needed)

### Plugin Example Structure

See existing plugins in `plugin/` directory for reference patterns:
- `plugin/rtmp/` - TCP protocol example
- `plugin/webrtc/` - Complex plugin with multiple interfaces
- `plugin/mp4/` - Recorder plugin with post-processing

### Entry Point and Plugin Importing

Plugins are imported in the main entry point (see `example/default/main.go`). To enable a plugin, add its import:

```go
import (
    _ "m7s.live/v5/plugin/rtmp"   // Enables RTMP plugin
    _ "m7s.live/v5/plugin/webrtc" // Enables WebRTC plugin
    // ... more plugins
)
```

The blank import triggers the plugin's `init()` function which registers it with the framework.

### Cross-Plugin Communication

**Global Instance Pattern:** Plugins can expose global instances for cross-plugin access:
```go
var myPluginInstance *MyPlugin

func (p *MyPlugin) Start() error {
    myPluginInstance = p
    // ...
}

// Exported API
func DoSomething() {
    if myPluginInstance != nil {
        // Use myPluginInstance
    }
}
```

**Event-Based:** Use webhook system or task queues for decoupled communication.

### Configuration Best Practices

- Use struct tags for default values and descriptions: `default:"value" desc:"Description"`
- Config field names are mapped to YAML keys (case-insensitive, hyphens/underscores stripped)
- Environment variables override config: `PLUGINNAME_CONFIGKEY=value`
- Use `s.Config.Parse(&s.MyConfig, "PLUGINNAME")` for custom config parsing

### Cross-Plugin Communication Patterns

#### 1. Global Instance Pattern
```go
// Expose global instance for cross-plugin access
var s3PluginInstance *S3Plugin

func (p *S3Plugin) Start() error {
    s3PluginInstance = p  // Set global instance
    // ... rest of start logic
}

// Provide public API functions
func TriggerUpload(filePath string, deleteAfter bool) {
    if s3PluginInstance != nil {
        s3PluginInstance.QueueUpload(filePath, objectKey, deleteAfter)
    }
}
```

#### 2. Event-Driven Integration
```go
// In one plugin: trigger event after completion
if t.filePath != "" {
    t.Info("MP4 file processing completed, triggering S3 upload")
    s3plugin.TriggerUpload(t.filePath, false)
}
```

#### 3. Shared Queue Managers
Multiple plugins can share Work instances for coordinated processing.

### Asynchronous Task Development Best Practices

#### 1. Implement Task Interfaces
```go
type MyTask struct {
    task.Task
    // ... custom fields
}

func (t *MyTask) Start() error {
    // Initialize resources, validate inputs
    return nil
}

func (t *MyTask) Run() error {
    // Main work execution
    // Return task.ErrTaskComplete for successful completion
    return nil
}
```

#### 2. Use Work for Queue Management
```go
type MyQueueManager struct {
    task.Work
}

var myQueue MyQueueManager

func init() {
    m7s.Servers.AddTask(&myQueue)
}

// Queue tasks from anywhere
myQueue.AddTask(&MyTask{...}, logger)
```

#### 3. Error Handling and Retry
- Tasks automatically support retry mechanisms
- Use `task.SetRetry(maxRetry, interval)` for custom retry behavior
- Return `task.ErrTaskComplete` for successful completion
- Return other errors to trigger retry or failure handling

## Configuration Structure

### Global Configuration
- HTTP/TCP/UDP/QUIC listeners
- Database connections (SQLite, MySQL, PostgreSQL, DuckDB)
- Authentication settings
- Admin interface settings
- Global stream alias mappings

### Plugin Configuration
Each plugin can define its own configuration structure that gets merged with global settings.

## Database Integration

Supports multiple database backends:
- **SQLite:** Default lightweight option
- **MySQL:** Production deployments
- **PostgreSQL:** Production deployments  
- **DuckDB:** Analytics use cases

Automatic migration is handled for core models including users, proxies, and stream aliases.

## Protocol Support

### Built-in Plugins
- **RTMP:** Real-time messaging protocol
- **RTSP:** Real-time streaming protocol
- **HLS:** HTTP live streaming
- **WebRTC:** Web real-time communication
- **GB28181:** Chinese surveillance standard
- **FLV:** Flash video format
- **MP4:** MPEG-4 format with post-processing capabilities
- **SRT:** Secure reliable transport
- **S3:** File upload integration with AWS S3/MinIO compatibility

## Authentication & Security

- JWT-based authentication for admin interface
- Stream-level authentication with URL signing
- Role-based access control (admin/user)
- Webhook support for external auth integration

## Development Guidelines

### Code Style
- Follow existing patterns and naming conventions
- Use the task system for async operations
- Implement proper error handling and logging
- Use the configuration system for all settings

### Testing
- Unit tests should be placed alongside source files
- Integration tests can use the example configurations
- Use the mock.py script for protocol testing

### Async Task Development
- Always use Work instances for queue management
- Implement proper Start/Run lifecycle in tasks
- Use global instance pattern for cross-plugin communication
- Handle errors gracefully with appropriate retry strategies

### Performance Considerations
- Memory pool is enabled by default (disable with `disable_rm`)
- Zero-copy design for media data where possible
- Lock-free data structures for high concurrency
- Efficient buffer management with ring buffers
- Queue-based processing prevents blocking main threads

## Debugging & Monitoring

### Built-in Debug Plugin
The `debug` plugin provides extensive debugging capabilities:
- **Performance Metrics:** Real-time server metrics via Prometheus endpoint (`/api/metrics`)
- **pprof Integration:** CPU and memory profiling (`/debug/pprof/`)
- **Trace View:** Visual task execution timeline
- **Heap Analysis:** Memory allocation tracking
- **GC Stats:** Garbage collection metrics
- **Connection Tracking:** Detailed info on all network connections

**Enable debug plugin in main.go:**
```go
import _ "m7s.live/v5/plugin/debug"
```

### Logging
- **Structured Logging:** Uses slog with JSON format for structured analysis
- **Log Rotation:** Built-in support via `logrotate` plugin
- **Fatal Crash Logging:** Crashes are automatically logged to `fatal/` directory with stack traces
- **Configurable Levels:** Debug, Info, Warn, Error, Fatal (default: Info)
- **Multi-Output:** Supports console, file, and custom outputs

### Task System Debugging
- Tasks automatically include detailed logging with task IDs and types
- Task descriptions dynamically update with progress information
- Event loop status and queue lengths are logged automatically
- Use `task.Debug/Info/Warn/Error` methods for consistent logging
- Monitor task retry counts and failure reasons in logs

### Prometheus Monitoring
Add this to your Prometheus config to scrape metrics:
```yaml
scrape_configs:
  - job_name: "monibuca"
    metrics_path: "/api/metrics"
    static_configs:
      - targets: ["localhost:8080"]
```

## Web Admin Interface

- **Web-based UI:** Served from `admin.zip` file (automatically loaded if present)
- **API Documentation:** Swagger UI available at `/swagger/`
- **RESTful API:** Complete API for all server operations at `/api/`
- **Real-time Monitoring:** Dashboard showing streams, subscribers, and system status
- **Configuration Management:** Edit server and plugin configs via UI
- **User Management:** Role-based access control (admin/user roles)
- **Stream Playback:** Preview streams directly in the browser

To enable login mechanism:
```yaml
global:
  admin:
    enableLogin: true
    users:
      - username: admin
        password: admin
        role: admin
```

## Deployment

### Docker Deployment

**Build from source:**
```bash
docker build -t monibuca .
docker run -p 8080:8080 -p 1935:1935 monibuca
```

**Using Docker Compose:**
```yaml
version: '3.8'
services:
  monibuca:
    build: .
    ports:
      - "8080:8080"   # HTTP API
      - "1935:1935"   # RTMP
      - "554:554"     # RTSP
    volumes:
      - ./config.yaml:/config.yaml
      - ./records:/records
    environment:
      - TZ=Asia/Shanghai
```

### Production Considerations

**Performance Tuning:**
- Enable memory pool (default enabled)
- Use `enable_buddy` tag for memory pre-allocation
- Tune file descriptor limits (ulimit -n 65535)
- Adjust GOMAXPROCS (defaults to CPU count)

**Security:**
- Always enable authentication in production
- Use HTTPS/WSS for secure communication
- Set strong passwords for admin users
- Restrict API access to trusted networks

**Reliability:**
- Use process manager (systemd, supervisor)
- Enable log rotation
- Set up monitoring and alerting
- Regularly update to latest version

## Common Issues & Troubleshooting

### Port Conflicts
- Default HTTP port: 8080
- Default gRPC port: 50051
- Default RTMP port: 1935
- Default RTSP port: 554
- Check plugin-specific port configurations in `config.yaml`

### Database Connection
- Ensure proper build tags for database support
- Check DSN format (examples in `config.yaml`)
- Verify database server is reachable
- Check database user permissions and credentials
- For SQLite, ensure write permissions to database file location

### Plugin Loading
- Plugins are auto-discovered from imports (must be imported in main.go)
- Check plugin enable/disable status (env var: `PLUGINNAME_ENABLE=false`)
- Verify plugin is listed in imports
- Check configuration merging in logs (level: Debug)

### Task System Issues
- Ensure Work instances are added to server during initialization
- Check task queue status if tasks aren't executing (debug plugin UI)
- Verify proper error handling in task implementation
- Monitor task retry counts and failure reasons in logs
- Check that task's `Start()` method completes successfully

### Stream Publishing/Subscribing
- Check stream path format (alphanumeric with optional slashes)
- Verify authentication (check secret and expire parameters)
- Check codec compatibility (H.264/H.265 for video, AAC/MP3 for audio)
- Monitor network connectivity between publisher and server

### Performance Issues
- Check CPU and memory usage via debug plugin
- Look for memory leaks in pprof heap dump
- Check for blocked goroutines in pprof goroutine dump
- Verify no long-running tasks are blocking event loops
- Check network bandwidth and latency
