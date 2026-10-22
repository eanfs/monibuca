package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"m7s.live/v5"
	_ "m7s.live/v5/plugin/mp4"
	mp4pkg "m7s.live/v5/plugin/mp4/pkg"
	_ "m7s.live/v5/plugin/rtmp"
)

// TestRecoveryAfterKill 端到端验收录制完整性承诺：
//
//	推流 → 开录 → SIGKILL 子进程（模拟断电/进程被杀）→ 重启 → 无需人工介入，
//	启动恢复任务用伴随索引重建可播放 MP4，并删除 sidecar。
//
// 本机没有 MinIO/S3，因此走 local 存储：S3 解析/上传分支已由 Task 6 的单元测试
// 覆盖（plugin/mp4/pkg/recovery_upload_test.go）。崩因由真实子进程承担——测试
// 二进制以 helper 模式重新执行自身（见 recoveryChildEnv），父进程 kill -9 它。
//
// 依赖 ffmpeg/ffprobe：缺失时跳过（明确原因），-short 也跳过。
const (
	recoveryChildEnv  = "M7S_RECOVERY_CHILD"
	recoveryConfigEnv = "M7S_RECOVERY_CHILD_CONFIG"

	beforeKillLog   = "child-before-kill.log"
	afterRestartLog = "child-after-restart.log"
)

// ansiEscape 匹配 console-slog 输出的颜色控制序列。
var ansiEscape = regexp.MustCompile("\x1b\\[[0-9;]*m")

func TestRecoveryAfterKill(t *testing.T) {
	// 子进程模式：启动真实 server 并阻塞，直到被父进程 SIGKILL。
	if os.Getenv(recoveryChildEnv) == "1" {
		cfg := os.Getenv(recoveryConfigEnv)
		if cfg == "" {
			t.Fatalf("%s is not set in child mode", recoveryConfigEnv)
		}
		if err := m7s.Run(context.Background(), cfg); err != nil {
			t.Fatalf("child server exited: %v", err)
		}
		return
	}

	if testing.Short() {
		t.Skip("skipping crash/restart integration test in -short mode")
	}
	for _, tool := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found in PATH: %v", tool, err)
		}
	}

	source := repoFile(t, "example/default/test.flv")

	root := t.TempDir()
	storageBase := filepath.Join(root, "storage")
	recordDir := filepath.Join(storageBase, "rec")
	if err := os.MkdirAll(recordDir, 0o755); err != nil {
		t.Fatalf("create record dir: %v", err)
	}

	mediaPath := filepath.Join(recordDir, "crash.mp4")
	sidecar := mediaPath + mp4pkg.IndexExtension

	// 失败时 dump 子进程日志：端口绑定失败、配置错误等只会在子进程日志里留痕，
	// 否则调用方只能看到某个 waitFor 超时。
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, name := range []string{beforeKillLog, afterRestartLog} {
			plain := readChildLog(root, name)
			if plain == "" {
				continue
			}
			t.Logf("=== %s (tail) ===\n%s", name, tail(plain, 8000))
		}
	})

	// ---- 段 1：启动 server，推流并开录 ----
	first := startRecoveryServer(t, root, storageBase, recordDir, beforeKillLog)
	httpPort, rtmpPort := first.httpPort, first.rtmpPort

	ffmpeg := startFFmpeg(t, root, source, rtmpPort)

	waitFor(t, 30*time.Second, 250*time.Millisecond, "record api accepted", func() error {
		status, body, err := postJSON(fmt.Sprintf("http://127.0.0.1:%d/mp4/api/start/live/test", httpPort), map[string]string{
			"filePath": "rec",
			"fileName": "crash.mp4",
			"fragment": "0",
			"duration": "0",
			"type":     "mp4",
		})
		if err != nil {
			return err
		}
		if status == http.StatusOK {
			return nil
		}
		// 重试过程中若第一次已经成功，返回 record exists 同样视为就绪。
		if strings.Contains(body, "record exists") {
			return nil
		}
		return fmt.Errorf("http %d: %s", status, strings.TrimSpace(body))
	})

	// 等索引真正落盘且样本足够（Recovery 至少需要一个关键帧才能重建 stss）。
	waitFor(t, 45*time.Second, 250*time.Millisecond, "index sidecar with samples", func() error {
		idx, err := mp4pkg.ReadIndex(sidecar)
		if err != nil {
			return err
		}
		if len(idx.Records) < 60 {
			return fmt.Errorf("only %d samples", len(idx.Records))
		}
		keyframes := 0
		for _, r := range idx.Records {
			if r.Flags&mp4pkg.IndexFlagKeyFrame != 0 {
				keyframes++
			}
		}
		if keyframes == 0 {
			return fmt.Errorf("%d samples but no keyframe yet", len(idx.Records))
		}
		info, err := os.Stat(mediaPath)
		if err != nil {
			return err
		}
		if info.Size() < 100*1024 {
			return fmt.Errorf("media only %d bytes", info.Size())
		}
		return nil
	})

	// ---- 段 2：SIGKILL 子进程，模拟掉电/崩溃 ----
	killProcess(t, ffmpeg)
	first.stop()
	t.Logf("child killed; media=%d bytes sidecar=%d bytes", statSize(t, mediaPath), statSize(t, sidecar))

	// 恢复任务会跳过 mtime 新于 minage 的文件。生产默认 5m（spec），测试配置
	// 显式调成 6s 以免等待；下限仍是 max(5s, 2×flushinterval)=5s。
	time.Sleep(7 * time.Second)

	// ---- 段 3：重启 server，等待启动恢复自动完成 ----
	// 重启同样走端口重试：kill 后旧端口可能处于 TIME_WAIT，或被其他进程抢占。
	startRecoveryServer(t, root, storageBase, recordDir, afterRestartLog)

	waitFor(t, 90*time.Second, 500*time.Millisecond, "automatic recovery", func() error {
		for _, leftover := range []string{sidecar, sidecar + ".recovering"} {
			if _, err := os.Stat(leftover); err == nil {
				return fmt.Errorf("sidecar still present: %s", filepath.Base(leftover))
			} else if !os.IsNotExist(err) {
				return err
			}
		}
		duration, codecs, err := probeMP4(mediaPath)
		if err != nil {
			return err
		}
		if duration <= 0 {
			return fmt.Errorf("ffprobe duration %v", duration)
		}
		if len(codecs) == 0 {
			return fmt.Errorf("ffprobe found no streams")
		}
		info, err := os.Stat(mediaPath)
		if err != nil {
			return err
		}
		t.Logf("recovered: duration=%v codecs=%v size=%d", duration, codecs, info.Size())
		return nil
	})

	// 日志契约：恢复扫描必须留下逐文件结果。console handler 会输出 ANSI 颜色，
	// 断言前先剥离，避免 outcome=<ESC>recovered 这类假阴性。
	logAfter := ansiEscape.ReplaceAllString(readFileString(t, filepath.Join(root, "child-after-restart.log")), "")
	if !strings.Contains(logAfter, "recovery result") {
		t.Errorf("recovery log missing 'recovery result'\n%s", tail(logAfter, 4000))
	}
	if !strings.Contains(logAfter, "outcome=recovered") {
		t.Errorf("recovery log missing 'outcome=recovered'\n%s", tail(logAfter, 4000))
	}

	// 恢复产物必须是可播放的 progressive MP4，且带顶层 moov。
	if !hasTopLevelBox(mediaPath, "moov") {
		t.Errorf("recovered file has no top-level moov: %s", mediaPath)
	}
}

// hasTopLevelBox 扫描顶层 box，确认 moov 已前移。
func hasTopLevelBox(path, typ string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var header [16]byte
	for i := 0; i < 64; i++ {
		if _, err := io.ReadFull(f, header[:8]); err != nil {
			return false
		}
		size := int64(uint32(header[0])<<24 | uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3]))
		if string(header[4:8]) == typ {
			return true
		}
		extra := int64(0)
		if size == 1 {
			if _, err := io.ReadFull(f, header[8:16]); err != nil {
				return false
			}
			var s uint64
			for _, b := range header[8:16] {
				s = s<<8 | uint64(b)
			}
			size = int64(s)
			extra = 8
		} else if size == 0 {
			return false
		}
		if size < 8+extra {
			return false
		}
		if _, err := f.Seek(size-8-extra, io.SeekCurrent); err != nil {
			return false
		}
	}
	return false
}

// recoveryServer 是一个以 helper 模式运行的 server 子进程。
//
// done 在 cmd.Wait 返回后关闭，exitErr 随即可读（close 建立 happens-before），
// 因此启动期间可以非阻塞地发现「子进程已退出」，而不是干等端口超时。
type recoveryServer struct {
	child    *exec.Cmd
	cfgPath  string
	httpPort int
	grpcPort int
	rtmpPort int
	done     chan struct{}
	exitErr  error
}

// startRecoveryServer 启动子进程并等待 http/rtmp 端口就绪。
//
// freePort 先监听 :0 再立即 Close，与子进程真正 bind 之间存在 TOCTOU 窗口：
// 端口可能被其他进程抢走，或两次 freePort 撞在同一端口。启动失败时换一组端口
// 重试，最终失败时把子进程日志带回错误信息。
func startRecoveryServer(t *testing.T, root, storageBase, recordDir, logName string) *recoveryServer {
	t.Helper()
	const attempts = 3
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		s := &recoveryServer{
			cfgPath:  filepath.Join(root, strings.TrimSuffix(logName, ".log")+".yaml"),
			httpPort: freePort(t),
			grpcPort: freePort(t),
			rtmpPort: freePort(t),
		}
		cfg := recoveryConfigYAML(storageBase, recordDir, s.httpPort, s.grpcPort, s.rtmpPort)
		if err := os.WriteFile(s.cfgPath, []byte(cfg), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		startRecoveryChild(t, root, s, logName)
		if err := waitServerPorts(s, 20*time.Second); err != nil {
			lastErr = err
			s.stop()
			t.Logf("start attempt %d/%d failed (http=%d grpc=%d rtmp=%d): %v\nchild log tail:\n%s",
				attempt, attempts, s.httpPort, s.grpcPort, s.rtmpPort, err, tail(readChildLog(root, logName), 4000))
			continue
		}
		return s
	}
	t.Fatalf("server did not start after %d attempts: %v\nchild log tail:\n%s",
		attempts, lastErr, tail(readChildLog(root, logName), 8000))
	return nil
}

func recoveryConfigYAML(storageBase, recordDir string, httpPort, grpcPort, rtmpPort int) string {
	return fmt.Sprintf(`global:
  http:
    listenaddr: 127.0.0.1:%d
  tcp:
    listenaddr: 127.0.0.1:%d
  loglevel: info
  storage:
    local:
      path: %s
mp4:
  enable: true
  record:
    index:
      enable: true
      flushinterval: 1s
    recovery:
      onstart: true
      minage: 6s
      dirs:
        - %s
rtmp:
  enable: true
  tcp:
    listenaddr: 127.0.0.1:%d
`, httpPort, grpcPort, yamlQuote(storageBase), yamlQuote(recordDir), rtmpPort)
}

// waitServerPorts 轮询 http/rtmp 端口；子进程中途退出时立即返回原因。
func waitServerPorts(s *recoveryServer, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ports := []int{s.httpPort, s.rtmpPort}
	for {
		select {
		case <-s.done:
			return fmt.Errorf("child exited during startup: %v", s.exitErr)
		default:
		}
		ready := 0
		for _, port := range ports {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
			if err == nil {
				_ = conn.Close()
				ready++
			}
		}
		if ready == len(ports) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("ports not ready after %s (http=%d rtmp=%d, ready=%d/%d)",
				timeout, s.httpPort, s.rtmpPort, ready, len(ports))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// startRecoveryChild 以 helper 模式重新执行测试二进制，启动真实 server。
func startRecoveryChild(t *testing.T, dir string, s *recoveryServer, logName string) {
	cfgPath := s.cfgPath
	t.Helper()
	bin, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatalf("resolve test binary: %v", err)
	}
	logFile, err := os.Create(filepath.Join(dir, logName))
	if err != nil {
		t.Fatalf("create child log: %v", err)
	}
	cmd := exec.Command(bin, "-test.run=^TestRecoveryAfterKill$", "-test.v")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), recoveryChildEnv+"=1", recoveryConfigEnv+"="+cfgPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start child: %v", err)
	}
	s.child = cmd
	s.done = make(chan struct{})
	go func() {
		s.exitErr = cmd.Wait()
		logFile.Close()
		close(s.done)
	}()
	t.Cleanup(s.stop)
}

// stop 终止子进程并等待 Wait 完成；可重复调用（done 关闭后立即返回）。
func (s *recoveryServer) stop() {
	if s == nil || s.child == nil || s.child.Process == nil {
		return
	}
	_ = s.child.Process.Kill()
	<-s.done
}

// readChildLog 读取子进程日志并剥离 ANSI 颜色；文件不存在时返回空串。
func readChildLog(dir, name string) string {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return ansiEscape.ReplaceAllString(string(data), "")
}

func startFFmpeg(t *testing.T, dir, source string, rtmpPort int) *exec.Cmd {
	t.Helper()
	logFile, err := os.Create(filepath.Join(dir, "ffmpeg.log"))
	if err != nil {
		t.Fatalf("create ffmpeg log: %v", err)
	}
	url := fmt.Sprintf("rtmp://127.0.0.1:%d/live/test", rtmpPort)
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error",
		"-re", "-stream_loop", "-1", "-i", source,
		"-c", "copy", "-f", "flv", url)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		logFile.Close()
		t.Fatalf("start ffmpeg: %v", err)
	}
	t.Cleanup(func() {
		killProcess(t, cmd)
		logFile.Close()
	})
	return cmd
}

func killProcess(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	if cmd == nil || cmd.Process == nil {
		return
	}
	if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
		return
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
}

func repoFile(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", rel))
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	if _, err := os.Stat(abs); err != nil {
		t.Fatalf("missing repo fixture %s: %v", rel, err)
	}
	return abs
}

func yamlQuote(s string) string { return strconv.Quote(s) }

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func waitFor(t *testing.T, timeout, interval time.Duration, what string, fn func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = fn(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timeout waiting for %s: %v", what, last)
		}
		time.Sleep(interval)
	}
}

func postJSON(url string, body any) (int, string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, "", err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(payload), nil
}

func probeMP4(path string) (time.Duration, []string, error) {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", "format=duration:stream=codec_name",
		"-of", "json", path).Output()
	if err != nil {
		return 0, nil, err
	}
	var probe struct {
		Streams []struct {
			CodecName string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		return 0, nil, fmt.Errorf("parse ffprobe output: %w", err)
	}
	seconds, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil {
		return 0, nil, fmt.Errorf("ffprobe duration %q: %w", probe.Format.Duration, err)
	}
	codecs := make([]string, 0, len(probe.Streams))
	for _, s := range probe.Streams {
		codecs = append(codecs, s.CodecName)
	}
	return time.Duration(seconds * float64(time.Second)), codecs, nil
}

func statSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return info.Size()
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
