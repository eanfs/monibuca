//go:build oss

package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"m7s.live/v5/pkg/config"
)

// OSSStorageConfig OSS存储配置
type OSSStorageConfig struct {
	Endpoint        string        `yaml:"endpoint" desc:"OSS服务端点"`
	AccessKeyID     string        `yaml:"access_key_id" desc:"OSS访问密钥ID"`
	AccessKeySecret string        `yaml:"access_key_secret" desc:"OSS访问密钥Secret"`
	Bucket          string        `yaml:"bucket" desc:"OSS存储桶名称"`
	PathPrefix      string        `yaml:"path_prefix" desc:"文件路径前缀"`
	UseSSL          bool          `yaml:"use_ssl" desc:"是否使用SSL" default:"true"`
	Timeout         int           `yaml:"timeout" desc:"上传超时时间（秒）" default:"900"`
	MaxRetries      int           `yaml:"max_retries" desc:"上传失败最大重试次数" default:"3"`
	RetryInterval   time.Duration `yaml:"retry_interval" desc:"重试基础间隔（指数退避）" default:"5s"`
}

func (c *OSSStorageConfig) GetType() StorageType {
	return StorageTypeOSS
}

func (c *OSSStorageConfig) Validate() error {
	if c.AccessKeyID == "" {
		return fmt.Errorf("access_key_id is required for OSS storage")
	}
	if c.AccessKeySecret == "" {
		return fmt.Errorf("access_key_secret is required for OSS storage")
	}
	if c.Bucket == "" {
		return fmt.Errorf("bucket is required for OSS storage")
	}
	if c.Endpoint == "" {
		return fmt.Errorf("endpoint is required for OSS storage")
	}
	return nil
}

// retryConfig 获取重试配置
func (c *OSSStorageConfig) retryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    c.MaxRetries,
		RetryInterval: c.RetryInterval,
	}
}

// OSSStorage OSS存储实现
type OSSStorage struct {
	config *OSSStorageConfig
	client *oss.Client
	bucket *oss.Bucket
}

// NewOSSStorage 创建OSS存储实例
func NewOSSStorage(config *OSSStorageConfig) (*OSSStorage, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// 设置默认值
	if config.Timeout == 0 {
		config.Timeout = 900
	}

	// 创建OSS客户端（设置连接和读写超时防止网络故障时永久挂起）
	connectTimeout := int64(10) // 10秒连接超时
	rwTimeout := int64(config.Timeout)
	if rwTimeout <= 0 {
		rwTimeout = 900
	}
	client, err := oss.New(config.Endpoint, config.AccessKeyID, config.AccessKeySecret,
		oss.Timeout(connectTimeout, rwTimeout))
	if err != nil {
		return nil, fmt.Errorf("failed to create OSS client: %w", err)
	}

	// 获取存储桶
	bucket, err := client.Bucket(config.Bucket)
	if err != nil {
		return nil, fmt.Errorf("failed to get OSS bucket: %w", err)
	}

	// 测试连接
	if err := testOSSConnection(bucket); err != nil {
		return nil, fmt.Errorf("OSS connection test failed: %w", err)
	}

	return &OSSStorage{
		config: config,
		client: client,
		bucket: bucket,
	}, nil
}

func (s *OSSStorage) GetKey() string {
	return "oss"
}

func (s *OSSStorage) CreateFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &OSSFile{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
	}, nil
}

func (s *OSSStorage) Delete(ctx context.Context, path string) error {
	objectKey := s.getObjectKey(path)
	return s.bucket.DeleteObject(objectKey)
}

func (s *OSSStorage) Exists(ctx context.Context, path string) (bool, error) {
	objectKey := s.getObjectKey(path)
	exists, err := s.bucket.IsObjectExist(objectKey)
	if err != nil {
		return false, err
	}
	return exists, nil
}

func (s *OSSStorage) GetSize(ctx context.Context, path string) (int64, error) {
	objectKey := s.getObjectKey(path)

	props, err := s.bucket.GetObjectDetailedMeta(objectKey)
	if err != nil {
		if strings.Contains(err.Error(), "NoSuchKey") {
			return 0, ErrFileNotFound
		}
		return 0, err
	}

	contentLength := props.Get("Content-Length")
	if contentLength == "" {
		return 0, nil
	}

	var size int64
	if _, err := fmt.Sscanf(contentLength, "%d", &size); err != nil {
		return 0, fmt.Errorf("failed to parse content length: %w", err)
	}

	return size, nil
}

func (s *OSSStorage) GetURL(ctx context.Context, path string) (string, error) {
	objectKey := s.getObjectKey(path)

	// 生成签名URL，24小时有效期
	url, err := s.bucket.SignURL(objectKey, oss.HTTPGet, 24*3600)
	if err != nil {
		return "", err
	}

	return url, nil
}

func (s *OSSStorage) OpenFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &OSSFile{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
	}, nil
}

func (s *OSSStorage) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	objectPrefix := s.getObjectKey(prefix)

	var files []FileInfo

	result, err := s.bucket.ListObjects(oss.Prefix(objectPrefix))
	if err != nil {
		return nil, err
	}
	for _, obj := range result.Objects {
		// 移除路径前缀
		fileName := obj.Key
		if s.config.PathPrefix != "" {
			fileName = strings.TrimPrefix(fileName, strings.TrimSuffix(s.config.PathPrefix, "/")+"/")
		}

		files = append(files, FileInfo{
			Name:         fileName,
			Size:         obj.Size,
			LastModified: obj.LastModified,
			ETag:         obj.ETag,
		})
	}

	return files, nil
}

func (s *OSSStorage) Close() error {
	// OSS客户端无需显式关闭
	return nil
}

// getObjectKey 获取OSS对象键
func (s *OSSStorage) getObjectKey(path string) string {
	if s.config.PathPrefix != "" {
		return strings.TrimSuffix(s.config.PathPrefix, "/") + "/" + path
	}
	return path
}

// testOSSConnection 测试OSS连接
func testOSSConnection(bucket *oss.Bucket) error {
	// 尝试列出对象来测试连接
	_, err := bucket.ListObjects(oss.MaxKeys(1))
	return err
}

// OSSFile OSS文件读写器
type OSSFile struct {
	mu        sync.Mutex
	storage   *OSSStorage
	objectKey string
	ctx       context.Context
	tempFile  *os.File // 本地临时文件，用于支持随机访问
	filePath  string   // 临时文件路径
}

// SetMetadata OSS 元数据支持的空实现，满足 File 接口。
func (f *OSSFile) SetMetadata(key, value string) {}

func (f *OSSFile) Name() string {
	return f.objectKey
}

func (f *OSSFile) Write(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.createTempFile(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Write(p)
}

func (f *OSSFile) Read(p []byte) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Read(p)
}

func (f *OSSFile) WriteAt(p []byte, off int64) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.createTempFile(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.WriteAt(p, off)
}

func (f *OSSFile) ReadAt(p []byte, off int64) (n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err = f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.ReadAt(p, off)
}

func (f *OSSFile) Sync() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile != nil {
		if err := f.tempFile.Sync(); err != nil {
			return err
		}
	}
	return f.uploadTempFile()
}

func (f *OSSFile) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.tempFile == nil {
		if err := f.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return f.tempFile.Seek(offset, whence)
}

func (f *OSSFile) Close() error {
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
		OnUploadFailed(f.filePath, f.objectKey, "oss", fileSize, nil, err)
	}
	return err
}

func (f *OSSFile) cleanup(deleteFile bool) {
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
func (f *OSSFile) createTempFile() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "osswriter_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	f.tempFile = tempFile
	f.filePath = tempFile.Name()
	return nil
}

func (f *OSSFile) Stat() (os.FileInfo, error) {
	if f.tempFile == nil {
		return nil, fmt.Errorf("oss file not initialized")
	}
	return f.tempFile.Stat()
}

// FinalizeFromTemp 让 OSSFile 直接以 srcPath 指向的完整文件作为上传源。
// 实现 storage.TempFileFinalizer。详见 s3.go 同名方法。
func (f *OSSFile) FinalizeFromTemp(srcPath string) error {
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

// uploadTempFile 上传临时文件到OSS，带并发控制和指数退避重试
func (f *OSSFile) uploadTempFile() error {
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
	log.Printf("[OSS] uploading: key=%s size=%d active=%d/%d",
		f.objectKey, fileSize, GetActiveUploads(), GetMaxConcurrentUploads())

	rc := f.storage.config.retryConfig()

	return UploadWithRetry(uploadCtx, rc, "OSS", f.objectKey,
		nil, // OSS PutObjectFromFile 不需要 resetFn（按文件路径上传）
		func() error {
			if err := f.storage.bucket.PutObjectFromFile(f.objectKey, f.filePath); err != nil {
				return fmt.Errorf("failed to upload to OSS: %w", err)
			}
			log.Printf("[OSS] upload successful: %s", f.objectKey)
			return nil
		},
	)
}

// downloadToTemp 下载OSS对象到本地临时文件
func (f *OSSFile) downloadToTemp() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "ossreader_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	f.tempFile = tempFile
	f.filePath = tempFile.Name()

	// 下载OSS对象
	err = f.storage.bucket.GetObjectToFile(f.objectKey, f.filePath)
	if err != nil {
		tempFile.Close()
		os.Remove(f.filePath)
		if strings.Contains(err.Error(), "NoSuchKey") {
			return ErrFileNotFound
		}
		return fmt.Errorf("failed to download from OSS: %w", err)
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

var _ TempFileFinalizer = (*OSSFile)(nil)

func init() {
	Factory["oss"] = func(conf any) (Storage, error) {
		var ossConfig OSSStorageConfig
		config.Parse(&ossConfig, conf.(map[string]any))
		return NewOSSStorage(&ossConfig)
	}

	// 注册 OSS 存储类型 Schema
	RegisterSchema(StorageSchema{
		Type:        "oss",
		Name:        "阿里云 OSS",
		Description: "阿里云对象存储服务",
		Properties:  GenerateSchemaFromStruct(OSSStorageConfig{}),
	})
}
