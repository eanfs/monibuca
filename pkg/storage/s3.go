//go:build s3

package storage

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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
	Endpoint        string        `desc:"S3服务端点"`
	Region          string        `desc:"AWS区域" default:"us-east-1"`
	AccessKeyID     string        `desc:"S3访问密钥ID"`
	SecretAccessKey string        `desc:"S3秘密访问密钥"`
	Bucket          string        `desc:"S3存储桶名称"`
	PathPrefix      string        `desc:"文件路径前缀"`
	ForcePathStyle  bool          `desc:"强制路径样式（MinIO需要）"`
	UseSSL          bool          `desc:"是否使用SSL" default:"true"`
	Timeout         time.Duration `desc:"单次上传超时时间" default:"15m"`
	MaxRetries      int           `desc:"上传失败最大重试次数" default:"3"`
	RetryInterval   time.Duration `desc:"重试基础间隔（指数退避）" default:"5s"`
	PartSize        int64         `desc:"multipart 分片大小（字节），默认 64MB" default:"67108864"`
	ConnectTimeout  time.Duration `desc:"TCP 连接超时" default:"10s"`
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

// getTimeout 获取上传超时时间，默认 15 分钟
func (c *S3StorageConfig) getTimeout() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return 15 * time.Minute
}

// retryConfig 获取重试配置
func (c *S3StorageConfig) retryConfig() RetryConfig {
	return RetryConfig{
		MaxRetries:    c.MaxRetries,
		RetryInterval: c.RetryInterval,
	}
}

// getConnectTimeout 获取 TCP 连接超时，默认 10 秒
func (c *S3StorageConfig) getConnectTimeout() time.Duration {
	if c.ConnectTimeout > 0 {
		return c.ConnectTimeout
	}
	return 10 * time.Second
}

// newHTTPClient 创建带优化配置的 HTTP Client
func (c *S3StorageConfig) newHTTPClient() *http.Client {
	connectTimeout := c.getConnectTimeout()
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   connectTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			TLSHandshakeTimeout:   connectTimeout,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   10,
			ExpectContinueTimeout: 5 * time.Second,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

// S3Storage S3存储实现
type S3Storage struct {
	config     *S3StorageConfig
	s3Client   *s3.S3
	uploader   *s3manager.Uploader
	downloader *s3manager.Downloader
}

// NewS3Storage 创建S3存储实例
func NewS3Storage(cfg *S3StorageConfig) (*S3Storage, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	// 创建AWS配置
	awsConfig := &aws.Config{
		Region:           aws.String(cfg.Region),
		Credentials:      credentials.NewStaticCredentials(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		S3ForcePathStyle: aws.Bool(cfg.ForcePathStyle),
		HTTPClient:       cfg.newHTTPClient(),
	}

	// 设置端点（用于MinIO或其他S3兼容服务）
	if cfg.Endpoint != "" {
		endpoint := cfg.Endpoint
		if !strings.HasPrefix(endpoint, "http") {
			protocol := "http"
			if cfg.UseSSL {
				protocol = "https"
			}
			endpoint = protocol + "://" + endpoint
		}
		awsConfig.Endpoint = aws.String(endpoint)
		awsConfig.DisableSSL = aws.Bool(!cfg.UseSSL)
	}

	// 创建AWS会话
	sess, err := session.NewSession(awsConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create AWS session: %w", err)
	}

	// 创建S3客户端
	s3Client := s3.New(sess)

	// 测试连接
	if err := testS3Connection(s3Client, cfg.Bucket); err != nil {
		return nil, fmt.Errorf("S3 connection test failed: %w", err)
	}

	// 创建 uploader，配置 PartSize
	uploader := s3manager.NewUploader(sess, func(u *s3manager.Uploader) {
		if cfg.PartSize > 0 {
			u.PartSize = cfg.PartSize
		}
	})

	return &S3Storage{
		config:     cfg,
		s3Client:   s3Client,
		uploader:   uploader,
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
	mu        sync.Mutex
	storage   *S3Storage
	objectKey string
	ctx       context.Context
	tempFile  *os.File          // 本地临时文件，用于支持随机访问
	filePath  string            // 临时文件路径
	readOnly  bool              // 只读模式，不上传到S3
	metadata  map[string]string // 用户自定义元数据，上传时携带
}

// SetMetadata 设置上传到 S3 时携带的用户元数据，须在 Close 前调用。
func (w *S3File) SetMetadata(key, value string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.metadata == nil {
		w.metadata = make(map[string]string)
	}
	w.metadata[key] = value
}

func (w *S3File) Name() string {
	return w.objectKey
}

func (w *S3File) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tempFile == nil {
		if err = w.createTempFile(); err != nil {
			return 0, err
		}
	}
	return w.tempFile.Write(p)
}

func (w *S3File) Read(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tempFile == nil {
		if err = w.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return w.tempFile.Read(p)
}

func (w *S3File) WriteAt(p []byte, off int64) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tempFile == nil {
		if err = w.createTempFile(); err != nil {
			return 0, err
		}
	}
	return w.tempFile.WriteAt(p, off)
}

func (w *S3File) ReadAt(p []byte, off int64) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tempFile == nil {
		if err = w.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return w.tempFile.ReadAt(p, off)
}

func (w *S3File) Sync() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readOnly {
		if w.tempFile != nil {
			return w.tempFile.Sync()
		}
		return nil
	}
	if w.tempFile != nil {
		if err := w.tempFile.Sync(); err != nil {
			return err
		}
	}
	return w.uploadTempFile()
}

func (w *S3File) Seek(offset int64, whence int) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.tempFile == nil {
		if err := w.downloadToTemp(); err != nil {
			return 0, err
		}
	}
	return w.tempFile.Seek(offset, whence)
}

func (w *S3File) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.readOnly {
		defer w.cleanup(true)
		if w.tempFile != nil {
			return w.tempFile.Sync()
		}
		return nil
	}
	if w.tempFile != nil {
		if err := w.tempFile.Sync(); err != nil {
			defer w.cleanup(true)
			return err
		}
	}
	err := w.uploadTempFile()
	// 上传失败时保留临时文件（供补传），成功时删除
	w.cleanup(err == nil)
	if err != nil && OnUploadFailed != nil && w.filePath != "" {
		var fileSize int64
		if w.tempFile != nil {
			if stat, statErr := w.tempFile.Stat(); statErr == nil {
				fileSize = stat.Size()
			}
		}
		OnUploadFailed(w.filePath, w.objectKey, "s3", fileSize, w.metadata, err)
	}
	return err
}

// cleanup 清理临时文件。deleteFile=true 时删除磁盘文件，否则仅关闭句柄保留文件。
func (w *S3File) cleanup(deleteFile bool) {
	if w.tempFile != nil {
		w.tempFile.Close()
		w.tempFile = nil
	}
	if deleteFile && w.filePath != "" {
		os.Remove(w.filePath)
		w.filePath = ""
	}
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

// FinalizeFromTemp 让 S3File 直接以 srcPath 指向的完整文件作为上传源，
// 省去调用方「temp → S3File 内部 temp」的全量回拷。
// 后续 Close() 会上传该文件；上传成功删除它，失败保留它供补传。
// 实现 storage.TempFileFinalizer。
func (w *S3File) FinalizeFromTemp(srcPath string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	f, err := adoptUploadTempFile(w.tempFile, w.filePath, srcPath)
	if err != nil {
		w.tempFile = nil
		return fmt.Errorf("finalize from temp: %w", err)
	}
	w.tempFile = f
	w.filePath = srcPath
	return nil
}

// uploadTempFile 上传临时文件到S3，带并发控制和指数退避重试
func (w *S3File) uploadTempFile() error {
	// 解耦上传 ctx 与文件 ctx (= Recorder.Context).
	// Recorder dispose 时其 ctx 被 cancel, 但写好的临时文件应当继续上传完成.
	// WithoutCancel 保留 ctx 的 Values (trace / auth), 但 Done() 永不 close.
	// 上传链路的真实超时由每次 attempt 的 WithTimeout 控制.
	uploadCtx := context.WithoutCancel(w.ctx)

	// 获取上传槽位（并发控制）
	if err := AcquireUploadSlot(uploadCtx); err != nil {
		return fmt.Errorf("acquire upload slot: %w", err)
	}
	defer ReleaseUploadSlot()

	// 重置文件指针到开头
	if _, err := w.tempFile.Seek(0, 0); err != nil {
		return fmt.Errorf("failed to seek temp file: %w", err)
	}

	var fileSize int64
	if stat, err := w.tempFile.Stat(); err == nil {
		fileSize = stat.Size()
	}
	log.Printf("[S3] uploading: bucket=%s key=%s size=%d active=%d/%d",
		w.storage.config.Bucket, w.objectKey, fileSize, GetActiveUploads(), GetMaxConcurrentUploads())

	rc := w.storage.config.retryConfig()

	return UploadWithRetry(uploadCtx, rc, "S3", w.objectKey,
		// resetFn: 每次重试前重置文件指针
		func() error {
			_, err := w.tempFile.Seek(0, 0)
			return err
		},
		// uploadFn: 执行单次上传
		func() error {
			timeout := w.storage.config.getTimeout()
			ctx, cancel := context.WithTimeout(uploadCtx, timeout)
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

			if _, err := w.storage.uploader.UploadWithContext(ctx, uploadInput); err != nil {
				return fmt.Errorf("failed to upload to S3: %w", err)
			}

			log.Printf("[S3] upload successful: %s", w.objectKey)
			return nil
		},
	)
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

var _ TempFileFinalizer = (*S3File)(nil)

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
