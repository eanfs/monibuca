# Test Plugin

The Test Plugin is a comprehensive testing framework for Monibuca v5 that provides automated testing capabilities for various streaming protocols and media processing scenarios. It enables developers to validate streaming functionality, performance, and reliability through configurable test cases.

## Overview

The Test Plugin serves as a quality assurance tool for Monibuca's streaming capabilities, offering:

- **Automated Test Cases**: Pre-configured test scenarios for common streaming workflows
- **Multi-Protocol Support**: Testing across RTMP, RTSP, SRT, WebRTC, HLS, MP4, FLV, and PS protocols
- **Stress Testing**: Performance testing with configurable concurrent push/pull operations
- **Real-time Monitoring**: Live status updates via Server-Sent Events (SSE)
- **Flexible Task System**: Extensible task framework for custom test scenarios

## Architecture

### Core Components

1. **TestPlugin**: Main plugin controller managing test cases and stress testing
2. **TestCase**: Individual test scenario with configurable tasks and parameters
3. **TestTaskFactory**: Factory pattern for creating different types of test tasks
4. **Task Types**: Specialized tasks for different operations (push, pull, snapshot, write, read)

### Task System Design

The plugin uses a sophisticated task orchestration system:

```go
type TestTaskConfig struct {
    Action     string        `json:"action"`     // Task type: push, pull, snapshot, write, read
    Delay      time.Duration `json:"delay"`      // Delay before task execution
    Format     string        `json:"format"`     // Protocol/format: rtmp, rtsp, srt, etc.
    ServerAddr string        `json:"serverAddr"` // Target server address
    Input      string        `json:"input"`      // Input source (file, URL, etc.)
    StreamPath string        `json:"streamPath"` // Stream identifier
}
```

### Task Types

#### 1. Push Task (`AcceptPushTask`)
- **Purpose**: Simulates media streaming to the server
- **Implementation**: Uses FFmpeg to push media from files or live sources
- **Supported Formats**: RTMP, RTSP, SRT, WebRTC, PS
- **Design Insight**: Leverages FFmpeg's robust streaming capabilities while maintaining control through process management

#### 2. Snapshot Task (`SnapshotTask`)
- **Purpose**: Captures frames from streams to verify content quality
- **Implementation**: Two modes:
  - Direct FFmpeg capture from stream URLs
  - Internal stream subscription with AnnexB format processing
- **Design Insight**: Dual approach allows testing both external accessibility and internal stream processing

#### 3. Write Task (`WriteRemoteTask`)
- **Purpose**: Records streams to various formats
- **Implementation**: Integrates with Monibuca's recording plugins
- **Supported Formats**: MP4, FLV, HLS, PS
- **Design Insight**: Reuses existing recording infrastructure for consistency

#### 4. Read Task (`ReadRemoteTask`)
- **Purpose**: Pulls streams from various sources
- **Implementation**: Uses Monibuca's puller plugins
- **Supported Formats**: MP4, FLV, HLS, RTMP, RTSP, SRT, WebRTC, AnnexB
- **Design Insight**: Unified interface for different puller implementations

## Key Design Patterns

### 1. Factory Pattern
The `TestTaskFactory` uses a registry pattern to dynamically create tasks:

```go
func (f *TestTaskFactory) Register(action string, taskCreator func(*TestCase, TestTaskConfig) task.ITask) {
    f.tasks[action] = taskCreator
}
```

This allows easy extension with new task types without modifying core code.

### 2. Task Orchestration with Reflection
The test case execution uses Go's reflection API for dynamic task scheduling:

```go
subTaskSelect := []reflect.SelectCase{
    {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(time.After(ts.Timeout))},
    {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ts.Done())},
}
```

This enables complex timing control and parallel task execution.

### 3. Real-time Status Updates
The plugin implements Server-Sent Events (SSE) for live monitoring:

```go
func (p *TestPlugin) GetTestCaseSSE(w http.ResponseWriter, r *http.Request) {
    util.NewSSE(w, r.Context(), func(sse *util.SSE) {
        // Real-time status broadcasting
    })
}
```

### 4. Stress Testing Architecture
The stress testing system maintains collections of active push/pull operations:

```go
type TestPlugin struct {
    pushers util.Collection[string, *m7s.PushJob]
    pullers util.Collection[string, *m7s.PullJob]
}
```

This allows dynamic scaling and management of concurrent operations.

## Configuration

### Test Case Configuration

Test cases are defined in YAML format with the following structure:

```yaml
cases:
  test_name:
    description: "Test description"
    videoCodec: "h264"
    audioCodec: "aac"
    videoOnly: false
    audioOnly: false
    timeout: "30s"
    tasks:
      - action: push
        format: rtmp
        delay: 0s
      - action: snapshot
        format: rtmp
        delay: 5s
```

### Pre-configured Test Cases

The plugin includes comprehensive test scenarios:

- **Protocol Cross-Testing**: RTMP↔RTSP, RTMP↔SRT, RTSP↔SRT
- **File Format Testing**: MP4, FLV, HLS reading and recording
- **Advanced Features**: PS encapsulation, AnnexB processing, WebRTC testing

## API Endpoints

### REST API
- `GET /test/api/cases` - List test cases
- `POST /test/api/cases/execute` - Execute test cases
- `GET /test/api/stress/count` - Get stress test counts
- `POST /test/api/stress/push/{protocol}/{count}` - Start push stress test
- `POST /test/api/stress/pull/{protocol}/{count}` - Start pull stress test

### Server-Sent Events
- `GET /sse/cases` - Real-time test case status updates

### gRPC API
Full gRPC service definition available in `pb/test.proto` with corresponding HTTP gateway endpoints.

## Usage Examples

### Basic Test Execution
```bash
# Execute a specific test case
curl -X POST http://localhost:8080/test/api/cases/execute \
  -H "Content-Type: application/json" \
  -d '{"names": ["rtmp2rtmp"]}'
```

### Stress Testing
```bash
# Start 10 concurrent RTMP pushes
curl -X POST http://localhost:8080/test/api/stress/push/rtmp/10 \
  -H "Content-Type: application/json" \
  -d '{
    "streamPath": "stress_test",
    "remoteURL": "rtmp://localhost/live/%d"
  }'
```

### Real-time Monitoring
```javascript
// Monitor test case status
const eventSource = new EventSource('/sse/cases');
eventSource.onmessage = function(event) {
    const testCases = JSON.parse(event.data);
    // Update UI with test status
};
```

## Advanced Features

### 1. Dynamic Stream Path Generation
The plugin automatically generates unique stream paths for parallel test execution:

```go
if taskConfig.StreamPath == "" {
    taskConfig.StreamPath = fmt.Sprintf("test/%d", ts.ID)
}
```

### 2. Intelligent Input Handling
Supports both file-based and URL-based inputs with automatic path resolution:

```go
if taskConfig.Input != "" && !strings.Contains(taskConfig.Input, ".") {
    taskConfig.Input = fmt.Sprintf("%s/%d", taskConfig.Input, ts.ID)
}
```

### 3. Comprehensive Logging
Each test case maintains detailed logs with timestamps:

```go
func (ts *TestCase) Write(buf []byte) (int, error) {
    ts.Logs += time.Now().Format("2006-01-02 15:04:05") + " " + string(buf) + "\n"
    return len(buf), nil
}
```

## Integration with Monibuca Ecosystem

The Test Plugin seamlessly integrates with Monibuca's plugin architecture:

- **Plugin Registration**: Uses `m7s.InstallPlugin` for automatic discovery
- **Configuration Management**: Leverages Monibuca's configuration system
- **Task Framework**: Built on Monibuca's task management system
- **Stream Management**: Integrates with publisher/subscriber model

## Performance Considerations

### Memory Management
- Uses `gomem.Memory` for efficient buffer management
- Implements proper cleanup for temporary files and processes
- Leverages Go's garbage collector for automatic memory management

### Concurrency
- Thread-safe operations using Monibuca's `Call` method
- Efficient collection management for stress testing
- Non-blocking SSE implementation

### Resource Cleanup
- Automatic process termination on task completion
- Temporary file cleanup after recording tests
- Proper connection management for network operations

## Extensibility

The plugin is designed for easy extension:

1. **New Task Types**: Register new task creators with the factory
2. **Custom Protocols**: Add support for new streaming protocols
3. **Advanced Metrics**: Extend monitoring and reporting capabilities
4. **Integration Tests**: Create complex multi-step test scenarios

## Best Practices

1. **Test Isolation**: Each test case runs with unique stream paths
2. **Timeout Management**: Configure appropriate timeouts for different scenarios
3. **Resource Monitoring**: Monitor system resources during stress testing
4. **Log Analysis**: Use detailed logs for debugging and performance analysis
5. **Incremental Testing**: Start with simple cases before complex scenarios

## Troubleshooting

### Common Issues

1. **FFmpeg Not Found**: Ensure FFmpeg is installed and in PATH
2. **Port Conflicts**: Check for port availability in stress testing
3. **File Permissions**: Ensure write permissions for recording tests
4. **Network Connectivity**: Verify network access for remote stream testing

### Debug Mode
Enable detailed logging by setting appropriate log levels in Monibuca configuration.

## Future Enhancements

- **Visual Test Reports**: HTML-based test result visualization
- **Performance Metrics**: Detailed performance analysis and reporting
- **CI/CD Integration**: Automated testing in continuous integration pipelines
- **Custom Assertions**: User-defined validation rules for test cases
- **Distributed Testing**: Multi-node testing capabilities

---

The Test Plugin represents a sophisticated approach to streaming media testing, combining flexibility, performance, and ease of use to ensure the reliability of Monibuca's streaming capabilities.
