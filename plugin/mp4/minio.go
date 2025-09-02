package plugin_mp4

import (
	"context"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinioConfig struct {
	Enable         bool   `default:"false" desc:"是否启用Minio上传功能"`
	Endpoint       string `desc:"Minio服务器地址"`
	AccessKeyID    string `desc:"访问密钥ID"`
	SecretAccessKey string `desc:"秘密访问密钥"`
	Bucket         string `desc:"存储桶名称"`
	UseSSL         bool   `default:"false" desc:"是否使用SSL连接"`
	DeleteAfterUpload bool `default:"false" desc:"上传后是否删除本地文件"`
}

type MinioClient struct {
	client *minio.Client
	config *MinioConfig
}

func NewMinioClient(config *MinioConfig) (*MinioClient, error) {
	// 初始化Minio客户端
	client, err := minio.New(config.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(config.AccessKeyID, config.SecretAccessKey, ""),
		Secure: config.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create Minio client: %w", err)
	}

	return &MinioClient{
		client: client,
		config: config,
	}, nil
}

func (m *MinioClient) UploadFile(filePath, objectName string) error {
	if m == nil || m.client == nil {
		return nil
	}

	// 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// 获取文件信息
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("failed to get file info: %w", err)
	}

	// 上传文件到Minio
	_, err = m.client.PutObject(context.Background(), m.config.Bucket, objectName, file, fileInfo.Size(), minio.PutObjectOptions{})
	if err != nil {
		return fmt.Errorf("failed to upload file to Minio: %w", err)
	}

	return nil
}