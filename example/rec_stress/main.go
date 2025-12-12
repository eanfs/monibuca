//go:build s3

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	yaml "gopkg.in/yaml.v3"
	"m7s.live/v5/pkg/storage"
)

// TaskStatus 任务状态
type TaskStatus int

const (
	StatusPending TaskStatus = iota
	StatusPulling
	StatusRecording
	StatusStopping
	StatusCompleted
	StatusFailed
)

func (s TaskStatus) String() string {
	switch s {
	case StatusPending:
		return "待执行"
	case StatusPulling:
		return "拉流中"
	case StatusRecording:
		return "录制中"
	case StatusStopping:
		return "停止中"
	case StatusCompleted:
		return "已完成"
	case StatusFailed:
		return "失败"
	default:
		return "未知"
	}
}

type successResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type pullProxyInfo struct {
	ID            uint32 `json:"ID,omitempty"`
	ParentID      uint32 `json:"parentID,omitempty"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Status        uint32 `json:"status,omitempty"`
	PullURL       string `json:"pullURL"`
	PullOnStart   bool   `json:"pullOnStart,omitempty"`
	StopOnIdle    bool   `json:"stopOnIdle,omitempty"`
	Audio         bool   `json:"audio,omitempty"`
	Description   string `json:"description,omitempty"`
	RecordPath    string `json:"recordPath,omitempty"`
	StreamPath    string `json:"streamPath"`
	CheckInterval string `json:"checkInterval,omitempty"`
}

type pullProxyListResp struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    []pullProxyInfo `json:"data"`
}

// RecordTask 单个录制任务
type RecordTask struct {
	Index      int
	URL        string
	StreamPath string
	RecPath    string
	Frag       time.Duration
	Total      time.Duration

	// 运行时状态
	Status    TaskStatus
	StartTime time.Time
	EndTime   time.Time
	Error     error
	mu        sync.RWMutex
}

// SetStatus 设置任务状态
func (t *RecordTask) SetStatus(status TaskStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Status = status
}

// GetStatus 获取任务状态
func (t *RecordTask) GetStatus() TaskStatus {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Status
}

// SetError 设置错误信息
func (t *RecordTask) SetError(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Error = err
	if err != nil {
		t.Status = StatusFailed
	}
}

// GetError 获取错误信息
func (t *RecordTask) GetError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.Error
}

type startRecordReq struct {
	Fragment string `json:"fragment,omitempty"`
	FilePath string `json:"filePath,omitempty"`
	FileName string `json:"fileName,omitempty"`
}

type pullReq struct {
	RemoteURL string `json:"remoteURL"`
}

type Config struct {
	Server  string       `yaml:"server"`
	Record  RecordConfig `yaml:"record"`
	S3      S3Config     `yaml:"s3"`
	Sources []Source     `yaml:"sources"`
}

type RecordConfig struct {
	FilePath string        `yaml:"filePath"`
	Fragment time.Duration `yaml:"fragment"`
	Duration time.Duration `yaml:"duration"`
}

type S3Config struct {
	Endpoint        string        `yaml:"endpoint"`
	Region          string        `yaml:"region"`
	AccessKeyID     string        `yaml:"accessKeyID"`
	SecretAccessKey string        `yaml:"secretAccessKey"`
	Bucket          string        `yaml:"bucket"`
	PathPrefix      string        `yaml:"pathPrefix"`
	ForcePathStyle  bool          `yaml:"forcePathStyle"`
	UseSSL          bool          `yaml:"useSSL"`
	Timeout         time.Duration `yaml:"timeout"`
}

type Source struct {
	URL    string       `yaml:"url"`
	Stream string       `yaml:"stream"`
	Record RecordConfig `yaml:"record"`
}

// StressTest 压力测试管理器
type StressTest struct {
	cfg       *Config
	tasks     []*RecordTask
	baseURL   string
	startTime time.Time
	endTime   time.Time

	// 统计信息
	totalTasks    int32
	completedTask int32
	failedTasks   int32
}

// NewStressTest 创建压力测试管理器
func NewStressTest(cfg *Config) *StressTest {
	return &StressTest{
		cfg:     cfg,
		baseURL: strings.TrimRight(cfg.Server, "/"),
	}
}

// PrepareTasks 准备所有任务
func (st *StressTest) PrepareTasks() {
	st.tasks = make([]*RecordTask, 0, len(st.cfg.Sources))
	for i, src := range st.cfg.Sources {
		streamPath := src.Stream
		if streamPath == "" {
			streamPath = fmt.Sprintf("stress/%d", i)
		}
		recPath := src.Record.FilePath
		if recPath == "" {
			recPath = st.cfg.Record.FilePath
		}
		frag := firstNonZero(src.Record.Fragment, st.cfg.Record.Fragment)
		total := firstNonZero(src.Record.Duration, st.cfg.Record.Duration)
		recPath = filepath.ToSlash(filepath.Join(recPath, strings.ReplaceAll(streamPath, "/", "_")))

		task := &RecordTask{
			Index:      i,
			URL:        src.URL,
			StreamPath: streamPath,
			RecPath:    recPath,
			Frag:       frag,
			Total:      total,
			Status:     StatusPending,
		}
		st.tasks = append(st.tasks, task)
	}
	st.totalTasks = int32(len(st.tasks))
}

// RunTask 执行单个任务（仅启动录制）
func (st *StressTest) RunTask(task *RecordTask) {
	task.StartTime = time.Now()
	task.SetStatus(StatusPulling)

	fmt.Printf("[%d/%d] 开始拉流 %s -> %s\n", task.Index+1, st.totalTasks, task.URL, task.StreamPath)

	addURL := fmt.Sprintf("%s/api/proxy/pull/add", st.baseURL)
	payload := pullProxyInfo{
		ParentID:    0,
		Name:        strings.ReplaceAll(task.StreamPath, "/", "-"),
		Type:        "rtsp",
		StreamPath:  task.StreamPath,
		PullURL:     task.URL,
		PullOnStart: true,
	}

	if err := httpPostJSON(addURL, payload); err != nil {
		task.SetError(fmt.Errorf("添加拉流代理失败: %w", err))
		fmt.Printf("[%d/%d] 拉流失败: %v\n", task.Index+1, st.totalTasks, err)
		atomic.AddInt32(&st.failedTasks, 1)
		task.EndTime = time.Now()
		return
	}

	time.Sleep(2 * time.Second)

	task.SetStatus(StatusRecording)
	fmt.Printf("[%d/%d] 开始录制 stream=%s fragment=%s path=%s\n",
		task.Index+1, st.totalTasks, task.StreamPath, task.Frag.String(), task.RecPath)

	startRecURL := fmt.Sprintf("%s/mp4/api/start/%s", st.baseURL, task.StreamPath)
	if err := httpPostJSON(startRecURL, startRecordReq{
		FilePath: task.RecPath,
	}); err != nil {
		task.SetError(fmt.Errorf("启动录制失败: %w", err))
		fmt.Printf("[%d/%d] 录制失败: %v\n", task.Index+1, st.totalTasks, err)
		atomic.AddInt32(&st.failedTasks, 1)

		// 清理拉流代理
		removeURL := fmt.Sprintf("%s/api/proxy/pull/remove/0?streamPath=%s", st.baseURL, task.StreamPath)
		if err := httpPostJSON(removeURL, nil); err != nil {
			fmt.Printf("[%d/%d] 清理拉流代理失败: %v\n", task.Index+1, st.totalTasks, err)
		}

		task.EndTime = time.Now()
		return
	}

	fmt.Printf("[%d/%d] 录制中，持续时间 %s\n", task.Index+1, st.totalTasks, task.Total)
}

// StopTask 停止单个任务的录制
func (st *StressTest) StopTask(task *RecordTask) {
	if task.GetStatus() != StatusRecording {
		return // 只停止正在录制的任务
	}

	task.SetStatus(StatusStopping)
	stopRecURL := fmt.Sprintf("%s/mp4/api/stop/%s", st.baseURL, task.StreamPath)
	if err := httpPostJSON(stopRecURL, nil); err != nil {
		fmt.Printf("[%d/%d] 停止录制失败: %v\n", task.Index+1, st.totalTasks, err)
	}

	removeURL := fmt.Sprintf("%s/api/proxy/pull/remove/0?streamPath=%s", st.baseURL, task.StreamPath)
	if err := httpPostJSON(removeURL, nil); err != nil {
		fmt.Printf("[%d/%d] 移除拉流代理失败: %v\n", task.Index+1, st.totalTasks, err)
	}

	task.EndTime = time.Now()
	task.SetStatus(StatusCompleted)
	atomic.AddInt32(&st.completedTask, 1)
	fmt.Printf("[%d/%d] 任务完成，耗时 %s\n", task.Index+1, st.totalTasks, task.EndTime.Sub(task.StartTime))
}

// StopTasksBatch 分批停止录制任务，每5个一组，每10秒停止一组
func (st *StressTest) StopTasksBatch() {
	fmt.Printf("\n========== 开始分批停止录制 ==========\n")
	
	// 等待录制开始并运行一段时间
	time.Sleep(30 * time.Second)
	
	// 获取所有正在录制的任务
	var recordingTasks []*RecordTask
	for _, task := range st.tasks {
		if task.GetStatus() == StatusRecording {
			recordingTasks = append(recordingTasks, task)
		}
	}
	
	fmt.Printf("正在录制的任务数: %d\n", len(recordingTasks))
	
	// 分批停止，每批5个任务
	batchSize := 5
	for i := 0; i < len(recordingTasks); i += batchSize {
		end := i + batchSize
		if end > len(recordingTasks) {
			end = len(recordingTasks)
		}
		
		batch := recordingTasks[i:end]
		fmt.Printf("\n停止第 %d 批任务 (共 %d 个任务):\n", (i/batchSize)+1, len(batch))
		
		// 并行停止当前批次的任务
		var wg sync.WaitGroup
		for _, task := range batch {
			wg.Add(1)
			go func(t *RecordTask) {
				defer wg.Done()
				st.StopTask(t)
			}(task)
		}
		wg.Wait()
		
		// 如果不是最后一批，等待10秒
		if end < len(recordingTasks) {
			fmt.Printf("等待 10 秒后停止下一批...\n")
			time.Sleep(10 * time.Second)
		}
	}
	
	fmt.Printf("\n========== 所有录制任务已停止 ==========\n")
}

// Run 执行所有任务
func (st *StressTest) Run() error {
	st.startTime = time.Now()
	fmt.Printf("\n========== 压力测试开始 ==========\n")
	fmt.Printf("服务器: %s\n", st.cfg.Server)
	fmt.Printf("任务总数: %d\n", st.totalTasks)
	fmt.Printf("开始时间: %s\n\n", st.startTime.Format("2006-01-02 15:04:05"))

	// 启动所有录制任务
	var wg sync.WaitGroup
	wg.Add(len(st.tasks))

	for _, task := range st.tasks {
		task := task
		go func() {
			defer wg.Done()
			st.RunTask(task)
		}()
	}

	wg.Wait()
	
	// 使用分批停止逻辑
	st.StopTasksBatch()
	
	st.endTime = time.Now()

	return nil
}

// PrintStatistics 打印统计信息
func (st *StressTest) PrintStatistics() {
	fmt.Printf("\n========== 压力测试统计 ==========\n")
	fmt.Printf("总任务数: %d\n", st.totalTasks)
	fmt.Printf("成功任务: %d\n", st.completedTask)
	fmt.Printf("失败任务: %d\n", st.failedTasks)
	fmt.Printf("总耗时: %s\n", st.endTime.Sub(st.startTime))
	fmt.Printf("结束时间: %s\n", st.endTime.Format("2006-01-02 15:04:05"))

	if st.failedTasks > 0 {
		fmt.Printf("\n失败任务详情:\n")
		for _, task := range st.tasks {
			if task.GetStatus() == StatusFailed {
				fmt.Printf("  [%d] %s: %v\n", task.Index+1, task.StreamPath, task.GetError())
			}
		}
	}

	fmt.Printf("\n任务详情:\n")
	for _, task := range st.tasks {
		status := task.GetStatus()
		duration := task.EndTime.Sub(task.StartTime)
		if duration == 0 && status != StatusCompleted {
			duration = time.Since(task.StartTime)
		}
		fmt.Printf("  [%d] %s - 状态: %s, 耗时: %s\n",
			task.Index+1, task.StreamPath, status, duration)
	}
	fmt.Printf("==================================\n\n")
}

// VerifyS3Files 验证S3文件
func (st *StressTest) VerifyS3Files() error {
	fmt.Println("等待 15 秒后开始 S3 文件验证...")
	time.Sleep(15 * time.Second)

	s, err := storage.NewS3Storage(&storage.S3StorageConfig{
		Endpoint:        st.cfg.S3.Endpoint,
		Region:          st.cfg.S3.Region,
		AccessKeyID:     st.cfg.S3.AccessKeyID,
		SecretAccessKey: st.cfg.S3.SecretAccessKey,
		Bucket:          st.cfg.S3.Bucket,
		PathPrefix:      st.cfg.S3.PathPrefix,
		ForcePathStyle:  st.cfg.S3.ForcePathStyle,
		UseSSL:          st.cfg.S3.UseSSL,
		Timeout:         st.cfg.S3.Timeout,
	})
	if err != nil {
		return fmt.Errorf("创建 S3 存储失败: %w", err)
	}
	defer s.Close()

	prefix := "record/mp4/live/"
	files, err := s.List(context.Background(), strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return fmt.Errorf("S3 列表失败: %w", err)
	}

	var count int
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".mp4") {
			count++
		}
	}

	expected := 0
	for _, t := range st.tasks {
		if t.Frag <= 0 {
			continue
		}
		expected += int(t.Total / t.Frag)
	}

	fmt.Printf("\nS3 文件验证结果:\n")
	fmt.Printf("  路径前缀: %s\n", prefix)
	fmt.Printf("  实际文件数: %d\n", count)
	fmt.Printf("  预期文件数: %d\n", expected)

	if count != expected {
		return fmt.Errorf("文件数量不匹配: 实际=%d 预期=%d", count, expected)
	}

	fmt.Println("  验证结果: ✓ 通过")
	return nil
}

func httpPostJSON(url string, body any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}

	// 创建带超时的 context
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// 使用带超时的客户端
	client := &http.Client{
		Timeout: 10 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %s failed: %s", url, strings.TrimSpace(string(b)))
	}
	var sr successResp
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Code != 0 && sr.Code != 200 && sr.Code != 201 {
		return fmt.Errorf("api %s error: code=%d msg=%s", url, sr.Code, sr.Message)
	}
	return nil
}

func main() {
	var configFile = flag.String("config", "config.yaml", "YAML config path")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fmt.Println("加载配置失败:", err)
		os.Exit(2)
	}

	st := NewStressTest(cfg)
	st.PrepareTasks()

	if err := st.Run(); err != nil {
		fmt.Println("执行压力测试失败:", err)
		os.Exit(1)
	}

	st.PrintStatistics()

	if err := st.VerifyS3Files(); err != nil {
		fmt.Println("S3 文件验证失败:", err)
		os.Exit(1)
	}

	fmt.Println("\n压力测试全部完成！")
}

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.S3.Region == "" {
		c.S3.Region = "us-east-1"
	}
	if c.S3.Timeout == 0 {
		c.S3.Timeout = 30 * time.Second
	}
	return &c, nil
}

func firstNonZero(a, b time.Duration) time.Duration {
	if a != 0 {
		return a
	}
	return b
}

// protoDurationString 将 Go duration 转换为 protobuf json 格式，例如 "600s"
func protoDurationString(d time.Duration) string {
	secs := float64(d) / float64(time.Second)
	return fmt.Sprintf("%gs", secs)
}
