# RTSP 插件

RTSP 插件为 Monibuca 提供完整的 RTSP 服务器和客户端功能，支持 RTSP 流的发布、播放和代理。

## 功能特性

- **RTSP 服务器**：接受 RTSP 客户端连接，支持流发布和播放
- **RTSP 客户端**：从远程 RTSP 源拉取流
- **双传输模式**：支持 TCP 和 UDP 两种传输协议
- **身份认证**：内置用户名/密码认证功能
- **双向代理**：支持拉流和推流代理
- **标准兼容**：实现 RTSP 协议标准 (RFC 2326/RFC 7826)

## 配置说明

```yaml
rtsp:
  tcp:
    listenaddr: :554        # RTSP 服务器监听地址
  username: ""              # 认证用户名（可选）
  password: ""              # 认证密码（可选）
  udpport: 20001-30000      # 媒体传输的 UDP 端口范围
```

### 配置参数

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `tcp.listenaddr` | string | `:554` | RTSP 服务器监听地址和端口 |
| `username` | string | `""` | 认证用户名（为空表示不启用认证）|
| `password` | string | `""` | 认证密码 |
| `udpport` | range | `20001-30000` | RTP/RTCP 传输使用的 UDP 端口范围 |

## 使用方法

### 拉取 RTSP 流

将远程 RTSP 流拉取到 Monibuca：

```yaml
rtsp:
  pull:
    camera1:
      url: rtsp://admin:password@192.168.1.100/stream
```

或使用统一的 API：
```bash
curl -X POST http://localhost:8080/api/stream/pull \
  -H "Content-Type: application/json" \
  -d '{
    "protocol": "rtsp",
    "streamPath": "camera1",
    "remoteURL": "rtsp://admin:password@192.168.1.100/stream"
  }'
```

### 通过 RTSP 推流

将 Monibuca 中的流推送到远程 RTSP 服务器：

```yaml
rtsp:
  push:
    camera1:
      target: rtsp://192.168.1.200/live/stream
```

### 向 Monibuca RTSP 服务器发布流

使用 FFmpeg 或其他 RTSP 客户端发布流：

```bash
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://localhost:554/live/stream
```

### 从 Monibuca RTSP 服务器播放流

使用任何 RTSP 客户端播放流：

```bash
ffplay rtsp://localhost:554/live/stream
```

或使用 VLC、OBS 等任何支持 RTSP 的播放器。

## 传输模式

### TCP 传输（交织模式）

- 更可靠，可穿透防火墙
- 延迟较高
- 自动回退选项

### UDP 传输

- 延迟更低
- 适合局域网环境
- 需要开放 UDP 端口范围
- RTP/RTCP 使用独立端口

## 支持的 RTSP 方法

| 方法 | 方向 | 说明 |
|------|------|------|
| OPTIONS | 双向 | 查询支持的方法 |
| DESCRIBE | 拉流 | 获取流的 SDP 信息 |
| ANNOUNCE | 推流 | 声明要发布的流 |
| SETUP | 双向 | 设置传输参数 |
| PLAY | 拉流 | 开始播放流 |
| RECORD | 推流 | 开始录制/发布 |
| TEARDOWN | 双向 | 关闭连接 |

## 身份认证

配置用户名和密码后，服务器将要求 HTTP 基本认证：

```yaml
rtsp:
  username: admin
  password: secret123
```

客户端必须在 URL 中提供凭据：
```
rtsp://admin:secret123@localhost:554/live/stream
```

## 高级功能

### 拉流代理

自动拉取和缓存远程 RTSP 流。需要在 `global` 节点下配置：

```yaml
global:
  pullproxy:
    - id: 1                          # 唯一ID标识，必须大于0
      name: "camera-1"               # 拉流代理名称
      type: "rtsp"                   # 拉流协议类型
      streampath: "live/camera1"     # 在Monibuca中的流路径
      pullonstart: true              # 是否在启动时自动拉流
      pull:
        url: "rtsp://admin:password@192.168.1.100/stream"
      description: "前门摄像头"
    - id: 2
      name: "camera-2"
      type: "rtsp"
      streampath: "live/camera2"
      pullonstart: false
      pull:
        url: "rtsp://admin:password@192.168.1.101/stream"
```

或使用 API 动态管理拉流代理（参见下方 API 接口部分）。

### 推流代理

自动将流推送到远程 RTSP 服务器。需要在 `global` 节点下配置：

```yaml
global:
  pushproxy:
    - id: 1                          # 唯一ID标识，必须大于0
      name: "push-1"                 # 推流代理名称
      type: "rtsp"                   # 推流协议类型
      streampath: "live/stream1"     # 源流路径
      pushonstart: true              # 是否在启动时自动推流
      push:
        url: "rtsp://192.168.1.200/live/stream1"
      description: "推送到远程服务器"
```

或使用 API 动态管理推流代理（参见下方 API 接口部分）。

## 兼容性

### 已测试设备/软件

- ✅ FFmpeg
- ✅ VLC Media Player
- ✅ OBS Studio
- ✅ ONVIF 兼容设备

### 已知问题

查看 [BAD_DEVICE.md](BAD_DEVICE.md) 了解非标准 RTSP 实现的设备。

## 编解码器支持

插件透明传递编解码器信息。支持的编解码器取决于源和目标：

**视频**：H.264、H.265/HEVC、MPEG-4、MJPEG  
**音频**：AAC、G.711 (PCMA/PCMU)、G.726、MP3、OPUS

## 性能优化建议

- 互联网传输使用 TCP 模式
- 局域网传输使用 UDP 模式（延迟更低）
- 根据并发流数量调整 UDP 端口范围
- 启用身份认证防止未授权访问
- 高并发场景考虑使用硬件转码

## 故障排查

### 连接被拒绝
- 检查 554 端口是否需要 root/管理员权限
- 尝试使用其他端口（如 8554）
- 检查防火墙设置

### 无视频/音频
- 检查源和客户端之间的编解码器兼容性
- 查看日志中的 SDP 信息
- 使用 VLC 或 FFplay 测试以隔离问题

### UDP 丢包
- 增加 UDP 端口范围
- 切换到 TCP 传输
- 检查网络质量和带宽

## API 接口

### 拉取流

使用统一的拉流 API，将 `protocol` 设置为 `rtsp`：

```bash
POST /api/stream/pull
Content-Type: application/json

{
  "protocol": "rtsp",
  "streamPath": "camera1",
  "remoteURL": "rtsp://admin:password@192.168.1.100/stream",
  "pubAudio": true,
  "pubVideo": true
}
```

**参数说明：**
- `protocol` (必填): 设置为 `"rtsp"`
- `streamPath` (必填): Monibuca 中的本地流路径
- `remoteURL` (必填): 要拉取的远程 RTSP URL
- `pubAudio` (可选): 是否发布音频
- `pubVideo` (可选): 是否发布视频
- `testMode` (可选): 0 = 正常拉流，1 = 拉流但不发布
- 其他发布配置选项可用（参见 global.proto 中的 GlobalPullRequest）

### 停止流

```bash
POST /api/stream/stop/{streamPath}
```

### 管理拉流/推流代理

- `GET /api/proxy/pull/list` - 列出所有拉流代理
- `POST /api/proxy/pull/add` - 添加拉流代理
- `POST /api/proxy/pull/update` - 更新拉流代理
- `POST /api/proxy/pull/remove/{id}` - 删除拉流代理
- `GET /api/proxy/push/list` - 列出所有推流代理
- `POST /api/proxy/push/add` - 添加推流代理
- `POST /api/proxy/push/update` - 更新推流代理
- `POST /api/proxy/push/remove/{id}` - 删除推流代理

## 致谢

本插件参考了 AlexxIT 开发的优秀项目 [go2rtc](https://github.com/AlexxIT/go2rtc) 的代码和实现思路，该项目提供了一个功能全面的媒体服务器解决方案，具有先进的流媒体处理能力。

## 许可证

本插件是 Monibuca 项目的一部分，遵循相同的许可证条款。

