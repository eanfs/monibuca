// m7s-recover 从没有 moov 的半成品 progressive-MP4 临时文件
// （s3writer_*.tmp）反向重建可播放的 MP4。
//
// 典型用法：
//
//	go run ./example/recover -dir /data/monibuca/tmp -dry-run
//	go run ./example/recover -dir /data/monibuca/tmp -concurrency 4
//
// dry-run 只扫描并输出报告，不写任何文件。退出码只在「所有文件都失败」时为
// 非零；部分失败仍返回 0，便于批量抢救现场录像。
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	plugin_mp4 "m7s.live/v5/plugin/mp4"
)

type fileResult struct {
	path   string
	result plugin_mp4.LegacyResult
	err    error
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(argv []string) int {
	fs := flag.NewFlagSet("m7s-recover", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory containing s3writer_*.tmp files")
	dryRun := fs.Bool("dry-run", false, "scan and report only; write nothing")
	concurrency := fs.Int("concurrency", 2, "number of files processed in parallel")
	fps := fs.Float64("fps", 0, "override frame rate (0 = infer from SPS VUI)")
	videoOnly := fs.Bool("video-only", false, "skip heuristic audio recovery")
	force := fs.Bool("force", false, "overwrite existing <name>.recovered.mp4")
	pattern := fs.String("pattern", "s3writer_*.tmp", "input file glob within -dir")
	if err := fs.Parse(argv); err != nil {
		return 2
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "error: -dir is required")
		fs.Usage()
		return 2
	}
	if *concurrency < 1 {
		*concurrency = 1
	}

	paths, err := filepath.Glob(filepath.Join(*dir, *pattern))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: glob: %v\n", err)
		return 2
	}
	// .m7sidx 永远不是媒体文件；即使自定义 pattern 也不要误扫。
	files := paths[:0]
	for _, p := range paths {
		if strings.HasSuffix(p, ".m7sidx") {
			continue
		}
		files = append(files, p)
	}
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Printf("no files matched %s in %s\n", *pattern, *dir)
		return 0
	}

	started := time.Now()
	results := processFiles(files, *concurrency, *dryRun, *fps, *videoOnly, *force)
	return report(results, *dryRun, started)
}

// processFiles 用固定大小的 worker 池并发处理；输出顺序与输入一致。
func processFiles(files []string, concurrency int, dryRun bool, fps float64, videoOnly, force bool) []fileResult {
	jobs := make(chan int)
	results := make([]fileResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = processOne(files[i], dryRun, fps, videoOnly, force)
			}
		}()
	}
	for i := range files {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func processOne(path string, dryRun bool, fps float64, videoOnly, force bool) fileResult {
	out := outputPath(path)
	res := fileResult{path: path}
	if !dryRun && !force {
		if _, err := os.Stat(out); err == nil {
			res.err = fmt.Errorf("output already exists: %s (use -force to overwrite)", out)
			return res
		}
	}
	opts := plugin_mp4.LegacyOptions{
		FPS:       fps,
		VideoOnly: videoOnly,
		DryRun:    dryRun,
	}
	if !dryRun {
		opts.OutputPath = out
	}
	res.result, res.err = plugin_mp4.RebuildFromMDAT(path, opts)
	return res
}

func outputPath(input string) string {
	// 非 .tmp 输入时 TrimSuffix 原样返回，行为与“去掉 .tmp 再加后缀”一致。
	return strings.TrimSuffix(input, ".tmp") + ".recovered.mp4"
}

func report(results []fileResult, dryRun bool, started time.Time) int {
	var ok, failed int
	var frames, audioFrames int
	var durationMs int64

	for _, r := range results {
		name := filepath.Base(r.path)
		if r.err != nil {
			failed++
			fmt.Printf("[fail] %s: %v\n", name, r.err)
			continue
		}
		ok++
		frames += r.result.VideoFrames
		audioFrames += r.result.AudioFrames
		durationMs += r.result.DurationMs

		audio := "audio=none"
		if r.result.AudioRecovered {
			audio = fmt.Sprintf("audio=%d (%s)", r.result.AudioFrames, r.result.AudioCodec)
		}
		target := r.result.OutputPath
		if dryRun {
			target = "(dry-run, not written)"
		}
		fmt.Printf("[ok]   %s -> %s frames=%d %s duration=%.1fs\n",
			name, target, r.result.VideoFrames, audio, float64(r.result.DurationMs)/1000.0)
		for _, w := range r.result.Warnings {
			fmt.Printf("       warn: %s\n", w)
		}
	}

	fmt.Printf("\nsummary: files=%d ok=%d failed=%d video_frames=%d audio_frames=%d duration=%.1fs elapsed=%s\n",
		len(results), ok, failed, frames, audioFrames, float64(durationMs)/1000.0,
		time.Since(started).Round(time.Millisecond))
	if dryRun {
		fmt.Println("dry-run: no files were written")
	}
	if len(results) > 0 && failed == len(results) {
		return 1
	}
	return 0
}
