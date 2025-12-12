//go:build s3

package storage

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"m7s.live/v5/pkg/config"
)

// S3StorageConfig S3存储配置
type S3StorageConfig struct {
	Endpoint             string        `desc:"S3服务端点"`
	Region               string        `desc:"AWS区域" default:"us-east-1"`
	AccessKeyID          string        `desc:"S3访问密钥ID"`
	SecretAccessKey      string        `desc:"S3秘密访问密钥"`
	Bucket               string        `desc:"S3存储桶名称"`
	PathPrefix           string        `desc:"文件路径前缀"`
	ForcePathStyle       bool          `desc:"强制路径样式（MinIO需要）"`
	UseSSL               bool          `desc:"是否使用SSL" default:"true"`
	Timeout              time.Duration `desc:"上传超时时间" default:"30s"`
	Concurrency          int           `desc:"单个文件上传时的并发分片数" default:"10"`
	PartSize             int64         `desc:"分片大小(字节)" default:"10485760"` // 默认10MB
	MaxConcurrentUploads int           `desc:"同时上传的最大文件数量" default:"8"`
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
	logger     *slog.Logger

	// 并发控制
	uploadSemaphore chan struct{} // 信号量，控制同时上传的文件数量

	// 并发追踪
	activeUploads  int32 // 当前活跃的上传数量
	totalUploads   int32 // 总上传数量
	successUploads int32 // 成功上传数量
	failedUploads  int32 // 失败上传数量
	uploadMutex    sync.Mutex
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

	// 创建 logger
	logger := slog.Default().With("component", "S3Storage")

	// 配置 Uploader 的并发参数
	uploader := s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		u.Concurrency = config.Concurrency
		u.PartSize = config.PartSize
		u.LeavePartsOnError = false // 上传失败时清理分片
	})

	// 创建信号量，控制同时上传的文件数量
	uploadSemaphore := make(chan struct{}, config.MaxConcurrentUploads)

	logger.Info("S3Storage initialized",
		"bucket", config.Bucket,
		"region", config.Region,
		"endpoint", config.Endpoint,
		"concurrency", config.Concurrency,
		"partSize", config.PartSize,
		"maxConcurrentUploads", config.MaxConcurrentUploads)

	return &S3Storage{
		config:          config,
		s3Client:        s3Client,
		uploader:        uploader,
		downloader:      s3manager.NewDownloader(sess),
		logger:          logger,
		uploadSemaphore: uploadSemaphore,
	}, nil
}

func (s *S3Storage) CreateFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	s.logger.Debug("CreateFile called", "path", path, "objectKey", objectKey)
	return &S3File{
		storage:   s,
		objectKey: objectKey,
		ctx:       ctx,
		readOnly:  false,
	}, nil
}

func (s *S3Storage) OpenFile(ctx context.Context, path string) (File, error) {
	objectKey := s.getObjectKey(path)
	s.logger.Debug("OpenFile called", "path", path, "objectKey", objectKey)
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
		// 检查是否是404错误
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "NoSuchKey") {
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
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "NoSuchKey") {
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
	// 记录最终统计信息
	s.logger.Info("S3Storage closing",
		"totalUploads", atomic.LoadInt32(&s.totalUploads),
		"successUploads", atomic.LoadInt32(&s.successUploads),
		"failedUploads", atomic.LoadInt32(&s.failedUploads),
		"activeUploads", atomic.LoadInt32(&s.activeUploads))
	// S3客户端无需显式关闭
	return nil
}

// GetUploadStats 获取上传统计信息
func (s *S3Storage) GetUploadStats() (total, success, failed, active int32) {
	return atomic.LoadInt32(&s.totalUploads),
		atomic.LoadInt32(&s.successUploads),
		atomic.LoadInt32(&s.failedUploads),
		atomic.LoadInt32(&s.activeUploads)
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
	storage   *S3Storage
	objectKey string
	ctx       context.Context
	tempFile  *os.File // 本地临时文件，用于支持随机访问
	filePath  string   // 临时文件路径
	readOnly  bool     // 只读模式，不上传到S3
}

func (w *S3File) Name() string {
	return w.objectKey
}

func (w *S3File) Write(p []byte) (n int, err error) {
	// 如果还没有创建临时文件，先创建
	if w.tempFile == nil {
		if err = w.createTempFile(); err != nil {
			w.storage.logger.Error("Write: failed to create temp file", "objectKey", w.objectKey, "err", err)
			return 0, err
		}
	}

	// 写入到临时文件
	n, err = w.tempFile.Write(p)
	if err != nil {
		w.storage.logger.Error("Write: failed to write to temp file", "objectKey", w.objectKey, "size", len(p), "err", err)
	}
	return n, err
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

	// 如果使用临时文件，先同步到磁盘
	if w.tempFile != nil {
		if err := w.tempFile.Sync(); err != nil {
			w.storage.logger.Error("Sync: failed to sync temp file to disk", "objectKey", w.objectKey, "err", err)
			return err
		}
		// 获取文件大小用于日志
		if stat, err := w.tempFile.Stat(); err == nil {
			w.storage.logger.Info("Sync: temp file synced to disk",
				"objectKey", w.objectKey,
				"size", stat.Size(),
				"path", w.filePath)
		}
	}

	// 上传到 S3
	if err := w.uploadTempFile(); err != nil {
		w.storage.logger.Error("Sync: upload to S3 failed", "objectKey", w.objectKey, "err", err)
		return err
	}
	return nil
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
	w.storage.logger.Debug("Close: starting", "objectKey", w.objectKey, "readOnly", w.readOnly)

	var syncErr error
	if err := w.Sync(); err != nil {
		syncErr = err
		w.storage.logger.Error("Close: Sync failed", "objectKey", w.objectKey, "err", err)
	}

	// 关闭临时文件
	if w.tempFile != nil {
		if err := w.tempFile.Close(); err != nil {
			w.storage.logger.Error("Close: failed to close temp file", "objectKey", w.objectKey, "err", err)
			if syncErr == nil {
				syncErr = err
			}
		}
	}

	// 只有在上传成功（或只读模式）时才清理临时文件
	if w.filePath != "" {
		if syncErr == nil || w.readOnly {
			if err := os.Remove(w.filePath); err != nil {
				w.storage.logger.Warn("Close: failed to remove temp file", "path", w.filePath, "err", err)
			} else {
				w.storage.logger.Debug("Close: temp file removed", "path", w.filePath)
			}
		} else {
			// 上传失败，保留临时文件以便后续重试或手动处理
			w.storage.logger.Warn("Close: temp file preserved due to upload failure",
				"path", w.filePath,
				"objectKey", w.objectKey,
				"err", syncErr)
		}
	}

	w.storage.logger.Debug("Close: completed", "objectKey", w.objectKey, "success", syncErr == nil)
	return syncErr
}

// createTempFile 创建临时文件
func (w *S3File) createTempFile() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "s3writer_*.tmp")
	if err != nil {
		w.storage.logger.Error("createTempFile: failed to create temp file", "objectKey", w.objectKey, "err", err)
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	w.tempFile = tempFile
	w.filePath = tempFile.Name()
	w.storage.logger.Debug("createTempFile: temp file created", "objectKey", w.objectKey, "path", w.filePath)
	return nil
}

func (w *S3File) Stat() (os.FileInfo, error) {
	return w.tempFile.Stat()
}

// uploadTempFile 上传临时文件到S3
func (w *S3File) uploadTempFile() (err error) {
	if w.tempFile == nil {
		w.storage.logger.Debug("uploadTempFile: no temp file to upload", "objectKey", w.objectKey)
		return nil
	}

	// 获取信号量，控制并发上传数量
	// 如果已达到最大并发数，这里会阻塞等待
	w.storage.uploadSemaphore <- struct{}{}
	defer func() {
		// 释放信号量
		<-w.storage.uploadSemaphore
	}()

	// 增加并发计数
	activeCount := atomic.AddInt32(&w.storage.activeUploads, 1)
	totalCount := atomic.AddInt32(&w.storage.totalUploads, 1)

	defer func() {
		// 减少并发计数
		atomic.AddInt32(&w.storage.activeUploads, -1)

		// 更新统计
		if err == nil {
			atomic.AddInt32(&w.storage.successUploads, 1)
		} else {
			atomic.AddInt32(&w.storage.failedUploads, 1)
		}
	}()

	// 重置文件指针到开头
	if _, err := w.tempFile.Seek(0, 0); err != nil {
		w.storage.logger.Error("uploadTempFile: failed to seek", "objectKey", w.objectKey, "err", err)
		return fmt.Errorf("failed to seek temp file: %w", err)
	}

	// 获取文件大小
	stat, _ := w.tempFile.Stat()
	fileSize := int64(0)
	if stat != nil {
		fileSize = stat.Size()
	}

	// 计算队列中等待的任务数量
	queuedUploads := len(w.storage.uploadSemaphore)

	w.storage.logger.Info("uploadTempFile: starting upload",
		"objectKey", w.objectKey,
		"bucket", w.storage.config.Bucket,
		"size", fileSize,
		"activeUploads", activeCount,
		"queuedUploads", queuedUploads,
		"maxConcurrent", w.storage.config.MaxConcurrentUploads,
		"totalUploads", totalCount,
		"tempPath", w.filePath)

	// 上传到S3
	startTime := time.Now()
	_, err = w.storage.uploader.UploadWithContext(w.ctx, &s3manager.UploadInput{
		Bucket:      aws.String(w.storage.config.Bucket),
		Key:         aws.String(w.objectKey),
		Body:        w.tempFile,
		ContentType: aws.String("application/octet-stream"),
	})
	duration := time.Since(startTime)

	if err != nil {
		w.storage.logger.Error("uploadTempFile: upload failed",
			"objectKey", w.objectKey,
			"size", fileSize,
			"duration", duration,
			"activeUploads", activeCount-1,
			"err", err)
		return fmt.Errorf("failed to upload to S3: %w", err)
	}

	w.storage.logger.Info("uploadTempFile: upload successful",
		"objectKey", w.objectKey,
		"size", fileSize,
		"duration", duration,
		"activeUploads", activeCount-1,
		"successCount", atomic.LoadInt32(&w.storage.successUploads),
		"failedCount", atomic.LoadInt32(&w.storage.failedUploads))

	return nil
}

// downloadToTemp 下载S3对象到本地临时文件
func (w *S3File) downloadToTemp() error {
	// 创建临时文件
	tempFile, err := os.CreateTemp("", "s3reader_*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	w.tempFile = tempFile
	w.filePath = tempFile.Name()

	// 下载S3对象
	_, err = w.storage.downloader.DownloadWithContext(w.ctx, tempFile, &s3.GetObjectInput{
		Bucket: aws.String(w.storage.config.Bucket),
		Key:    aws.String(w.objectKey),
	})

	if err != nil {
		tempFile.Close()
		os.Remove(w.filePath)
		if strings.Contains(err.Error(), "NotFound") || strings.Contains(err.Error(), "NoSuchKey") {
			return ErrFileNotFound
		}
		return fmt.Errorf("failed to download from S3: %w", err)
	}

	// 重置文件指针到开始位置
	_, err = tempFile.Seek(0, 0)
	if err != nil {
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
}
