# GB28181 Plugin Guidelines

中国公共安全视频监控国标协议(GB/T 28181)实现。支持下级设备管理、上级级联、直播/录像点播、云台控制。

## 核心概念

| 组件 | 职责 | 关键文件 |
|------|------|---------|
| **Device** | 下级设备实体 | `device.go` |
| **Channel** | 设备通道(摄像头/NVR端口) | `channel.go` |
| **Dialog** | SIP会话管理(直播/录像) | `dialog.go` |
| **Platform** | 上级平台连接 | `platform.go` |
| **DownloadDialog** | 录像下载会话 | `downloaddialog.go` |

## 协议流程

### 下级设备注册流程
```
设备 --REGISTER--> GB28181插件
GB28181插件 --Catalog(MESSAGE)--> 设备 (获取通道列表)
设备 --定期Keepalive--> GB28181插件
```

### 直播/录像播放
```
用户/上级 --> GB28181插件.INVITE(设备ID+通道ID[+时间范围])
GB28181插件 --> 设备.INVITE(SDP参数)
设备 --> GB28181插件 (RTP/PS流)
GB28181插件 --> 用户/上级 (转发流)
```

### 级联上级
```
GB28181插件 --REGISTER--> 上级平台
上级平台 --INVITE--> GB28181插件
GB28181插件 --INVITE--> 下级设备
下级设备 --RTP/PS--> GB28181插件 --RTP/PS--> 上级平台
```

## 关键实现

### SIP消息处理
- **核心库**: `github.com/emiago/sipgo`
- **入口**: `index.go` 的 `OnRequest()` 处理所有SIP请求
- **状态机**: `Device` 和 `Platform` 管理连接生命周期
- **消息类型**: REGISTER, MESSAGE(Catalog/Keepalive), INVITE, BYE, ACK

### 媒体流处理
- **端口管理**: `PortBitmap` (TCP/UDP端口池) + 单端口模式
- **RTP接收**: `pkg/rtp_reader.go` - PS封装的RTP流
- **PS解封装**: `m7s.live/v5/pkg/format/ps` - 提取H264/H265/AAC
- **流发布**: 通过 `Dialog.Publish()` 注入m7s核心

### 数据库模型 (`pkg/`)
- `device_model.go` - 设备持久化(ID/IP/端口/状态)
- `channel_model.go` - 通道信息(编码/状态/父通道)
- `platform_model.go` - 上级配置
- 使用 `gorm` ORM,支持SQLite/MySQL/PostgreSQL

## 常见任务

### 添加新的SIP命令支持
1. 在 `api.go` 添加gRPC接口定义
2. 在 `device.go` 或 `platform.go` 实现SIP消息构造
3. 使用 `gb28181.BuildXMLMessage()` 生成符合国标的XML
4. 通过 `sipgo.Client.Request()` 发送

### 处理新的通道类型
1. 更新 `channel_model.go` 添加字段
2. 在 `CatalogQuery()` 响应中解析新类型
3. 调整 `channel.go` 的流处理逻辑

### 调试SIP交互
- 启用日志: 配置 `loglevel: debug`
- SIP包抓取: `tcpdump -i any port 5060 -w gb28181.pcap`
- 关键日志点: `OnRequest`, `OnResponse`, `StartInvite`

## 配置要点

```yaml
gb28181:
  mediaip: 10.15.94.58      # 必填:流媒体收流IP
  sipip: 10.15.94.58        # 必填:SIP通讯IP
  mediaport: "10001-20000"  # 端口范围
  serial: "34020000002000000001"  # 服务器ID
  realm: "3402000000"       # SIP域
  password: "123456"        # 设备认证密码
  sip:
    listenaddr:
      - udp::15060          # SIP端口
```

### 播放URL模式
```
正则: ^gb_\\d+/(.+)$
示例: /flv/gb_1/34020000001110000003/34020000001320000003.flv
格式: /协议/gb_1/设备ID/通道ID.扩展名
```

## 避坑指南

| 问题 | 原因 | 解决 |
|------|------|------|
| 设备注册失败 | IP配置错误/防火墙 | 确保 `sipip` 可达,检查5060端口 |
| 无法收流 | `mediaip` 不对/端口未开 | 配置正确的公网IP,开放端口范围 |
| 录像播放卡顿 | 设备I帧间隔大 | 要求设备降低GOP(2秒内) |
| SIP超时 | NAT穿透问题 | 使用TCP模式或配置STUN |
| 通道同步慢 | Catalog响应大 | 分批查询,调整 `GetChannels` 分页 |

## API接口

完整API参考: `plugin/gb28181/README.md` (包含50+接口)

**高频接口**:
- `GET /gb28181/api/devices` - 设备列表
- `GET /gb28181/api/devices/{deviceId}/channels` - 通道列表
- `GET /gb28181/api/records/{deviceId}/{channelId}` - 录像查询
- `GET /gb28181/api/ptz/{deviceId}/{channelId}` - 云台控制

## 性能优化

- **端口池预分配**: 启动时初始化PortBitmap避免锁竞争
- **SIP Client复用**: `clients Collection` 按IP:Port缓存连接
- **异步Catalog**: Device注册后异步查询通道避免阻塞
- **单端口模式**: 高并发场景使用单TCP/UDP端口+SSRC路由

## 测试

```bash
# 启动服务
cd example/default && go run -tags sqlite main.go

# 使用GB28181模拟器连接
# 配置模拟器: SIP服务器 = 配置的sipip:5060

# 手动SIP测试(需sipgo工具)
sipgo call sip:34020000002000000001@10.15.94.58:5060
```

## 相关文档

- `doc_CN/gb28181_protocol.md` - 协议详解(如果存在)
- `pb/gb28181.proto` - gRPC接口定义
- `pkg/xml.go` - 国标XML消息格式
- [GB/T 28181-2022标准文档](需要单独获取)
