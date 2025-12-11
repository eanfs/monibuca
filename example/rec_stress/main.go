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
	"time"

	yaml "gopkg.in/yaml.v3"
	"m7s.live/v5/pkg/storage"
)

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

type task struct {
	Index      int
	URL        string
	StreamPath string
	RecPath    string
	Frag       time.Duration
	Total      time.Duration
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

func httpPostJSON(url string, body any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("http %s failed: %s", url, strings.TrimSpace(string(b)))
	}
	// tolerate different response shapes
	var sr successResp
	_ = json.NewDecoder(resp.Body).Decode(&sr)
	if sr.Code != 0 && sr.Code != 200 && sr.Code != 201 { // consider 0/200/201 as success
		return fmt.Errorf("api %s error: code=%d msg=%s", url, sr.Code, sr.Message)
	}
	return nil
}

func main() {
	var configFile = flag.String("config", "config.yaml", "YAML config path")
	flag.Parse()

	cfg, err := loadConfig(*configFile)
	if err != nil {
		fmt.Println("load config failed:", err)
		os.Exit(2)
	}

	base := strings.TrimRight(cfg.Server, "/")

	// prepare tasks first so we know where to verify later
	tasks := make([]task, 0, len(cfg.Sources))
	for i, src := range cfg.Sources {
		streamPath := src.Stream
		if streamPath == "" {
			streamPath = fmt.Sprintf("stress/%d", i)
		}
		recPath := src.Record.FilePath
		if recPath == "" {
			recPath = cfg.Record.FilePath
		}
		frag := firstNonZero(src.Record.Fragment, cfg.Record.Fragment)
		total := firstNonZero(src.Record.Duration, cfg.Record.Duration)
		// isolate dir per stream
		recPath = filepath.ToSlash(filepath.Join(recPath, strings.ReplaceAll(streamPath, "/", "_")))
		tasks = append(tasks, task{Index: i, URL: src.URL, StreamPath: streamPath, RecPath: recPath, Frag: frag, Total: total})
	}

	var wg sync.WaitGroup
	wg.Add(len(tasks))
	for _, t := range tasks {
		t := t // capture
		go func() {
			defer wg.Done()
			fmt.Printf("[%d/%d] Start pull %s -> %s\n", t.Index+1, len(tasks), t.URL, t.StreamPath)
			addURL := fmt.Sprintf("%s/api/proxy/pull/add", base)
			payload := pullProxyInfo{
				ParentID:    0,
				Name:        strings.ReplaceAll(t.StreamPath, "/", "-"),
				Type:        "rtsp",
				StreamPath:  t.StreamPath,
				PullURL:     t.URL,
				PullOnStart: true,
			}
			if err := httpPostJSON(addURL, payload); err != nil {
				fmt.Println("  add pull proxy failed:", err)
				return
			}
			// wait 2s after pull
			time.Sleep(2 * time.Second)

			fmt.Printf("  Start record stream=%s fragment=%s filePath=%s\n", t.StreamPath, t.Frag.String(), t.RecPath)
			startRecURL := fmt.Sprintf("%s/mp4/api/start/%s", base, t.StreamPath)
			if err := httpPostJSON(startRecURL, startRecordReq{Fragment: protoDurationString(t.Frag), FilePath: t.RecPath}); err != nil {
				fmt.Println("  start record failed:", err)
				return
			}

			fmt.Println("  Recording for", t.Total)
			time.Sleep(t.Total)

			stopRecURL := fmt.Sprintf("%s/mp4/api/stop/%s", base, t.StreamPath)
			if err := httpPostJSON(stopRecURL, nil); err != nil {
				fmt.Println("  stop record failed:", err)
			}
			removeURL := fmt.Sprintf("%s/api/proxy/pull/remove/0?streamPath=%s", base, t.StreamPath)
			if err := httpPostJSON(removeURL, nil); err != nil {
				fmt.Println("  remove pull proxy failed:", err)
			}
		}()
	}
	wg.Wait()

	// After all recordings finished, wait 90s then verify S3 files under the global directory
	fmt.Println("All recordings finished. Waiting 90s before S3 verification...")
	time.Sleep(90 * time.Second)
	if err := verifyS3Total(cfg, tasks, "record/mp4/live/"); err != nil {
		fmt.Println("S3 total verification failed:", err)
		os.Exit(1)
	}
	fmt.Println("S3 total verification succeeded")
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

// convert Go duration to protobuf json format, e.g. "600s"
func protoDurationString(d time.Duration) string {
	// protobuf JSON uses seconds with "s" suffix, supports fractional seconds
	secs := float64(d) / float64(time.Second)
	return fmt.Sprintf("%gs", secs)
}

// verifyS3Total lists all objects under a fixed prefix and validates the total .mp4 file count
func verifyS3Total(cfg *Config, tasks []task, prefix string) error {
	s, err := storage.NewS3Storage(&storage.S3StorageConfig{
		Endpoint:        cfg.S3.Endpoint,
		Region:          cfg.S3.Region,
		AccessKeyID:     cfg.S3.AccessKeyID,
		SecretAccessKey: cfg.S3.SecretAccessKey,
		Bucket:          cfg.S3.Bucket,
		PathPrefix:      cfg.S3.PathPrefix,
		ForcePathStyle:  cfg.S3.ForcePathStyle,
		UseSSL:          cfg.S3.UseSSL,
		Timeout:         cfg.S3.Timeout,
	})
	if err != nil {
		return fmt.Errorf("NewS3Storage: %w", err)
	}
	defer s.Close()

	files, err := s.List(context.Background(), strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return fmt.Errorf("S3 List: %w", err)
	}
	var count int
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".mp4") {
			count++
		}
	}

	// expected total = sum over each task of (floor(total/frag)+1)
	expected := 0
	for _, t := range tasks {
		if t.Frag <= 0 {
			continue
		}
		expected += int(t.Total/t.Frag) + 1
	}

	fmt.Printf("S3 total .mp4 under '%s': %d, expected: %d\n", prefix, count, expected)
	if count != expected {
		return fmt.Errorf("file count mismatch: got=%d expected=%d", count, expected)
	}
	return nil
}

// verifyS3 lists recPath and verifies .mp4 fragment file count.
func verifyS3(endpoint, region, ak, sk, bucket, pathPrefix, recPath string, useSSL, forcePathStyle bool, timeout time.Duration, total, frag time.Duration) error {
	if ak == "" || sk == "" || bucket == "" {
		return fmt.Errorf("missing S3 credentials/bucket; provide via flags or env")
	}
	conf := &storage.S3StorageConfig{
		Endpoint:        endpoint,
		Region:          region,
		AccessKeyID:     ak,
		SecretAccessKey: sk,
		Bucket:          bucket,
		PathPrefix:      pathPrefix,
		ForcePathStyle:  forcePathStyle,
		UseSSL:          useSSL,
		Timeout:         timeout,
	}
	s, err := storage.NewS3Storage(conf)
	if err != nil {
		return fmt.Errorf("NewS3Storage: %w", err)
	}
	defer s.Close()

	// wait a little for last upload to finish
	time.Sleep(2 * time.Second)

	// List all objects under recPath
	files, err := s.List(context.Background(), strings.TrimSuffix(recPath, "/"))
	if err != nil {
		return fmt.Errorf("S3 List: %w")
	}
	// count only .mp4 files
	var count int
	for _, f := range files {
		if strings.HasSuffix(strings.ToLower(f.Name), ".mp4") {
			count++
		}
	}

	// Expected count = floor(total/frag) + 1
	expected := int(total / frag)
	if expected < 0 {
		expected = 0
	}
	expected = expected + 1

	if count != expected {
		return fmt.Errorf("file count mismatch: got=%d expected=%d (total=%s, fragment=%s)", count, expected, total, frag)
	}
	fmt.Printf("S3 files under '%s': %d, expected: %d\n", recPath, count, expected)
	return nil
}
