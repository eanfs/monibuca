//go:build s3

package storage

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"m7s.live/v5/pkg/config"
)

// isS3NotFoundError 使用 AWS SDK 类型断言判断是否为 404 错误，避免脆弱的字符串匹配
func isS3NotFoundError(err error) bool {
	if aerr, ok := err.(awserr.Error); ok {
		switch aerr.Code() {
		case s3.ErrCodeNoSuchKey, "NotFound", "NoSuchBucket":
			return true
		}
	}
	return false
}

// S3StorageConfig S3存储配置
type S3StorageConfig struct {
	Endpoint           string        `desc:"S3服务端点"`
	Region             string        `desc:"AWS区域" default:"us-east-1"`
	AccessKeyID        string        `desc:"S3访问密钥ID"`
	SecretAccessKey    string        `desc:"S3秘密访问密钥"`
	Bucket             string        `desc:"S3存储桶名称"`
	PathPrefix         string        `desc:"文件路径前缀"`
	ForcePathStyle     bool          `desc:"强制路径样式（MinIO需要）"`
	UseSSL             bool          `desc:"是否使用SSL" default:"true"`
	Timeout            time.Duration `desc:"基础上传超时时间" default:"60s"`
	TimeoutPerMB       time.Duration `desc:"每MB额外超时时间，用于大文件动态计算超时" default:"3s"`
	MaxTimeout         time.Duration `desc:"最大超时时间限制" default:"15m"`
	UploadPartSize     int64         `desc:"multipart上传分片大小(字节)，大文件建议64MB(67108864)" default:"67108864"`
	UploadConcurrency  int           `desc:"单文件multipart并发分片数，建议1-2避免大文件占满带宽" default:"1"`
}

func (c *S3StorageConfig) GetType() StorageType {
	return StorageTypeS3
}

func (c *S3StorageConfig) Validate() error {
	if c.AccessKeyID == "" {
		return fmt.Errorf("access_key_id is required for S3 storage")
	}
	if c.SecretAccessKey == "" {
		return fmt.Errorf("secret_access_key is required for S3 storage")
	}
	if c.Bucket == "" {
		return fmt.Errorf("bucket is required for S3 storage")
	}
	return nil
}

// S3Storage S3存储实现
type S3Storage struct {
	config     *S3StorageConfig
	s3Client   *s3.S3
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
}

// NewS3Storage 创建S3存储实例
func NewS3Storage(config *S3StorageConfig) (*S3Storage, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}

	// 创建AWS配置
	awsConfig := &aws.Config{
		Region:           aws.String(config.Region),
		Credentials:      credentials.NewStaticCredentials(config.AccessKeyID, config.SecretAccessKey, ""),
		S3ForcePathStyle: aws.Bool(config.ForcePathStyle),
	}

	// 设置端点（用于MinIO或其他S3兼容服务）
	if config.Endpoint != "" {
		endpoint := config.Endpoint
		if !strings.HasPrefix(endpoint, "http") {
			protocol := "http"
			if config.UseSSL {
				protocol = "https"
			}
			endpoint = protocol + "://" + endpoint
		}
		awsConfig.Endpoint = aws.String(endpoint)
		awsConfig.DisableSSL = aws.Bool(!config.UseSSL)
	}

	// 创建AWS会话
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %w", err)
	}

	// 创建S3客户端
	s3Client := s3.New(sess)

	// 测试连接
	if err := testS3Connection(s3Client, config.Bucket); err != nil {
		return nil, fmt.Errorf("S3 connection test failed: %w", err)
	}

	return &S3Storage{
		config:     config,
		s3Client:   s3Client,
		uploader: s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
			if config.UploadPartSize > 0 {
				u.PartSize = config.UploadPartSize
			}
			if config.UploadConcurrency > 0 {
				u.Concurrency = config.UploadConcurrency
			}
		}),
		downloader: s3manager.NewDownloader(sess),
	}, nil
}
func (s *S3Storage) GetKey() string {
	return "s3"
}
func (s *S3Storage) CreateFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &S3File{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
		readOnly:  false,
	}, nil
}

func (s *S3Storage) OpenFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	return &S3File{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
		readOnly:  true, // 只读模式
	}, nil
}

func (s *S3Storage) Delete(ctx context.Context, path string) error {
	objectKey := s.getObjectKey(path)

	_, err := s.s3Client.DeleteObjectWithContext(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(objectKey),
	})

	return err
}

func (s *S3Storage) Exists(ctx context.Context, path string) (bool, error) {
	objectKey := s.getObjectKey(path)

	_, err := s.s3Client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(objectKey),
	})

	if err != nil {
		if isS3NotFoundError(err) {
			return false, nil
		}
		return false, err
	}

	return true, nil
}

func (s *S3Storage) GetSize(ctx context.Context, path string) (int64, error) {
	objectKey := s.getObjectKey(path)

	result, err := s.s3Client.HeadObjectWithContext(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(objectKey),
	})

	if err != nil {
		if isS3NotFoundError(err) {
			return 0, ErrFileNotFound
		}
		return 0, err
	}

	if result.ContentLength == nil {
		return 0, nil
	}

	return *result.ContentLength, nil
}

func (s *S3Storage) GetURL(ctx context.Context, path string) (string, error) {
	objectKey := s.getObjectKey(path)

	req, _ := s.s3Client.GetObjectRequest(&s3.GetObjectInput{
		Bucket: aws.String(s.config.Bucket),
		Key:    aws.String(objectKey),
	})

	url, err := req.Presign(24 * time.Hour) // 24小时有效期
	if err != nil {
		return "", err
	}

	return url, nil
}

func (s *S3Storage) List(ctx context.Context, prefix string) ([]FileInfo, error) {
	objectPrefix := s.getObjectKey(prefix)

	var files []FileInfo

	err := s.s3Client.ListObjectsV2PagesWithContext(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.config.Bucket),
		Prefix: aws.String(objectPrefix),
	}, func(page *s3.ListObjectsV2Output, lastPage bool) bool {
		for _, obj := range page.Contents {
			// 移除路径前缀
			fileName := *obj.Key
			if s.config.PathPrefix != "" {
				fileName = strings.TrimPrefix(fileName, strings.TrimSuffix(s.config.PathPrefix, "/")+"/")
			}

			files = append(files, FileInfo{
				Name:         fileName,
				Size:         *obj.Size,
				LastModified: *obj.LastModified,
				ETag:         *obj.ETag,
			})
		}
		return true
	})

	return files, err
}

func (s *S3Storage) Close() error {
	// S3客户端无需显式关闭
	return nil
}

// getObjectKey 获取S3对象键
func (s *S3Storage) getObjectKey(path string) string {
	if s.config.PathPrefix != "" {
		return strings.TrimSuffix(s.config.PathPrefix, "/") + "/" + path
	}
	return path
}

// testS3Connection 测试S3连接
func testS3Connection(s3Client *s3.S3, bucket string) error {
	_, err := s3Client.HeadBucket(&s3.HeadBucketInput{
		Bucket: aws.String(bucket),
	})
	return err
}

// S3File S3文件读写器
type S3File struct {
	storage      *S3Storage
	objectKey    string
	ctx          context.Context
	tempFile     *os.File          // 本地临时文件，用于支持随机访问
	filePath     string            // 临时文件路径
	readOnly     bool              // 只读模式，不上传到S3
	metadata     map[string]string // 用户自定义元数据，上传时携带
	uploadFailed bool              // 标记上传是否失败，用于重试时保留临时文件
}

// SetMetadata 设置上传到 S3 时携带的用户元数据，须在 Close 前调用。
func (w *S3File) SetMetadata(key, value string) {
	if w.metadata == nil {
		w.metadata = make(map[string]string)
	}
	w.metadata[key] = value
}

func (w *S3File) Name() string {
	return w.objectKey
}

func (w *S3File) Write(p []byte) (n int, err error) {
	// 如果还没有创建临时文件，先创建
	if w.tempFile == nil {
		if err = w.createTempFile(); err != nil {
			return 0, err
		}
	}

	// 写入到临时文件
	return w.tempFile.Write(p)
}

func (w *S3File) Read(p []byte) (n int, err error) {
	// 如果还没有创建缓存文件，先下载到本地
	if w.tempFile == nil {
		if err = w.downloadToTemp(); err != nil {
			return 0, err
		}
	}

	// 从本地缓存文件读取
	return w.tempFile.Read(p)
}

func (w *S3File) WriteAt(p []byte, off int64) (n int, err error) {
	// 如果还没有创建临时文件，先创建
	if w.tempFile == nil {
		if err = w.createTempFile(); err != nil {
			return 0, err
		}
	}

	// 写入到临时文件的指定位置
	return w.tempFile.WriteAt(p, off)
}

func (w *S3File) ReadAt(p []byte, off int64) (n int, err error) {
	// 如果还没有创建缓存文件，先下载到本地
	if w.tempFile == nil {
		if err = w.downloadToTemp(); err != nil {
			return 0, err
		}
	}

	// 从本地缓存文件的指定位置读取
	return w.tempFile.ReadAt(p, off)
}

func (w *S3File) Sync() error {
	// 只读模式不上传
	if w.readOnly {
		if w.tempFile != nil {
			return w.tempFile.Sync()
		}
		return nil
	}

	// 先将临时文件同步到磁盘
	if w.tempFile != nil {
		if err := w.tempFile.Sync(); err != nil {
			return err
		}
	}
	return w.uploadTempFile()
}

func (w *S3File) Seek(offset int64, whence int) (int64, error) {
	// 如果还没有创建临时文件，先创建或下载
	if w.tempFile == nil {
		if err := w.downloadToTemp(); err != nil {
			return 0, err
		}
	}

	// 使用临时文件进行随机访问
	return w.tempFile.Seek(offset, whence)
}

func (w *S3File) Close() error {
	var errs []error

	// 先执行上传
	if uploadErr := w.Sync(); uploadErr != nil {
		errs = append(errs, uploadErr)
	}

	// 清理临时文件句柄
	if w.tempFile != nil {
		if closeErr := w.tempFile.Close(); closeErr != nil {
			errs = append(errs, fmt.Errorf("close temp file: %w", closeErr))
		}
		w.tempFile = nil
	}

	// 只有上传成功时才删除临时文件
	// 上传失败时保留临时文件，以便重试
	if len(errs) == 0 {
		// 上传成功，删除临时文件
		if w.filePath != "" {
			if removeErr := os.Remove(w.filePath); removeErr != nil && !os.IsNotExist(removeErr) {
				errs = append(errs, fmt.Errorf("remove temp file: %w", removeErr))
			}
			w.filePath = ""
		}
	} else {
		// 上传失败，标记状态并保留临时文件
		w.uploadFailed = true
		log.Printf("[S3] upload failed, temp file preserved for retry: %s", w.filePath)
	}

	// 返回第一个错误（通常是上传错误）
	if len(errs) > 0 {
		return errs[0]
	}
	return nil
}

// Reopen 重新打开文件以支持上传重试
func (w *S3File) Reopen() error {
	if w.filePath == "" {
		return fmt.Errorf("no temp file to reopen")
	}

	// 关闭旧的文件句柄（如果存在）
	if w.tempFile != nil {
		w.tempFile.Close()
	}

	// 重新打开临时文件
	tempFile, err := os.OpenFile(w.filePath, os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("failed to reopen temp file: %w", err)
	}

	w.tempFile = tempFile
	w.uploadFailed = false
	log.Printf("[S3] temp file reopened for retry: %s", w.filePath)
	return nil
}

// CleanupTempFile 清理临时文件（用于所有重试失败后的最终清理）
func (w *S3File) CleanupTempFile() error {
	var errs []error

	// 关闭文件句柄
	if w.tempFile != nil {
		if closeErr := w.tempFile.Close(); closeErr != nil {
			errs = append(errs, fmt.Errorf("close temp file: %w", closeErr))
		}
		w.tempFile = nil
	}

	// 删除临时文件
	if w.filePath != "" {
		if removeErr := os.Remove(w.filePath); removeErr != nil && !os.IsNotExist(removeErr) {
			errs = append(errs, fmt.Errorf("remove temp file: %w", removeErr))
		} else {
			log.Printf("[S3] temp file cleaned up: %s", w.filePath)
		}
		w.filePath = ""
	}

	// 返回所有错误
	if len(errs) > 0 {
		return fmt.Errorf("cleanup errors: %v", errs)
	}
	return nil
}

// createTempFile 创建临时文件
func (w *S3File) createTempFile() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "s3writer_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	w.tempFile = tempFile
	w.filePath = tempFile.Name()
	return nil
}

func (w *S3File) Stat() (os.FileInfo, error) {
	if w.tempFile == nil {
		return nil, fmt.Errorf("s3 file not initialized")
	}
	return w.tempFile.Stat()
}

// uploadTempFile 上传临时文件到S3
func (w *S3File) uploadTempFile() (err error) {
	// 重置文件指针到开头
	if _, err = w.tempFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek temp file: %w", err)
	}

	stat, _ := w.tempFile.Stat()
	fileSize := stat.Size()

	// 计算动态超时时间
	timeout := w.calculateTimeout(fileSize)
	log.Printf("[S3] uploading: bucket=%s key=%s size=%d timeout=%s",
		w.storage.config.Bucket, w.objectKey, fileSize, timeout)

	// 使用带超时的 background context，避免因录像 context 取消而中断上传
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	uploadInput := &s3manager.UploadInput{
		Bucket:      aws.String(w.storage.config.Bucket),
		Key:         aws.String(w.objectKey),
		Body:        w.tempFile,
		ContentType: aws.String("application/octet-stream"),
	}
	if len(w.metadata) > 0 {
		uploadInput.Metadata = aws.StringMap(w.metadata)
	}

	if _, err = w.storage.uploader.UploadWithContext(ctx, uploadInput); err != nil {
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	log.Printf("[S3] upload successful: %s (size=%d, timeout=%s)", w.objectKey, fileSize, timeout)
	return nil
}

// calculateTimeout 根据文件大小动态计算超时时间
// 公式：timeout = min(baseTimeout + fileSizeMB * timeoutPerMB, maxTimeout)
func (w *S3File) calculateTimeout(fileSize int64) time.Duration {
	config := w.storage.config

	// 基础超时
	baseTimeout := config.Timeout
	if baseTimeout <= 0 {
		baseTimeout = 60 * time.Second
	}

	// 如果没有配置动态超时，直接返回基础超时
	if config.TimeoutPerMB <= 0 {
		return baseTimeout
	}

	// 计算文件大小（MB）
	fileSizeMB := float64(fileSize) / (1024 * 1024)

	// 动态超时 = 基础超时 + 文件大小(MB) × 每MB超时
	dynamicTimeout := baseTimeout + time.Duration(fileSizeMB*float64(config.TimeoutPerMB))

	// 应用最大超时限制
	maxTimeout := config.MaxTimeout
	if maxTimeout <= 0 {
		maxTimeout = 15 * time.Minute
	}

	if dynamicTimeout > maxTimeout {
		log.Printf("[S3] calculated timeout %s exceeds max %s, using max timeout",
			dynamicTimeout, maxTimeout)
		return maxTimeout
	}

	return dynamicTimeout
}

// downloadToTemp 下载S3对象到本地临时文件
func (w *S3File) downloadToTemp() error {
	tempFile, err := os.CreateTemp("", "s3reader_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	w.tempFile = tempFile
	w.filePath = tempFile.Name()

	_, err = w.storage.downloader.DownloadWithContext(w.ctx, tempFile, &s3.GetObjectInput{
		Bucket: aws.String(w.storage.config.Bucket),
		Key:    aws.String(w.objectKey),
	})

	if err != nil {
		tempFile.Close()
		os.Remove(w.filePath)
		if isS3NotFoundError(err) {
			return ErrFileNotFound
		}
		return fmt.Errorf("failed to download from S3: %w", err)
	}

	// 重置文件指针到开始位置
	if _, err = tempFile.Seek(0, 0); err != nil {
		tempFile.Close()
		os.Remove(w.filePath)
		return fmt.Errorf("failed to seek temp file: %w", err)
	}

	return nil
}

func init() {
	Factory["s3"] = func(conf any) (Storage, error) {
		var s3Config S3StorageConfig
		config.Parse(&s3Config, conf.(map[string]any))
		return NewS3Storage(&s3Config)
	}

	// 注册 S3 存储类型 Schema
	RegisterSchema(StorageSchema{
		Type:        "s3",
		Name:        "S3 存储",
		Description: "AWS S3 或兼容 S3 协议的对象存储（如 MinIO）",
		Properties:  GenerateSchemaFromStruct(S3StorageConfig{}),
	})
}
