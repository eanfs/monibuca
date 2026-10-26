package mp4

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"m7s.live/v5/plugin/mp4/pkg/box"
)

// TestDemuxerBoxTree tests the Demuxer by reading a test MP4 file and printing the box tree structure.
func TestDemuxerBoxTree(t *testing.T) {
	// Open the test mp4 file. It is assumed to be located in 'testdata/test_regular.mp4'.
	const testFile = "/Users/dexter/project/v5/monibuca/example/default/dump/test_regular.mp4"
	if _, statErr := os.Stat(testFile); statErr != nil {
		t.Skipf("test media file not available: %v", statErr)
	}
	f, err := os.Open(testFile)
	if err != nil {
		t.Fatalf("failed to open test mp4 file: %v", err)
	}
	defer f.Close()

	// Create a new Demuxer with the file reader
	d := NewDemuxer(f)

	// Call Demux to process the file; we don't use the result directly here.
	err = d.Demux()
	if err != nil {
		t.Fatalf("demuxing failed: %v", err)
	}

	// Reset the file pointer to the beginning to re-read boxes for tree display
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("failed to seek to beginning: %v", err)
	}

	fmt.Println("MP4 Box Tree:")
	// Read and print each top-level box
	for {
		b, err := box.ReadFrom(f)
		if err != nil {
			if err == io.EOF {
				break
			}
			t.Fatalf("failed to read box: %v", err)
		}
		printBox(b, 0)
	}
}

// printBox prints a box's type and size, and recursively prints its children if available.
func printBox(b interface{}, indent int) {
	ind := strings.Repeat("  ", indent)
	// Determine the box type name using reflection
	typeOfBox := reflect.TypeOf(b)
	if typeOfBox.Kind() == reflect.Ptr {
		typeOfBox = typeOfBox.Elem()
	}
	// Try to get the size from a method Size() int
	var size int
	if s, ok := b.(interface{ Size() int }); ok {
		size = s.Size()
	}
	fmt.Printf("%s%s (size: %d)\n", ind, typeOfBox.Name(), size)

	// If the box is a container and has child boxes, print them recursively.
	if container, ok := b.(interface{ ChildrenBoxes() []interface{} }); ok {
		for _, child := range container.ChildrenBoxes() {
			printBox(child, indent+1)
		}
	} else if container, ok := b.(interface{ ChildrenBoxes() []any }); ok {
		for _, child := range container.ChildrenBoxes() {
			printBox(child, indent+1)
		}
	}
}

// inProgressTruncatedMDAT 模拟"只写了 box 头、载荷还没落盘"的尾部：
// 声明 4096 字节，实际剩余 0。进行中录制被 demux 时必然读到这种尾巴。
var inProgressTruncatedMDAT = []byte{0, 0, 0x10, 0, 'm', 'd', 'a', 't'}

// buildInProgressStream 用真实 muxer 造一段内存 mp4 并追加截断尾部。
// fragment=true 走分片模式；moof=false 时只落 init(moov)，没有 moof。
func buildInProgressStream(t *testing.T, fragment, moof bool) []byte {
	t.Helper()
	flag := Flag(0)
	if fragment {
		flag = FLAG_FRAGMENT
	}
	m := NewMuxer(flag)
	var buf bytes.Buffer
	if err := m.WriteInitSegment(&buf); err != nil {
		t.Fatalf("write init segment: %v", err)
	}
	if !fragment {
		return append(buf.Bytes(), inProgressTruncatedMDAT...)
	}
	track := m.AddTrack(box.MP4_CODEC_H264)
	track.Timescale = 1000
	parsed, err := box.ReadFrom(bytes.NewReader(restoreTestSTSD(t, true)))
	if err != nil {
		t.Fatalf("parse stsd: %v", err)
	}
	track.STSD = parsed.(*box.STSDBox)
	if err = m.WriteSample(&buf, track, box.Sample{KeyFrame: true, Timestamp: 0, Duration: 40}); err != nil {
		t.Fatalf("write sample 0: %v", err)
	}
	if moof {
		// 第二个 sample 触发首个 moof/mdat 落盘（首个 sample 只缓存）。
		if err = m.WriteSample(&buf, track, box.Sample{Timestamp: 40, Duration: 40}); err != nil {
			t.Fatalf("write sample 1: %v", err)
		}
	} else if err = m.WriteMoov(&buf); err != nil {
		t.Fatalf("write moov: %v", err)
	}
	return append(buf.Bytes(), inProgressTruncatedMDAT...)
}

// 进行中文件读到"声明长度超出剩余字节"的 box 必须优雅收尾：Demux 返回 nil，
// 已解析的样本保留。这是 plugin/mp4/recovery.go 恢复进行中录像的前提；
// 也钉住 demuxer 用 errors.Is 判断 io.ErrUnexpectedEOF（box 层的拒绝包装了它）。
func TestDemuxTruncatedTailIsGraceful(t *testing.T) {
	cases := []struct {
		name        string
		fragment    bool
		moof        bool
		wantSamples int
	}{
		// moof 完整 + mdat 声明超出剩余：样本表已建立，必须保留。
		{"fragment_with_moof", true, true, 1},
		// 只有 init(moov)：没有样本，但同样不能返回错误。
		{"fragment_init_only", true, false, 0},
		// 非分片进行中文件（IsFragment=false），同样不能返回错误。
		{"progressive", false, false, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := NewDemuxer(bytes.NewReader(buildInProgressStream(t, c.fragment, c.moof)))
			if err := d.Demux(); err != nil {
				t.Fatalf("truncated tail must be graceful, got %v", err)
			}
			samples := 0
			for _, track := range d.Tracks {
				samples += len(track.Samplelist)
			}
			if samples != c.wantSamples {
				t.Fatalf("samples = %d, want %d", samples, c.wantSamples)
			}
		})
	}
}
