# Monibuca v5 项目概述

Monibuca 是一个使用纯 Go 语言开发的、高度可扩展的高性能流媒体服务器开发框架。它旨在提供高并发、低延迟的流媒体处理能力，并支持多种流媒体协议和功能。

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

## 构建与运行

### 前提条件

*   Go 1.23 或更高版本
*   对流媒体协议有基本了解

### 运行默认配置

```bash
cd example/default
go run -tags sqlite main.go
```

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

### Web UI

将 `admin.zip` 文件（不要解压）放在与配置文件相同的目录中。然后访问 http://localhost:8080 即可访问 UI。

## 开发约定

### 项目结构

*   `example/`: 包含各种使用示例
*   `pkg/`: 核心库代码
*   `plugin/`: 各种功能插件
*   `pb/`: Protocol Buffer 生成的代码
*   `doc/`: 项目文档
*   `scripts/`: 脚本文件

### 配置

*   使用 YAML 格式进行配置
*   支持多层级配置覆盖（环境变量 > 配置文件 > 默认值）
*   插件配置通常以插件名小写作为前缀

### 日志

*   使用 `slog` 进行日志记录
*   支持不同日志级别（debug, info, warn, error, trace）
*   插件可以有自己的日志记录器
*   支持 VictoriaMetrics 时序数据库存储日志

### 插件开发

*   插件需要实现 `IPlugin` 接口
*   通过 `InstallPlugin` 函数注册插件
*   插件可以注册 HTTP 处理函数、gRPC 服务等
*   插件可以有自己的配置结构体

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
*   **snap**: 截图功能，支持水印

### 设备接入插件
*   **gb28181**: GB28181 国标设备接入
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
*   **crontab**: 定时任务和计划录像
*   **mcp**: Model Context Protocol 支持
*   **rtp**: RTP 协议处理

## 最新特性

### v5 版本新增功能
*   **WebTransport 支持**: 新增 WebTransport 插件，支持通过 WebTransport 协议进行推拉流
*   **MCP 集成**: 新增 MCP 插件，支持 Model Context Protocol，提供 AI 助手功能
*   **定时任务**: 新增 crontab 插件，支持计划录像和定时任务调度
*   **VictoriaMetrics 日志**: 新增 vmlog 插件，支持将日志存储到 VictoriaMetrics 时序数据库
*   **房间管理**: 增强 room 插件功能，支持多人房间和信令交互
*   **S3 存储**: 增强 mp4 插件，支持 AWS S3 和兼容存储的录制文件存储
*   **级联功能**: 增强 cascade 插件，支持更灵活的分布式部署方案

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

## 第三方插件

*   JT1078 插件: https://github.com/cuteLittleDevil/m7s-jt1078

## 许可证

基于 AGPL 许可证分发，详见 `LICENSE` 文件。