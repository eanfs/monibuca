# Windows 开发环境说明

本文用于在 Windows 上快速搭建并运行本项目（以 `example/default` + `sqlite` 为例）。

## 1. 安装基础工具

- Git（用于拉取代码、子模块等）
- Go（建议 `>= 1.24`，并确保 `go` 在 `PATH` 中）

验证：
```powershell
go version
```

## 2. 运行示例（推荐 sqlite 标签）

`sqlite` 标签使用纯 Go 的 sqlite 实现（不依赖 CGO），在 Windows 上更省心。

```powershell
cd example\default
$env:CGO_ENABLED = "0"
go run -tags sqlite main.go -c config.yaml
```

## 3. VS Code 调试

仓库自带 `.vscode/launch.json`：

- `Launch example/default (sqlite)`：直接启动 `example/default`，并默认 `CGO_ENABLED=0`

## 4. Protobuf 代码生成（可选）

### 4.1 安装 protoc

需要 `protoc` 可执行文件在 `PATH` 中（自行选择安装方式：Chocolatey / Scoop / 手动下载均可）。

验证：
```powershell
protoc --version
```

### 4.2 安装 Go 相关插件

确保 `GOBIN`（或默认的 `%USERPROFILE%\go\bin`）在 `PATH` 中，然后安装：
```powershell
go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
go install github.com/grpc-ecosystem/grpc-gateway/v2/protoc-gen-grpc-gateway@latest
```

验证（任意一个不存在都会导致生成失败）：
```powershell
protoc-gen-go --version
protoc-gen-go-grpc --version
protoc-gen-grpc-gateway --version
```

### 4.3 执行生成

全局 proto：
```powershell
.\scripts\protoc.bat
```

插件 proto：
```powershell
.\scripts\protoc.bat rtsp
```

## 5. 可选：FFmpeg 依赖

部分功能依赖 FFmpeg，可使用脚本下载到 `3rd/ffmpeg6`：
```powershell
python .\scripts\install_ffmpeg.py
```
