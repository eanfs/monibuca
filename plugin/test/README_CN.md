# 测试插件

测试插件是 Monibuca v5 的综合测试框架，为各种流媒体协议和媒体处理场景提供自动化测试能力。它使开发者能够通过可配置的测试用例来验证流媒体功能、性能和可靠性。

## 概述

测试插件作为 Monibuca 流媒体能力的质量保证工具，提供：

- **自动化测试用例**：为常见流媒体工作流预配置测试场景
- **多协议支持**：支持 RTMP、RTSP、SRT、WebRTC、HLS、MP4、FLV 和 PS 协议的测试
- **压力测试**：通过可配置的并发推拉流操作进行性能测试
- **实时监控**：通过服务器发送事件（SSE）提供实时状态更新
- **灵活的任务系统**：可扩展的任务框架，支持自定义测试场景

## 架构设计

### 核心组件

1. **TestPlugin**：主插件控制器，管理测试用例和压力测试
2. **TestCase**：具有可配置任务和参数的单个测试场景
3. **TestTaskFactory**：用于创建不同类型测试任务的工厂模式
4. **任务类型**：针对不同操作的专业化任务（推流、拉流、截图、录制、读取）

### 任务系统设计

插件使用复杂的任务编排系统：

```go
type TestTaskConfig struct {
    Action     string        `json:"action"`     // 任务类型：push, pull, snapshot, write, read
    Delay      time.Duration `json:"delay"`      // 任务执行前的延迟
    Format     string        `json:"format"`     // 协议/格式：rtmp, rtsp, srt 等
    ServerAddr string        `json:"serverAddr"` // 目标服务器地址
    Input      string        `json:"input"`      // 输入源（文件、URL 等）
    StreamPath string        `json:"streamPath"` // 流标识符
}
```

### 任务类型详解

#### 1. 推流任务（`AcceptPushTask`）
- **目的**：模拟向服务器推送媒体流
- **实现**：使用 FFmpeg 从文件或实时源推送媒体
- **支持格式**：RTMP、RTSP、SRT、WebRTC、PS
- **设计巧思**：利用 FFmpeg 强大的流媒体能力，同时通过进程管理保持控制

#### 2. 截图任务（`SnapshotTask`）
- **目的**：从流中捕获帧以验证内容质量
- **实现**：两种模式：
  - 直接从流 URL 进行 FFmpeg 捕获
  - 内部流订阅与 AnnexB 格式处理
- **设计巧思**：双重方法允许测试外部可访问性和内部流处理

#### 3. 录制任务（`WriteRemoteTask`）
- **目的**：将流录制为各种格式
- **实现**：与 Monibuca 的录制插件集成
- **支持格式**：MP4、FLV、HLS、PS
- **设计巧思**：重用现有录制基础设施以确保一致性

#### 4. 读取任务（`ReadRemoteTask`）
- **目的**：从各种源拉取流
- **实现**：使用 Monibuca 的拉流插件
- **支持格式**：MP4、FLV、HLS、RTMP、RTSP、SRT、WebRTC、AnnexB
- **设计巧思**：为不同拉流器实现提供统一接口

## 关键设计模式

### 1. 工厂模式
`TestTaskFactory` 使用注册表模式动态创建任务：

```go
func (f *TestTaskFactory) Register(action string, taskCreator func(*TestCase, TestTaskConfig) task.ITask) {
    f.tasks[action] = taskCreator
}
```

这允许轻松扩展新任务类型而无需修改核心代码。

### 2. 基于反射的任务编排
测试用例执行使用 Go 的反射 API 进行动态任务调度：

```go
subTaskSelect := []reflect.SelectCase{
    {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(time.After(ts.Timeout))},
    {Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ts.Done())},
}
```

这实现了复杂的时间控制和并行任务执行。

### 3. 实时状态更新
插件实现服务器发送事件（SSE）进行实时监控：

```go
func (p *TestPlugin) GetTestCaseSSE(w http.ResponseWriter, r *http.Request) {
    util.NewSSE(w, r.Context(), func(sse *util.SSE) {
        // 实时状态广播
    })
}
```

### 4. 压力测试架构
压力测试系统维护活跃推拉流操作的集合：

```go
type TestPlugin struct {
    pushers util.Collection[string, *m7s.PushJob]
    pullers util.Collection[string, *m7s.PullJob]
}
```

这允许动态扩展和管理并发操作。

## 配置说明

### 测试用例配置

测试用例以 YAML 格式定义，结构如下：

```yaml
cases:
  test_name:
    description: "测试描述"
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

### 预配置测试用例

插件包含全面的测试场景：

- **协议交叉测试**：RTMP↔RTSP、RTMP↔SRT、RTSP↔SRT
- **文件格式测试**：MP4、FLV、HLS 读取和录制
- **高级功能**：PS 封装、AnnexB 处理、WebRTC 测试

## API 接口

### REST API
- `GET /test/api/cases` - 列出测试用例
- `POST /test/api/cases/execute` - 执行测试用例
- `GET /test/api/stress/count` - 获取压力测试计数
- `POST /test/api/stress/push/{protocol}/{count}` - 开始推流压力测试
- `POST /test/api/stress/pull/{protocol}/{count}` - 开始拉流压力测试

### 服务器发送事件
- `GET /sse/cases` - 实时测试用例状态更新

### gRPC API
完整的 gRPC 服务定义在 `pb/test.proto` 中，包含相应的 HTTP 网关端点。

## 使用示例

### 基本测试执行
```bash
# 执行特定测试用例
curl -X POST http://localhost:8080/test/api/cases/execute \
  -H "Content-Type: application/json" \
  -d '{"names": ["rtmp2rtmp"]}'
```

### 压力测试
```bash
# 启动 10 个并发 RTMP 推流
curl -X POST http://localhost:8080/test/api/stress/push/rtmp/10 \
  -H "Content-Type: application/json" \
  -d '{
    "streamPath": "stress_test",
    "remoteURL": "rtmp://localhost/live/%d"
  }'
```

### 实时监控
```javascript
// 监控测试用例状态
const eventSource = new EventSource('/sse/cases');
eventSource.onmessage = function(event) {
    const testCases = JSON.parse(event.data);
    // 使用测试状态更新 UI
};
```

## 高级功能

### 1. 动态流路径生成
插件自动为并行测试执行生成唯一流路径：

```go
if taskConfig.StreamPath == "" {
    taskConfig.StreamPath = fmt.Sprintf("test/%d", ts.ID)
}
```

### 2. 智能输入处理
支持基于文件和基于 URL 的输入，具有自动路径解析：

```go
if taskConfig.Input != "" && !strings.Contains(taskConfig.Input, ".") {
    taskConfig.Input = fmt.Sprintf("%s/%d", taskConfig.Input, ts.ID)
}
```

### 3. 综合日志记录
每个测试用例维护带时间戳的详细日志：

```go
func (ts *TestCase) Write(buf []byte) (int, error) {
    ts.Logs += time.Now().Format("2006-01-02 15:04:05") + " " + string(buf) + "\n"
    return len(buf), nil
}
```

## 与 Monibuca 生态系统的集成

测试插件与 Monibuca 的插件架构无缝集成：

- **插件注册**：使用 `m7s.InstallPlugin` 进行自动发现
- **配置管理**：利用 Monibuca 的配置系统
- **任务框架**：基于 Monibuca 的任务管理系统构建
- **流管理**：与发布者/订阅者模型集成

## 性能考虑

### 内存管理
- 使用 `util.Memory` 进行高效的缓冲区管理
- 为临时文件和进程实现适当的清理
- 利用 Go 的垃圾收集器进行自动内存管理

### 并发性
- 使用 Monibuca 的 `Call` 方法进行线程安全操作
- 为压力测试进行高效的集合管理
- 非阻塞 SSE 实现

### 资源清理
- 任务完成时自动终止进程
- 录制测试后清理临时文件
- 网络操作的适当连接管理

## 可扩展性

插件设计便于扩展：

1. **新任务类型**：向工厂注册新的任务创建器
2. **自定义协议**：添加对新流媒体协议的支持
3. **高级指标**：扩展监控和报告功能
4. **集成测试**：创建复杂的多步骤测试场景

## 最佳实践

1. **测试隔离**：每个测试用例使用唯一的流路径运行
2. **超时管理**：为不同场景配置适当的超时
3. **资源监控**：在压力测试期间监控系统资源
4. **日志分析**：使用详细日志进行调试和性能分析
5. **增量测试**：在复杂场景之前从简单用例开始

## 故障排除

### 常见问题

1. **找不到 FFmpeg**：确保 FFmpeg 已安装并在 PATH 中
2. **端口冲突**：在压力测试中检查端口可用性
3. **文件权限**：确保录制测试的写入权限
4. **网络连接**：验证远程流测试的网络访问

### 调试模式
通过在 Monibuca 配置中设置适当的日志级别来启用详细日志记录。

## 设计巧思总结

### 1. 任务工厂模式
通过注册表模式实现任务的动态创建，使得添加新任务类型变得非常简单，无需修改核心代码。

### 2. 反射驱动的任务调度
使用 Go 的反射 API 实现复杂的任务编排，支持动态延迟和并行执行，提供了极大的灵活性。

### 3. 双重截图机制
截图任务支持两种模式：外部 FFmpeg 捕获和内部流订阅，既测试了外部可访问性，又验证了内部流处理能力。

### 4. 智能资源管理
通过 `util.Collection` 管理并发操作，支持动态扩展和收缩，实现了高效的压力测试。

### 5. 实时状态同步
使用 SSE 技术实现实时状态更新，为测试监控提供了良好的用户体验。

### 6. 流路径隔离
自动生成唯一的流路径，确保并行测试之间不会相互干扰，提高了测试的可靠性。

## 未来增强

- **可视化测试报告**：基于 HTML 的测试结果可视化
- **性能指标**：详细的性能分析和报告
- **CI/CD 集成**：在持续集成管道中的自动化测试
- **自定义断言**：用户定义的测试用例验证规则
- **分布式测试**：多节点测试能力

---

测试插件代表了流媒体测试的复杂方法，结合了灵活性、性能和易用性，以确保 Monibuca 流媒体能力的可靠性。通过其精心设计的架构和丰富的功能集，它为开发者提供了一个强大的工具来验证和优化流媒体系统。
