package snap

import (
	"fmt"
	"io"
	"os/exec"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/filerotate"

	m7s "m7s.live/v5"
	"m7s.live/v5/pkg/task"
)

// 定义传输模式的常量
const (
	SNAP_MODE_PIPE   SnapMode = "pipe"
	SNAP_MODE_REMOTE SnapMode = "remote"
)

type (
	SnapMode   string
	SnapConfig struct {
		Mode   SnapMode `default:"pipe" json:"mode" desc:"截图模式"` //截图模式
		Remote string   `json:"remote" desc:"远程地址"`
	}
	SnapRule struct {
		From      SnapConfig `json:"from"`
		LogToFile string     `json:"logtofile" desc:"截图是否写入日志"` //截图日志写入文件
	}

	Config struct {
		Input  interface{} `json:"input"`
		Output []Output    `json:"output"`
	}

	Output struct {
		Target string      `json:"target"`
		Conf   interface{} `json:"conf"`
	}
)

func NewTransform() m7s.ITransformer {
	ret := &Transformer{
		snapChan: make(chan struct{}, 1),
	}
	ret.SetDescription(task.OwnerTypeKey, "Snap")
	return ret
}

type Transformer struct {
	m7s.DefaultTransformer
	SnapRule
	logFile  *filerotate.File
	ffmpeg   *exec.Cmd
	snapChan chan struct{}
}

func (t *Transformer) TriggerSnap() {
	select {
	case t.snapChan <- struct{}{}:
	default:
		// 如果通道已满，移除旧的请求
		<-t.snapChan
		t.snapChan <- struct{}{}
	}
}

func (t *Transformer) Run() (err error) {

	// 等待截图触发信号
	<-t.snapChan
	s := t.GetTransformJob().Plugin.Server
	publisher, ok := s.Streams.Get(t.TransformJob.StreamPath)
	if !ok || publisher.VideoTrack.AVTrack == nil {
		return pkg.ErrNotFound
	}

	err = publisher.VideoTrack.WaitReady()
	if err != nil {
		return err
	}

	reader := pkg.NewAVRingReader(publisher.VideoTrack.AVTrack, "Origin")
	err = reader.StartRead(publisher.VideoTrack.GetIDR())
	if err != nil {
		return err
	}
	defer reader.StopRead()

	if reader.Value.Raw == nil {
		if err = reader.Value.Demux(publisher.VideoTrack.ICodecCtx); err != nil {
			return err
		}
	}
	var annexb pkg.AnnexB
	var track pkg.AVTrack

	track.ICodecCtx, track.SequenceFrame, err = annexb.ConvertCtx(publisher.VideoTrack.ICodecCtx)
	if track.ICodecCtx == nil {
		return fmt.Errorf("unsupported codec")
	}
	annexb.Mux(track.ICodecCtx, &reader.Value)

	// 创建ffmpeg命令
	cmd := exec.Command("ffmpeg", "-hide_banner", "-i", "pipe:0", "-vframes", "1", "-f", "mjpeg", "pipe:1")

	// 获取输入和输出pipe
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	// 启动ffmpeg进程
	if err = cmd.Start(); err != nil {
		return err
	}

	// 将annexb数据写入到ffmpeg的stdin
	_, err = annexb.WriteTo(stdin)
	stdin.Close()
	if err != nil {
		return err
	}

	// 从ffmpeg的stdout读取图片数据并写入到输出配置中
	_, err = io.Copy(t.TransformJob.Config.Output[0].Conf.(io.Writer), stdout)
	if err != nil {
		return err
	}

	// 等待ffmpeg进程结束
	if err = cmd.Wait(); err != nil {
		return err
	}

	return nil
}

func (t *Transformer) Dispose() {
	close(t.snapChan)
	if t.ffmpeg != nil {
		err := t.ffmpeg.Process.Kill()
		t.Error("kill ffmpeg", "err", err)
	}
	if t.logFile != nil {
		_ = t.logFile.Close()
	}
}
