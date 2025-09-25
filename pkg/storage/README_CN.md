# Storage Package

这个包提供了统一的存储接口，支持多种存储后端，包括本地存储、S3、OSS和COS。

## 条件编译

每种存储类型都使用条件编译，只有在指定相应的build tag时才会被编译：

- `local`: 本地文件系统存储
- `s3`: Amazon S3存储
- `oss`: 阿里云OSS存储  
- `cos`: 腾讯云COS存储

## 使用方法

### 编译时指定存储类型

```bash
# 只编译本地存储（默认包含，无需额外tags）
go build

# 只编译S3存储
go build -tags s3

# 编译多种存储类型
go build -tags "s3,oss"

# 编译所有存储类型
go build -tags "s3,oss,cos"

# 编译所有存储类型（包括本地存储）
go build -tags "s3,oss,cos"
```

**注意**：
- 本地存储（`local`）默认包含，无需指定build tag
- S3存储需要`-tags s3`
- OSS存储需要`-tags oss`  
- COS存储需要`-tags cos`
- 可以组合多个tags来支持多种存储类型

### 代码中使用

```go
import "m7s.live/v5/pkg/storage"

// 创建本地存储
localConfig := storage.LocalStorageConfig("/path/to/storage")
localStorage, err := storage.CreateStorage("local", localConfig)

// 创建S3存储
s3Config := &storage.S3StorageConfig{
    Endpoint:        "s3.amazonaws.com",
    Region:          "us-east-1",
    AccessKeyID:     "your-access-key",
    SecretAccessKey: "your-secret-key",
    Bucket:          "your-bucket",
    ForcePathStyle:  false, // MinIO需要设置为true
    UseSSL:          true,
    Timeout:         30 * time.Second,
}
s3Storage, err := storage.CreateStorage("s3", s3Config)

// 创建OSS存储
ossConfig := &storage.OSSStorageConfig{
    Endpoint:        "oss-cn-hangzhou.aliyuncs.com",
    AccessKeyID:     "your-access-key-id",
    AccessKeySecret: "your-access-key-secret",
    Bucket:          "your-bucket",
    UseSSL:          true,
    Timeout:         30,
}
ossStorage, err := storage.CreateStorage("oss", ossConfig)

// 创建COS存储
cosConfig := &storage.COSStorageConfig{
    SecretID:   "your-secret-id",
    SecretKey:  "your-secret-key",
    Region:     "ap-beijing",
    Bucket:     "your-bucket",
    UseHTTPS:   true,
    Timeout:    30,
}
cosStorage, err := storage.CreateStorage("cos", cosConfig)
```

## 存储类型

### Local Storage (`local`)

本地文件系统存储，不需要额外的依赖。

### S3 Storage (`s3`)

Amazon S3兼容存储，包括AWS S3和MinIO等。

依赖：
- `github.com/aws/aws-sdk-go`

### OSS Storage (`oss`)

阿里云对象存储服务。

依赖：
- `github.com/aliyun/aliyun-oss-go-sdk`

### COS Storage (`cos`)

腾讯云对象存储服务。

依赖：
- `github.com/tencentyun/cos-go-sdk-v5`

## 工厂模式

存储包使用工厂模式来创建不同类型的存储实例：

```go
var Factory = map[string]func(any) (Storage, error){}
```

每种存储类型在各自的文件中通过`init()`函数注册到工厂中：

- `local.go`: 注册本地存储工厂函数
- `s3.go`: 注册S3存储工厂函数（需要`-tags s3`）
- `oss.go`: 注册OSS存储工厂函数（需要`-tags oss`）
- `cos.go`: 注册COS存储工厂函数（需要`-tags cos`）

使用`CreateStorage(type, config)`函数来创建存储实例，其中`type`是存储类型字符串，`config`是对应的配置对象。

## 存储接口

所有存储实现都遵循统一的`Storage`接口：

```go
type Storage interface {
    // CreateFile 创建文件并返回文件句柄
    CreateFile(ctx context.Context, path string) (File, error)
    
    // Delete 删除文件
    Delete(ctx context.Context, path string) error
    
    // Exists 检查文件是否存在
    Exists(ctx context.Context, path string) (bool, error)
    
    // GetSize 获取文件大小
    GetSize(ctx context.Context, path string) (int64, error)
    
    // GetURL 获取文件访问URL
    GetURL(ctx context.Context, path string) (string, error)
    
    // List 列出文件
    List(ctx context.Context, prefix string) ([]FileInfo, error)
    
    // Close 关闭存储连接
    Close() error
}
```

## 使用示例

```go
package main

import (
    "context"
    "fmt"
    "m7s.live/v5/pkg/storage"
)

func main() {
    // 创建本地存储
    config := storage.LocalStorageConfig("/tmp/storage")
    s, err := storage.CreateStorage("local", config)
    if err != nil {
        panic(err)
    }
    defer s.Close()
    
    ctx := context.Background()
    
    // 创建文件并写入内容
    file, err := s.CreateFile(ctx, "test.txt")
    if err != nil {
        panic(err)
    }
    
    file.Write([]byte("Hello, World!"))
    file.Close()
    
    // 检查文件是否存在
    exists, err := s.Exists(ctx, "test.txt")
    if err != nil {
        panic(err)
    }
    fmt.Printf("File exists: %v\n", exists)
    
    // 获取文件大小
    size, err := s.GetSize(ctx, "test.txt")
    if err != nil {
        panic(err)
    }
    fmt.Printf("File size: %d bytes\n", size)
    
    // 列出文件
    files, err := s.List(ctx, "")
    if err != nil {
        panic(err)
    }
    for _, file := range files {
        fmt.Printf("File: %s, Size: %d\n", file.Name, file.Size)
    }
}
```
