# Monibuca v5.0.x Release Notes

## v5.0.4 (2025-08-15)

### 新增 / 改进 (Features & Improvements)
- GB28181: 支持更新 channelName / channelId（eba62c4）
- 定时任务(crontab): 初始化 SQL 支持（2bbee90）
- Snap 插件: 支持批量抓图（272def3）
- 管理后台: 支持自定义首页（15d830f）
- 推/拉代理: 支持可选参数更新（ad32f6f）
- 心跳/脉冲: pulse interval 允许为 0（17faf3f）
- 告警上报: 通过 Hook 发送报警（baf3640）
- 告警信息上报: 通过 Hook 发送 alarminfo（cad47ae）

## v5.0.3 (2025-06-27)

### 🎉 新功能 (New Features)

#### 录像与流媒体协议增强
- **MP4/FLV录像优化**：多项修复和优化录像拉取、分片、写入、格式转换等功能，提升兼容性和稳定性。
- **GB28181协议增强**：支持pullproxy代理GB28181流，完善平台配置、子码流播放、单独media port等能力。
- **插件与配置系统**：插件初始化、配置加载、数据库适配等增强，支持获取全部配置yaml示例。
- **WebRTC/HLS/RTMP协议适配**：WebRTC支持更多编解码器，HLS/RTMP协议兼容性提升。
- **crontab计划录像**：定时任务插件支持计划录像，拉流代理支持禁用。

### 🐛 问题修复 (Bug Fixes)
- **录像/流媒体相关**：修复mp4、flv、rtmp、hls等协议的多项bug，包括clone buffer、SQL语法、表结构适配等。
- **GB28181/数据库**：修复注册、流订阅、表结构、SQL语法等问题，适配PostgreSQL。
- **插件系统**：修复插件初始化、数据库对象赋值、配置加载等问题。

### 🛠️ 优化改进 (Improvements)
- **代码结构重构**：重构mp4、record、插件等系统，提升可维护性。
- **文档与示例**：完善文档说明，增加配置和API示例。
- **Docker镜像**：优化tcpdump、ffmpeg等工具集成。

### 👥 贡献者 (Contributors)
- langhuihui
- pggiroro
- banshan

---

## v5.0.2 (2025-06-05)

### 🎉 新功能 (New Features)

#### 核心功能
- **降低延迟** - 禁用了TCP WebRTC的重放保护功能，降低了延迟
- **配置系统增强** - 支持更多配置格式（支持配置项中插入`-`、`_`和大写字母），提升配置灵活性
- **原始数据检查** - 新增原始数据无帧检查功能，提升数据处理稳定性
- **MP4循环读取** - 支持MP4文件循环读取功能（通过配置 pull 配置下的 `loop` 配置）
- **S3插件** - 新增S3存储插件，支持云存储集成
- **TCP读写缓冲配置** - 新增TCP连接读写缓冲区配置选项（针对高并发下的吞吐能力增强）
- **拉流测试模式** - 新增拉流测试模式选项（可以选择拉流时不发布），便于调试和测试
- **SEI API格式扩展** - 扩展SEI API支持更多数据格式
- **Hook扩展** - 新增更多Hook回调点，增强扩展性
- **定时任务插件** - 新增crontab定时任务插件
- **服务器抓包** - 新增服务器抓包功能（调用`tcpdump`），支持TCP和UDP协议,API 说明见 [tcpdump](https://api.monibuca.com/api-301117332)

#### GB28181协议增强
- **平台配置支持** - GB28181现在支持从config.yaml中添加平台和平台通道配置
- **子码流播放** - 支持GB28181子码流播放功能
- **SDP优化** - 优化invite SDP中的mediaip和sipip处理
- **本地端口保存** - 修复GB28181本地端口保存到数据库的问题

#### MP4功能增强
- **FLV格式下载** - 支持从MP4录制文件下载FLV格式
- **下载功能修复** - 修复MP4下载功能的相关问题
- **恢复功能修复** - 修复MP4恢复功能

### 🐛 问题修复 (Bug Fixes)

#### 网络通信
- **TCP读取阻塞** - 修复TCP读取阻塞问题（增加了读取超时设置）
- **RTSP内存泄漏** - 修复RTSP协议的内存泄漏问题
- **RTSP音视频标识** - 修复RTSP无音频或视频标识的问题

#### GB28181协议
- **任务管理** - 使用task.Manager解决注册处理器的问题
- **计划长度** - 修复plan.length为168的问题
- **注册频率** - 修复GB28181注册过快导致启动过多任务的问题
- **联系信息** - 修复GB28181获取错误联系信息的问题

#### RTMP协议
- **时间戳处理** - 修复RTMP时间戳开头跳跃问题

### 🛠️ 优化改进 (Improvements)

#### Docker支持
- **tcpdump工具** - Docker镜像中新增tcpdump网络诊断工具

#### Linux平台优化
- **SIP请求优化** - Linux平台移除SIP请求中的viaheader

### 👥 贡献者 (Contributors)
- langhuihui
- pggiroro  
- banshan

---

## v5.0.1 (2025-05-21)

### 🎉 新功能 (New Features)

#### WebRTC增强
- **H265支持** - 新增WebRTC对H265编码的支持，提升视频质量和压缩效率

#### GB28181协议增强
- **订阅功能扩展** - GB28181模块现在支持订阅报警、移动位置、目录信息
- **通知请求** - 支持接收通知请求，增强与设备的交互能力

#### Docker优化
- **FFmpeg集成** - Docker镜像中新增FFmpeg工具，支持更多音视频处理场景
- **多架构支持** - 新增Docker多架构构建支持

### 🐛 问题修复 (Bug Fixes)

#### Docker相关
- **构建问题** - 修复Docker构建过程中的多个问题
- **构建优化** - 优化Docker构建流程，提升构建效率

#### RTMP协议
- **时间戳处理** - 修复RTMP第一个chunk类型3需要添加时间戳的问题

#### GB28181协议  
- **路径匹配** - 修复GB28181模块中播放流路径的正则表达式匹配问题

#### MP4处理
- **stsz box** - 修复stsz box采样大小的问题
- **G711音频** - 修复拉取MP4文件时读取G711音频的问题
- **H265解析** - 修复H265 MP4文件解析问题

### 🛠️ 优化改进 (Improvements)

#### 代码质量
- **错误处理** - 新增maxcount错误处理机制
- **文档更新** - 更新README文档和go.mod配置

#### 构建系统
- **ARM架构** - 减少JavaScript代码，优化ARM架构Docker构建
- **构建标签** - 移除Docker中不必要的构建标签

### 📦 其他更新 (Other Updates)
- **MCP相关** - 更新Model Context Protocol相关功能
- **依赖更新** - 更新项目依赖和模块配置

### 👥 贡献者 (Contributors)
- langhuihui

---
