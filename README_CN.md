# Monibuca v5

[![Go Reference](https://pkg.go.dev/badge/m7s.live/v5.svg)](https://pkg.go.dev/m7s.live/v5)

Monibuca（简称 m7s）是一款纯 Go 开发的开源流媒体服务器开发框架，支持多种流媒体协议。

## 特性

- 🚀 高性能：采用纯 Go 开发，充分利用 Go 的并发特性
- 🔌 插件化架构：核心功能都以插件形式提供，可按需加载
- 🛠 可扩展性强：支持自定义插件开发
- 📽 多协议支持：
  - RTMP
  - HTTP-FLV
  - HLS
  - WebRTC
  - GB28181
  - SRT
- 🎯 低延迟：针对实时性场景优化
- 📊 实时监控：支持 Prometheus 监控集成
- 🔄 集群支持：支持分布式部署

## 快速开始

### 安装

1. 确保已安装 Go 1.23 或更高版本
2. 创建新项目并初始化：

```bash
mkdir my-m7s-server && cd my-m7s-server
go mod init my-m7s-server
```

3. 创建主程序：

```go
package main

import (
	"context"

	"m7s.live/v5"
	_ "m7s.live/v5/plugin/debug"  // 调试插件
	_ "m7s.live/v5/plugin/flv"    // FLV插件
	_ "m7s.live/v5/plugin/rtmp"   // RTMP插件
)

func main() {
	m7s.Run(context.Background(), "config.yaml")
}
```

### 配置说明

创建 `config.yaml` 配置文件：

```yaml
# 全局配置
global:
  http: :8080

# 插件配置
rtmp:
  tcp: :1935
```

## 构建选项

| 构建标签   | 描述                   |
| ---------- | ---------------------- |
| disable_rm | 禁用内存池             |
| sqlite     | 启用 SQLite 存储       |
| sqliteCGO  | 启用 SQLite CGO 版本   |
| mysql      | 启用 MySQL 存储        |
| postgres   | 启用 PostgreSQL 存储   |
| duckdb     | 启用 DuckDB 存储       |
| taskpanic  | 抛出 panic（用于测试） |

## 项目结构

```
monibuca/
├── plugin/       # 官方插件目录
├── pkg/          # 核心包
├── example/      # 示例代码
├── doc/          # 文档
└── scripts/      # 实用脚本
```

## 插件开发

查看 [plugin/README_CN.md](./plugin/README_CN.md) 了解如何开发自定义插件。

## Prometheus 监控

配置 Prometheus：

```yaml
scrape_configs:
  - job_name: "monibuca"
    metrics_path: "/api/metrics"
    static_configs:
      - targets: ["localhost:8080"]
```

## 示例

更多使用示例请查看 [example](./example) 目录。

## 贡献指南

欢迎提交 Pull Request 或 Issue。

## 许可证

本项目采用 AGPL 许可证，详见 [LICENSE](./LICENSE) 文件。

## 相关资源

- [官方文档](https://docs.m7s.live/)
- [API 参考](https://pkg.go.dev/m7s.live/v5)
- [示例代码](./example)
