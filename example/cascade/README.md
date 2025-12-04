# Monibuca 级联服务器配置示例

这个示例展示了如何配置一个 Monibuca 级联服务器和三个级联客户端。

## 配置说明

### 1. 级联服务器 (cascadeserver.yml)
- **端口**: HTTP: 8080, QUIC: 44944
- **自动注册**: 启用
- **数据库**: SQLite, 文件路径: cascadeserver.db

### 2. 级联客户端 (cascadeclient1-3.yml)
- **客户端1**: HTTP 8081, QUIC 44945
- **客户端2**: HTTP 8082, QUIC 44946
- **客户端3**: HTTP 8083, QUIC 44947
- **上级服务器**: 127.0.0.1:44944
- **自动推流**: 启用

## 运行方式

### 方式1：分别启动（推荐用于调试）
在四个不同的终端窗口中分别执行：

```bash
# 级联服务器
cd example/cascade
go run -tags sqlite main.go -c cascadeserver.yml

# 客户端1
cd example/cascade
go run -tags sqlite main.go -c cascadeclient1.yml

# 客户端2
cd example/cascade
go run -tags sqlite main.go -c cascadeclient2.yml

# 客户端3
cd example/cascade
go run -tags sqlite main.go -c cascadeclient3.yml
```

### 方式2：同时启动（使用 multicascade.go）
在单个终端窗口中同时启动所有实例：

```bash
cd example/cascade
go run -tags sqlite main.go
```

默认会同时启动 1 个级联服务器和 3 个级联客户端。

## 验证方式

1. 查看服务器和客户端的日志，确认连接成功
2. 在任意客户端推流，检查上级服务器是否能收到该流
3. 通过 http://127.0.0.1:8080/debug 可以查看服务器状态

## 注意事项

- 确保端口没有冲突
- 所有客户端和服务器的配置文件中的 secret 应该保持一致或正确配置
- 级联服务器需要配置数据库支持
