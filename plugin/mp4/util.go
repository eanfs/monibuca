package plugin_mp4

import (
	"fmt"
	"io"
	"log"
	"os/exec"

	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
	mp4 "m7s.live/v5/plugin/mp4/pkg"
	"m7s.live/v5/plugin/mp4/pkg/box"
)

// ProcessWithFFmpeg 使用 FFmpeg 处理视频帧并生成截图
func ProcessWithFFmpeg(samples []box.Sample, index int, videoTrack *mp4.Track, output io.Writer) error {
	// 创建ffmpeg命令，直接输出JPEG格式
	cmd := exec.Command("ffmpeg",
		"-hide_banner",
		"-i", "pipe:0",
		"-vf", fmt.Sprintf("select=eq(n\\,%d)", index),
		"-vframes", "1",
		"-f", "mjpeg",
		"pipe:1")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	go func() {
		errOutput, _ := io.ReadAll(stderr)
		log.Printf("FFmpeg stderr: %s", errOutput)
	}()

	if err = cmd.Start(); err != nil {
		log.Printf("cmd.Start失败: %v", err)
		return err
	}

	go func() {
		defer stdin.Close()
		var convert *pkg.AVFrameConvert[*pkg.AnnexB]
		switch videoTrack.Cid {
		case box.MP4_CODEC_H264:
			var h264Ctx *codec.H264Ctx
			h264Ctx, err = codec.NewH264CtxFromRecord(videoTrack.ExtraData)
			if err != nil {
				log.Printf("解析H264失败: %v", err)
				return
			}
			convert = pkg.NewAVFrameConvert[*pkg.AnnexB](h264Ctx)
		case box.MP4_CODEC_H265:
			var h265Ctx *codec.H265Ctx
			h265Ctx, err = codec.NewH265CtxFromRecord(videoTrack.ExtraData)
			if err != nil {
				log.Printf("解析H265失败: %v", err)
				return
			}
			convert = pkg.NewAVFrameConvert[*pkg.AnnexB](h265Ctx)
		default:
			log.Printf("不支持的编解码器: %v", videoTrack.Cid)
			return
		}
		for _, sample := range samples {
			annexb, err := convert.Convert(&mp4.Video{
				Sample: sample,
			})
			if err != nil {
				log.Printf("转换失败: %v", err)
				continue
			}
			annexb.WriteTo(stdin)
		}
	}()

	// 从ffmpeg的stdout读取JPEG数据并写入到输出
	if _, err = io.Copy(output, stdout); err != nil {
		log.Printf("读取失败: %v", err)
		return err
	}
	if err = cmd.Wait(); err != nil {
		log.Printf("cmd.Wait失败: %v", err)
		return err
	}

	log.Printf("ffmpeg JPEG输出成功")
	return nil
}
