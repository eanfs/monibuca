# 196 演示环境:its-server 启动录制 400 → 视频未上传 MinIO 根因报告

- **日期**: 2026-07-15
- **环境**: 192.168.1.196(演示环境,suanfa-server)
  - `xde-monibuca`: `swr.../intetech/monibuca:v5.3.1.2607151131`(今日新镜像,12:48 启动)
  - `xde-its-server`: `v2.12.61.2602031634`(内含 `xde-media-spring-boot-starter-3.2.18.jar`)
  - MinIO: `xde-minio`(192.168.1.196:9000,桶 `vidu-media-bucket`)
- **现象**: 通过 xde-its-server 接口启动录制(900s),提前结束/15 分钟自动停止后,MinIO 中无视频。
- **结论**: **录制从未启动过** —— its-server 调 `POST /mp4/api/start/<streamPath>` 时 `duration` 传 JSON **数字**,monibuca v5.3.1 该字段为 proto **string**,gRPC-gateway 反序列化直接 **400 Bad Request**;后续 stop 因"录制不存在"返回 500。monibuca 自身"录制→S3 上传"链路经实测**完全正常**。

## 1. 证据链

| 时间 | 事实 | 来源 |
|---|---|---|
| 12:59:44.473 | its-server 发起录制 `duration: 900s` | its-server 日志 `开始MP4录制` |
| 12:59:44.476 | `POST /mp4/api/start/...` → **400 Bad Request**(全部流) | its-server 日志 `MP4录制失败` |
| 12:59:44+ | monibuca 侧无任何录制器启动日志 | monibuca 2026-07-15T13.log |
| 13:14:45(恰 900s 后) | its-server 自动停止调 `/mp4/api/stop/...` → **500**(`not found`) | its-server 日志 `停止MP4录制失败` |
| 复现 | `curl -d '{"duration":900}'` → 400 `proto: invalid value for string type: 900`;`-d '{"duration":"900"}'` → 进入 handler | 本次实测 |
| DTO 反编译 | `Mp4StartRequest.duration` 类型为 **`java.lang.Integer`**(Lombok DTO,jar 3.2.18) | xde-media-spring-boot-starter |
| 试录验证 | 用正确 string 参数试录 60s → moov fast-path → **成功上传 MinIO `vidu-media-bucket/DIAG-0715-test/test1.mp4`(3.8MB)** | monibuca 日志 + MinIO 数据目录 |

## 2. 为什么"以前能用"

- `ReqStartRecord.duration`(string)是 **2026-03-09 commit `5c1656e2`** 新增字段。
- 旧版 xde-monibuca 的 proto **没有 duration 字段**;grpc-gateway v2 默认 `DiscardUnknown: true`,未知字段 `{"duration":900}` 被**静默丢弃** → 录制正常启动,由 its-server 自己的 15 分钟定时器调 stop 结束。
- 升级到 v5.3.1 后 duration 成为**已知的 string 字段**,数字类型不再被容忍 → 400。

## 3. 修复建议(its-server / xde-media-spring-boot-starter)

1. **`Mp4StartRequest.duration` 由 `Integer` 改为 `String`**,发送 `"900"`(纯数字秒)或 `"900s"` 均可(服务端 `parseDurationWithDefault` 两者都支持)。
2. **注意联动**: duration 生效后 monibuca 会在 900s 到点**自行停录**(`duration reached, stopping recording`),its-server 自己的 15 分钟定时 stop 会再次收到 500 not-found —— stop 调用需**把 not-found 当作成功**(幂等),或改为不传 duration、完全由 its-server 定时 stop(=旧行为)。
3. 次要问题: its-server 每次都先调 `/api/proxy/pull/add` 且收到 500(疑为"代理已存在"),随后换路径成功拉流。不影响功能,建议 its-server 对"已存在"降级为 INFO,或改为先查询再添加。

## 4. 本次验证遗留

- MinIO `vidu-media-bucket/DIAG-0715-test/test1.mp4`(3.8MB,60s,640×360)为诊断试录,可删除;monibuca 本地 sqlite 中有对应 RecordStream 记录。
- 196 与容器内诊断临时文件已清理;monibuca/its-server 均未改动、未重启。
