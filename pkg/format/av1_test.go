package format

import (
	"testing"
	"time"

	"github.com/langhuihui/gomem"
	"m7s.live/v5/pkg"
	"m7s.live/v5/pkg/codec"
)

func TestAV1Frame_CheckCodecChange(t *testing.T) {
	// Test with nil codec context - should return error
	t.Run("nil codec context", func(t *testing.T) {
		frame := &AV1Frame{}
		err := frame.CheckCodecChange()
		if err != pkg.ErrUnsupportCodec {
			t.Errorf("Expected ErrUnsupportCodec, got %v", err)
		}
	})

	// Test with valid AV1 codec context
	t.Run("valid codec context", func(t *testing.T) {
		frame := &AV1Frame{
			Sample: pkg.Sample{
				ICodecCtx: &codec.AV1Ctx{
					ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
				},
			},
		}
		err := frame.CheckCodecChange()
		if err != nil {
			t.Errorf("Unexpected error: %v", err)
		}
	})
}

func TestAV1Frame_GetSize(t *testing.T) {
	t.Run("empty OBUs", func(t *testing.T) {
		frame := &AV1Frame{
			Sample: pkg.Sample{
				BaseSample: &pkg.BaseSample{
					Raw: &pkg.OBUs{},
				},
			},
		}
		size := frame.GetSize()
		if size != 0 {
			t.Errorf("Expected size 0, got %d", size)
		}
	})

	t.Run("with OBUs", func(t *testing.T) {
		obus := &pkg.OBUs{}

		// Add first OBU
		obu1 := obus.GetNextPointer()
		obu1.PushOne([]byte{1, 2, 3, 4})

		// Add second OBU
		obu2 := obus.GetNextPointer()
		obu2.PushOne([]byte{5, 6, 7, 8, 9})

		frame := &AV1Frame{
			Sample: pkg.Sample{
				BaseSample: &pkg.BaseSample{
					Raw: obus,
				},
			},
		}

		size := frame.GetSize()
		expectedSize := 4 + 5 // Total bytes in both OBUs
		if size != expectedSize {
			t.Errorf("Expected size %d, got %d", expectedSize, size)
		}
	})

	t.Run("non-OBUs raw data", func(t *testing.T) {
		frame := &AV1Frame{
			Sample: pkg.Sample{
				BaseSample: &pkg.BaseSample{
					Raw: &gomem.Memory{},
				},
			},
		}
		size := frame.GetSize()
		if size != 0 {
			t.Errorf("Expected size 0 for non-OBUs raw data, got %d", size)
		}
	})
}

func TestAV1Frame_Demux(t *testing.T) {
	mem := gomem.Memory{}
	mem.PushOne([]byte{1, 2, 3, 4, 5})

	frame := &AV1Frame{
		Sample: pkg.Sample{
			RecyclableMemory: gomem.RecyclableMemory{
				Memory: mem,
			},
			BaseSample: &pkg.BaseSample{},
		},
	}

	err := frame.Demux()
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// After demux, Raw should point to the Memory
	if frame.Sample.BaseSample.Raw != &frame.Sample.RecyclableMemory.Memory {
		t.Error("Raw should point to Memory after Demux")
	}
}

func TestAV1Frame_Mux(t *testing.T) {
	// Create source sample with OBUs
	obus := &pkg.OBUs{}

	obu1 := obus.GetNextPointer()
	obu1.PushOne([]byte{1, 2, 3})

	obu2 := obus.GetNextPointer()
	obu2.PushOne([]byte{4, 5, 6, 7})

	ctx := &codec.AV1Ctx{
		ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
	}

	sourceSample := &pkg.Sample{
		ICodecCtx: ctx,
		BaseSample: &pkg.BaseSample{
			Raw:       obus,
			Timestamp: time.Second,
			CTS:       100 * time.Millisecond,
		},
	}

	// Create destination frame
	destFrame := &AV1Frame{
		Sample: pkg.Sample{
			BaseSample: &pkg.BaseSample{},
		},
	}

	// Perform mux
	err := destFrame.Mux(sourceSample)
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// Verify codec context is set
	if destFrame.ICodecCtx != ctx {
		t.Error("Codec context not set correctly")
	}

	// Verify data was copied
	if destFrame.Memory.Size != 7 { // 3 + 4 bytes
		t.Errorf("Expected memory size 7, got %d", destFrame.Memory.Size)
	}
}

func TestAV1Frame_String(t *testing.T) {
	frame := &AV1Frame{
		Sample: pkg.Sample{
			ICodecCtx: &codec.AV1Ctx{
				ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
			},
			BaseSample: &pkg.BaseSample{
				Timestamp: time.Second,
				CTS:       100 * time.Millisecond,
			},
		},
	}

	str := frame.String()
	// Should contain AV1Frame, FourCC, Timestamp, and CTS
	if len(str) == 0 {
		t.Error("String() should not return empty string")
	}

	// The string should contain key information
	t.Logf("AV1Frame.String() output: %s", str)
}

func TestAV1Frame_Workflow(t *testing.T) {
	// Test the complete workflow: create -> demux -> mux
	t.Run("complete workflow", func(t *testing.T) {
		// Step 1: Create a frame with sample data
		mem := gomem.Memory{}
		mem.PushOne([]byte{1, 2, 3, 4, 5})

		ctx := &codec.AV1Ctx{
			ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
		}

		originalFrame := &AV1Frame{
			Sample: pkg.Sample{
				ICodecCtx: ctx,
				RecyclableMemory: gomem.RecyclableMemory{
					Memory: mem,
				},
				BaseSample: &pkg.BaseSample{
					Timestamp: time.Second,
					CTS:       100 * time.Millisecond,
					IDR:       true,
				},
			},
		}

		// Step 2: Demux
		err := originalFrame.Demux()
		if err != nil {
			t.Fatalf("Demux failed: %v", err)
		}

		// Step 3: Create OBUs for muxing
		obus := &pkg.OBUs{}
		obu := obus.GetNextPointer()
		obu.PushOne([]byte{10, 20, 30})

		sourceSample := &pkg.Sample{
			ICodecCtx: ctx,
			BaseSample: &pkg.BaseSample{
				Raw: obus,
			},
		}

		// Step 4: Mux into new frame
		newFrame := &AV1Frame{
			Sample: pkg.Sample{
				BaseSample: &pkg.BaseSample{},
			},
		}

		err = newFrame.Mux(sourceSample)
		if err != nil {
			t.Fatalf("Mux failed: %v", err)
		}

		// Step 5: Verify codec context
		if newFrame.ICodecCtx != ctx {
			t.Error("Codec context not preserved")
		}

		// Step 6: Check codec change should not return error
		err = newFrame.CheckCodecChange()
		if err != nil {
			t.Errorf("CheckCodecChange failed: %v", err)
		}
	})
}
