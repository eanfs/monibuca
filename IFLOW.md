# Monibuca v5 项目概述

Monibuca 是一个使用纯 Go 语言开发的高度可扩展高性能流媒体服务器开发框架。它采用插件化架构设计，旨在提供高并发、低延迟的流媒体处理能力，支持多种流媒体协议和丰富的扩展功能。

## 核心特性

*   **高性能**: 采用无锁设计、部分手动内存管理和多核计算
*   **低延迟**: 实现零等待转发，全链路亚秒级延迟
*   **模块化**: 按需加载，无限扩展性
*   **灵活性**: 高度可配置，适应各种流媒体场景
*   **可扩展性**: 支持分布式部署，轻松应对大规模场景
*   **调试友好**: 内置调试插件，实时性能监控与分析
*   **媒体处理**: 支持截图、转码、SEI 数据处理
*   **集群能力**: 内置级联和房间管理功能
*   **预览功能**: 支持视频预览、多屏预览、自定义屏幕布局
*   **安全性**: 提供加密传输和流认证
*   **性能监控**: 支持压力测试和性能指标收集（集成在测试插件中）
*   **日志管理**: 日志轮转、自动清理、自定义扩展，支持 VictoriaMetrics 时序数据库存储
*   **录制与回放**: 支持 MP4、HLS、FLV 格式，支持倍速、寻址、暂停，兼容 S3 存储
*   **动态时移**: 动态缓存设计，支持直播时移回放
*   **远程调用**: 支持 gRPC 接口，实现跨语言集成
*   **流别名**: 支持动态流别名，灵活的多流管理
*   **AI 能力**: 集成推理引擎，支持 ONNX 模型，支持自定义前后处理
*   **WebHook**: 订阅流生命周期事件，用于业务系统集成
*   **私有协议**: 支持自定义私有协议以满足特殊业务需求
*   **定时任务**: 支持 crontab 计划录像和任务调度
*   **MCP 集成**: 支持 Model Context Protocol，提供 AI 助手功能
*   **WebTransport**: 支持 WebTransport 协议进行推拉流
*   **S3 存储**: 集成云存储支持，支持 AWS S3、阿里云 OSS、腾讯云 COS 等
*   **抓包功能**: 支持服务器网络抓包，便于网络诊断和调试

## 支持的协议

*   RTMP
*   RTSP  
*   HTTP-FLV
*   WS-FLV
*   HLS
*   WebRTC
*   GB28181
*   ONVIF
*   SRT
*   WebTransport

## 技术架构

Monibuca 基于插件化架构设计，核心功能通过插件扩展。主要组件包括：

*   **Server**: 核心服务器，负责管理流、插件、任务等
*   **Plugin**: 插件系统，提供各种功能扩展
*   **Publisher**: 流发布者，负责接收和管理流数据
*   **Subscriber**: 流订阅者，负责消费流数据
*   **Task**: 任务系统，用于管理异步任务和生命周期
*   **Config**: 配置系统，支持多层级配置（环境变量 > 配置文件 > 默认值）

### 内存管理
- 采用 `gomem.ScalableMemoryAllocator` 实现高效内存分配
- 支持零拷贝操作和自动内存回收
- 内存池设计减少 GC 压力

### 数据流处理
- 基于泛型的音视频数据处理管道
- 支持 H.264/H.265 原始流数据处理
- 模块化的编解码器抽象接口

## 构建与运行

### 前提条件

*   Go 1.24 或更高版本
*   对流媒体协议有基本了解

### 运行默认配置

```bash
cd example/default
go run -tags sqlite main.go
```

### Web UI

将 `admin.zip` 文件（不要解压）放在与配置文件相同的目录中。然后访问 http://localhost:8080 即可访问 UI。

### 构建标签

可以使用以下构建标签来自定义构建：

| 构建标签 | 描述 |
| :--- | :--- |
| `disable_rm` | 禁用内存池 |
| `sqlite` | 启用 sqlite DB |
| `sqliteCGO` | 启用 sqlite cgo 版本 DB |
| `mysql` | 启用 mysql DB |
| `postgres` | 启用 postgres DB |
| `duckdb` | 启用 duckdb DB |
| `taskpanic` | 抛出 panic，用于测试 |
| `fasthttp` | 启用 fasthttp 服务器而不是 net/http |
| `enable_buddy` | 开启 buddy 内存预申请 |

## 开发约定

### 项目结构

*   `example/`: 包含各种使用示例（按端口分类：8080、8081、8082等）
*   `pkg/`: 核心库代码
*   `plugin/`: 各种功能插件
*   `pb/`: Protocol Buffer 生成的代码
*   `doc/`: 项目文档
*   `scripts/`: 脚本文件
*   `test/`: 集成测试

### 配置

*   使用 YAML 格式进行配置
*   支持多层级配置覆盖（环境变量 > 配置文件 > 默认值）
*   插件配置通常以插件名小写作为前缀
*   支持配置项中使用 `-`、`_` 和大写字母

### 日志

*   使用 `slog` 进行日志记录
*   支持不同日志级别（debug, info, warn, error, trace）
*   插件可以有自己的日志记录器
*   支持 VictoriaMetrics 时序数据库存储日志
*   控制台输出使用 `console-slog` 格式化

### 插件开发

*   插件需要实现 `IPlugin` 接口
*   通过 `InstallPlugin` 函数注册插件
*   插件可以注册 HTTP 处理函数、gRPC 服务等
*   插件可以有自己的配置结构体
*   支持插件生命周期管理（启动、停止、销毁）
*   集成 Prometheus 指标收集

### 任务系统

*   使用 `task` 包管理异步任务
*   任务具有生命周期管理（启动、停止、销毁）
*   任务可以有父子关系，形成任务树
*   支持任务重试机制

### 测试

*   使用 Go 标准测试包 `testing`
*   在 `test/` 目录下编写集成测试
*   使用 `example/test` 目录进行功能测试

## 插件列表

### 核心协议插件
*   **rtmp**: RTMP 协议支持
*   **rtsp**: RTSP 协议支持
*   **hls**: HLS 协议支持
*   **webrtc**: WebRTC 协议支持
*   **flv**: FLV 格式支持
*   **srt**: SRT 协议支持
*   **webtransport**: WebTransport 协议支持

### 媒体处理插件
*   **mp4**: MP4 录制和回放，支持 S3 存储
*   **transcode**: 转码功能
*   **sei**: SEI 数据处理
*   **snap**: 截图功能，支持水印和批量抓图

### 设备接入插件
*   **gb28181**: GB28181 国标设备接入，支持平台配置和子码流
*   **onvif**: ONVIF 设备发现和接入
*   **hiksdk**: 海康 SDK 接入

### 管理功能插件
*   **debug**: 调试和性能监控
*   **logrotate**: 日志轮转
*   **vmlog**: VictoriaMetrics 日志存储
*   **preview**: 视频预览和多屏显示
*   **room**: 房间管理功能
*   **test**: 压力测试和性能测试

### 系统集成插件
*   **cascade**: 级联功能，支持分布式部署
*   **crontab**: 定时任务和计划录像，支持任务状态监控
*   **mcp**: Model Context Protocol 支持，提供 AI 助手功能
*   **rtp**: RTP 协议处理

## 核心功能详解

### MCP 集成 (Model Context Protocol)
- 提供系统信息查询接口
- 支持流列表和流详情查询
- 集成截图功能
- 支持配置文件读取
- 通过 SSE 协议提供实时通信

### 定时任务系统 (Crontab)
- 支持计划录像任务管理
- 提供任务状态实时监控
- 支持多种时间格式和计划模式
- 集成数据库存储和恢复
- 支持任务优先级和依赖关系

### WebTransport 协议
- 支持通过 WebTransport 进行推拉流
- 兼容 HTTP/3 和 QUIC 协议
- 提供更好的实时性和低延迟

### S3 存储集成
- 支持 AWS S3、阿里云 OSS、腾讯云 COS 等
- 支持多种认证方式和端点配置
- 集成文件上传和下载功能
- 支持预签名 URL 和直传

### GB28181 协议增强
- 支持平台和通道配置管理
- 支持子码流播放
- 优化 SDP 处理逻辑
- 支持 Pull Proxy 代理模式

### 抓包功能
- 集成 tcpdump 网络诊断工具
- 支持 TCP 和 UDP 协议抓包
- 提供 Web API 接口进行抓包控制
- 支持抓包结果下载和分析

## 最新特性 (v5.0.x)

### v5.0.4 新增功能
*   **GB28181**: 支持更新 channelName / channelId
*   **定时任务**: 初始化 SQL 支持
*   **Snap 插件**: 支持批量抓图
*   **管理后台**: 支持自定义首页
*   **推/拉代理**: 支持可选参数更新
*   **心跳/脉冲**: pulse interval 允许为 0
*   **告警上报**: 通过 Hook 发送报警和告警信息

### v5.0.3 新增功能
*   **录像优化**: MP4/FLV 录像拉取、分片、写入、格式转换等多项修复和优化
*   **GB28181增强**: 支持pullproxy代理GB28181流，完善平台配置、子码流播放等
*   **插件系统**: 插件初始化、配置加载、数据库适配等增强
*   **crontab计划录像**: 支持定时任务插件计划录像，拉流代理支持禁用

### v5.0.2 新增功能
*   **降低延迟**: 禁用TCP WebRTC的重放保护功能
*   **S3存储**: 新增S3存储插件，支持云存储集成
*   **定时任务**: 新增crontab定时任务插件
*   **抓包功能**: 新增服务器抓包功能
*   **MP4循环读取**: 支持MP4文件循环读取功能
*   **TCP配置**: 新增TCP连接读写缓冲区配置选项

### v5.0.x 架构优化
*   **配置系统**: 支持更多配置格式，提升配置灵活性
*   **内存管理**: 采用新的内存分配器和零拷贝操作
*   **数据处理**: 基于泛型的音视频数据处理管道
*   **插件系统**: 增强插件生命周期管理和指标收集

## 监控和运维

### Prometheus 监控
Monibuca 内置 Prometheus 指标支持，配置示例：

```yaml
scrape_configs:
  - job_name: "monibuca"
    metrics_path: "/api/metrics"
    static_configs:
      - targets: ["localhost:8080"]
```

### 健康检查
*   HTTP API: `http://localhost:8080/api/summary`
*   系统信息: `http://localhost:8080/api/sysinfo`
*   流列表: `http://localhost:8080/api/stream/list`
*   配置文件: `http://localhost:8080/api/config`

### Web API 端点
*   `/api/stream/list` - 获取流列表
*   `/api/stream/info/{streamPath}` - 获取流详情
*   `/api/snap/{streamPath}` - 获取流截图
*   `/api/tcpdump` - 抓包控制接口
*   `/mcp` - MCP 服务端点

## 配置示例

### 多端口配置
```yaml
# 8080 端口 - 核心服务
global:
  http:
    listenaddr: :8080

# 8081 端口 - 级联客户端
cascadeclient:
  server: localhost:44944

# 8082 端口 - 设备管理
gb28181:
  sip:
    listenaddr:
      - udp::5060
```

### S3 存储配置
```yaml
storage:
  s3:
    region: "us-east-1"
    endpoint: "storage-dev.xiding.tech"
    accessKeyID: "your-access-key-id"
    secretAccessKey: "your-secret-access-key"
    bucket: "your-bucket-name"
    pathPrefix: "recordings"
    forcePathStyle: true
    useSSL: true
```

### 定时任务配置
```yaml
crontab:
  enable: true
  plans:
    - name: "夜间录像"
      plan: "0 20-7 * * *"
      streams:
        - streamPath: "live/camera1"
          filepath: "recordings/camera1_$date"
          fragment: 1h
```

## 第三方插件

*   JT1078 插件: https://github.com/cuteLittleDevil/m7s-jt1078

## 开发和调试工具

### gRPC 工具安装
```bash
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
```

### 开发环境推荐
- Visual Studio Code
- Goland
- Cursor
- CodeBuddy
- Trae
- Qoder
- Claude Code
- Kiro
- Windsurf

## 许可证

基于 AGPL 许可证分发，详见 `LICENSE` 文件。

## 版本历史

- **v5.0.4** (2025-08-15): 增强GB28181、crontab、snap等功能
- **v5.0.3** (2025-06-27): 录像优化、GB28181增强、插件系统改进
- **v5.0.2** (2025-06-05): 新增S3存储、MCP、抓包、定时任务等功能
- **v5.0.x**: 基于新架构重构，支持插件化扩展和现代Go特性