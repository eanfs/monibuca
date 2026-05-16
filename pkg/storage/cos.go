//go:build cos

package storage

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tencentyun/cos-go-sdk-v5"
	"m7s.live/v5/pkg/config"
)

// COSStorageConfig COS存储配置
type COSStorageConfig struct {
	SecretID      string        `yaml:"secret_id" desc:"COS Secret ID"`
	SecretKey     string        `yaml:"secret_key" desc:"COS Secret Key"`
	Region        string        `yaml:"region" desc:"COS区域"`
	Bucket        string        `yaml:"bucket" desc:"COS存储桶名称"`
	PathPrefix    string        `yaml:"path_prefix" desc:"文件路径前缀"`
	UseHTTPS      bool          `yaml:"use_https" desc:"是否使用HTTPS" default:"true"`
	Timeout       int           `yaml:"timeout" desc:"上传超时时间（秒）" default:"900"`
	MaxRetries    int           `yaml:"max_retries" desc:"上传失败最大重试次数" default:"3"`
	RetryInterval time.Duration `yaml:"retry_interval" desc:"重试基础间隔（指数退避）" default:"5s"`
}

func (c *COSStorageConfig) GetType() StorageType {
	return StorageTypeCOS
}

func (c *COSStorageConfig) Validate() error {
	if c.SecretID == "" {
		return fmt.Errorf("secret_id is required for COS storage")
	}
	if c.SecretKey == "" {
		return fmt.Errorf("secret_key is required for COS storage")
	}
	if c.Bucket == "" {
		return fmt.Errorf("bucket is required for COS storage")
	}
	if c.Region == "" {
		return fmt.Errorf("region is required for COS storage")
	}
	return nil
}

// retryConfig 获取重试配置
func (c *COSStorageConfig) retryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    c.MaxRetries,
		RetryInterval: c.RetryInterval,
	}
}

// COSStorage COS存储实现
type COSStorage struct {
	config *COSStorageConfig
	client *cos.Client
}

// NewCOSStorage 创建COS存储实例
func NewCOSStorage(config *COSStorageConfig) (*COSStorage, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// 设置默认值
	if config.Timeout == 0 {
		config.Timeout = 900
	}

	// 构建存储桶URL
	scheme := "http"
	if config.UseHTTPS {
		scheme = "https"
	}
	rawURL := fmt.Sprintf("%s://%s.cos.%s.myqcloud.com", scheme, config.Bucket, config.Region)
	bucketURL, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("failed to parse COS bucket URL %q: %w", rawURL, err)
	}

	// 创建COS客户端
	client := cos.NewClient(&cos.BaseURL{BucketURL: bucketURL}, &http.Client{
		Transport: &cos.AuthorizationTransport{
			SecretID:  config.SecretID,
			SecretKey: config.SecretKey,
		},
	})

	// 测试连接
	if err := testCOSConnection(client, config.Bucket); err != nil {
		return nil, fmt.Errorf("COS connection test failed: %w", err)
	}

	return &COSStorage{
		config: config,
		client: client,
	}, nil
}

func (s *COSStorage) GetKey() string {
	return "cos"
}
func (s *COSStorage) CreateFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &COSFile{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
	}, nil
}

func (s *COSStorage) Delete(ctx context.Context, path string) error {
	objectKey := s.getObjectKey(path)
	_, err := s.client.Object.Delete(ctx, objectKey)
	return err
}

func (s *COSStorage) OpenFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &COSFile{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
	}, nil
}

func (s *COSStorage) Exists(ctx context.Context, path string) (bool, error) {
	objectKey := s.getObjectKey(path)

	_, err := s.client.Object.Head(ctx, objectKey, nil)
	if err != nil {
		// 检查是否是404错误
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (s *COSStorage) GetSize(ctx context.Context, path string) (int64, error) {
	objectKey := s.getObjectKey(path)

	resp, err := s.client.Object.Head(ctx, objectKey, nil)
	if err != nil {
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
			return 0, ErrFileNotFound
		}
		return 0, err
	}

	return resp.ContentLength, nil
}

func (s *COSStorage) GetURL(ctx context.Context, path string) (string, error) {
	objectKey := s.getObjectKey(path)

	// 生成预签名URL，24小时有效期
	presignedURL, err := s.client.Object.GetPresignedURL(ctx, http.MethodGet, objectKey,
		s.config.SecretID, s.config.SecretKey, 24*time.Hour, nil)
	if err != nil {
		return "", err
	}

	return presignedURL.String(), nil
}

func (s *COSStorage) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	objectPrefix := s.getObjectKey(prefix)

	var files []FileInfo

	opt := &cos.BucketGetOptions{
		Prefix:  objectPrefix,
		MaxKeys: 1000,
	}

	result, _, err := s.client.Bucket.Get(ctx, opt)
	if err != nil {
		return nil, err
	}

	for _, obj := range result.Contents {
		// 移除路径前缀
		fileName := obj.Key
		if s.config.PathPrefix != "" {
			fileName = strings.TrimPrefix(fileName, strings.TrimSuffix(s.config.PathPrefix, "/")+"/")
		}

		lastModified, _ := time.Parse(time.RFC3339, obj.LastModified)
		files = append(files, FileInfo{
			Name:         fileName,
			Size:         obj.Size,
			LastModified: lastModified,
			ETag:         obj.ETag,
		})
	}

	return files, nil
}

func (s *COSStorage) Close() error {
	// COS客户端无需显式关闭
	return nil
}

// getObjectKey 获取COS对象键
func (s *COSStorage) getObjectKey(path string) string {
	if s.config.PathPrefix != "" {
		return strings.TrimSuffix(s.config.PathPrefix, "/") + "/" + path
	}
	return path
}

// testCOSConnection 测试COS连接
func testCOSConnection(client *cos.Client, bucket string) error {
	// 尝试获取存储桶信息来测试连接
	_, err := client.Bucket.Head(context.Background())
	return err
}

// COSFile COS文件读写器
type COSFile struct {
	mu        sync.Mutex
	storage   *COSStorage
	objectKey string
	ctx       context.Context
	tempFile  *os.File // 本地临时文件，用于支持随机访问
	filePath  string   // 临时文件路径
}

// SetMetadata COS 元数据支持的空实现，满足 File 接口。
func (f *COSFile) SetMetadata(key, value string) {}

func (f *COSFile) Name() string {
	return f.objectKey
}

func (f *COSFile) Write(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.createTempFile(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Write(p)
}

func (f *COSFile) Read(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Read(p)
}

func (f *COSFile) WriteAt(p []byte, off int64) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.createTempFile(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.WriteAt(p, off)
}

func (f *COSFile) ReadAt(p []byte, off int64) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.ReadAt(p, off)
}

func (f *COSFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile != nil {
		if err := f.tempFile.Sync(); err != nil {
			return err
		}
	}
	return f.uploadTempFile()
}

func (f *COSFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err := f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Seek(offset, whence)
}

func (f *COSFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile != nil {
		if err := f.tempFile.Sync(); err != nil {
			// Sync 失败时保留文件而非删除：经 FinalizeFromTemp 接管后
			// f.filePath 即调用方的临时文件，删除它会导致上层补传逻辑
			// (MoveToPendingDir) 找不到文件而丢数据。与上传失败分支一致。
			f.cleanup(false)
			return err
		}
	}
	err := f.uploadTempFile()
	f.cleanup(err == nil)
	if err != nil && OnUploadFailed != nil && f.filePath != "" {
		var fileSize int64
		if f.tempFile != nil {
			if stat, statErr := f.tempFile.Stat(); statErr == nil {
				fileSize = stat.Size()
			}
		}
		OnUploadFailed(f.filePath, f.objectKey, "cos", fileSize, nil, err)
	}
	return err
}

func (f *COSFile) cleanup(deleteFile bool) {
	if f.tempFile != nil {
		f.tempFile.Close()
		f.tempFile = nil
	}
	if deleteFile && f.filePath != "" {
		os.Remove(f.filePath)
		f.filePath = ""
	}
}

// createTempFile 创建临时文件
func (f *COSFile) createTempFile() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "coswriter_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	f.tempFile = tempFile
	f.filePath = tempFile.Name()
	return nil
}

func (f *COSFile) Stat() (os.FileInfo, error) {
	if f.tempFile == nil {
		return nil, fmt.Errorf("cos file not initialized")
	}
	return f.tempFile.Stat()
}

// FinalizeFromTemp 让 COSFile 直接以 srcPath 指向的完整文件作为上传源。
// 实现 storage.TempFileFinalizer。详见 s3.go 同名方法。
func (f *COSFile) FinalizeFromTemp(srcPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	adopted, err := adoptUploadTempFile(f.tempFile, f.filePath, srcPath)
	if err != nil {
		f.tempFile = nil
		return fmt.Errorf("finalize from temp: %w", err)
	}
	f.tempFile = adopted
	f.filePath = srcPath
	return nil
}

// uploadTempFile 上传临时文件到COS，带并发控制和指数退避重试
func (f *COSFile) uploadTempFile() error {
	// 解耦上传 ctx 与文件 ctx (= Recorder.Context). 详见 s3.go uploadTempFile 注释.
	uploadCtx := context.WithoutCancel(f.ctx)

	if err := AcquireUploadSlot(uploadCtx); err != nil {
		return fmt.Errorf("acquire upload slot: %w", err)
	}
	defer ReleaseUploadSlot()

	var fileSize int64
	if stat, err := f.tempFile.Stat(); err == nil {
		fileSize = stat.Size()
	}
	log.Printf("[COS] uploading: key=%s size=%d active=%d/%d",
		f.objectKey, fileSize, GetActiveUploads(), GetMaxConcurrentUploads())

	rc := f.storage.config.retryConfig()

	return UploadWithRetry(uploadCtx, rc, "COS", f.objectKey,
		nil, // COS PutFromFile 不需要 resetFn（按文件路径上传）
		func() error {
			if _, err := f.storage.client.Object.PutFromFile(uploadCtx, f.objectKey, f.filePath, nil); err != nil {
				return fmt.Errorf("failed to upload to COS: %w", err)
			}
			log.Printf("[COS] upload successful: %s", f.objectKey)
			return nil
		},
	)
}

// downloadToTemp 下载COS对象到本地临时文件
func (f *COSFile) downloadToTemp() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "cosreader_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	f.tempFile = tempFile
	f.filePath = tempFile.Name()

	// 下载COS对象
	_, err = f.storage.client.Object.GetToFile(f.ctx, f.objectKey, f.filePath, nil)
	if err != nil {
		tempFile.Close()
		os.Remove(f.filePath)
		if strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "NoSuchKey") {
			return ErrFileNotFound
		}
		return fmt.Errorf("failed to download from COS: %w", err)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, 0)
	if err != nil {
		tempFile.Close()
		os.Remove(f.filePath)
		return fmt.Errorf("failed to seek temp file: %w", err)
	}

	return nil
}

var _ TempFileFinalizer = (*COSFile)(nil)

func init() {
	Factory["cos"] = func(conf any) (Storage, error) {
		var cosConfig COSStorageConfig
		config.Parse(&cosConfig, conf.(map[string]any))
		return NewCOSStorage(&cosConfig)
	}

	// 注册 COS 存储类型 Schema
	RegisterSchema(StorageSchema{
		Type:        "cos",
		Name:        "腾讯云 COS",
		Description: "腾讯云对象存储服务",
		Properties:  GenerateSchemaFromStruct(COSStorageConfig{}),
	})
}
