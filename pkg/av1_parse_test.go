package pkg

import (
	"testing"

	"github.com/bluenviron/mediacommon/pkg/codecs/av1"
	"github.com/langhuihui/gomem"
	"m7s.live/v5/pkg/codec"
)

// TestParseAV1OBUs tests the ParseAV1OBUs method
func TestParseAV1OBUs(t *testing.T) {
	t.Run("empty reader", func(t *testing.T) {
		sample := &BaseSample{}
		mem := gomem.Memory{}
		reader := mem.NewReader()

		err := sample.ParseAV1OBUs(&reader)
		if err != nil {
			t.Errorf("Expected no error for empty reader, got: %v", err)
		}
	})

	t.Run("single OBU - Sequence Header", func(t *testing.T) {
		sample := &BaseSample{}

		// Create a simple AV1 OBU (Sequence Header)
		// OBU Header: type=1 (SEQUENCE_HEADER), extension_flag=0, has_size_field=1
		obuHeader := byte(0b00001010) // type=1, has_size=1
		obuSize := byte(4)            // Size of OBU payload
		payload := []byte{0x08, 0x0C, 0x00, 0x00}

		mem := gomem.Memory{}
		mem.PushOne([]byte{obuHeader, obuSize})
		mem.PushOne(payload)

		reader := mem.NewReader()
		err := sample.ParseAV1OBUs(&reader)
		if err != nil {
			t.Errorf("ParseAV1OBUs failed: %v", err)
		}

		nalus := sample.Raw.(*Nalus)
		if nalus.Count() != 1 {
			t.Errorf("Expected 1 OBU, got %d", nalus.Count())
		}
	})

	t.Run("multiple OBUs", func(t *testing.T) {
		sample := &BaseSample{}

		mem := gomem.Memory{}

		// First OBU - Temporal Delimiter
		obuHeader1 := byte(0b00010010) // type=2 (TEMPORAL_DELIMITER), has_size=1
		obuSize1 := byte(0)
		mem.PushOne([]byte{obuHeader1, obuSize1})

		// Second OBU - Frame Header with some payload
		obuHeader2 := byte(0b00011010) // type=3 (FRAME_HEADER), has_size=1
		obuSize2 := byte(3)
		payload2 := []byte{0x01, 0x02, 0x03}
		mem.PushOne([]byte{obuHeader2, obuSize2})
		mem.PushOne(payload2)

		reader := mem.NewReader()
		err := sample.ParseAV1OBUs(&reader)
		if err != nil {
			t.Errorf("ParseAV1OBUs failed: %v", err)
		}

		nalus := sample.Raw.(*Nalus)
		if nalus.Count() != 2 {
			t.Errorf("Expected 2 OBUs, got %d", nalus.Count())
		}
	})
}

// TestGetOBUs tests the GetOBUs method
func TestGetOBUs(t *testing.T) {
	t.Run("initialize empty OBUs", func(t *testing.T) {
		sample := &BaseSample{}
		obus := sample.GetOBUs()

		if obus == nil {
			t.Error("GetOBUs should return non-nil OBUs")
		}

		if sample.Raw != obus {
			t.Error("Raw should be set to the returned OBUs")
		}
	})

	t.Run("return existing OBUs", func(t *testing.T) {
		existingOBUs := &OBUs{}
		sample := &BaseSample{
			Raw: existingOBUs,
		}

		obus := sample.GetOBUs()
		if obus != existingOBUs {
			t.Error("GetOBUs should return the existing OBUs")
		}
	})
}

// TestAV1OBUTypes tests all AV1 OBU type constants
func TestAV1OBUTypes(t *testing.T) {
	tests := []struct {
		name     string
		obuType  int
		expected int
	}{
		{"SEQUENCE_HEADER", codec.AV1_OBU_SEQUENCE_HEADER, 1},
		{"TEMPORAL_DELIMITER", codec.AV1_OBU_TEMPORAL_DELIMITER, 2},
		{"FRAME_HEADER", codec.AV1_OBU_FRAME_HEADER, 3},
		{"TILE_GROUP", codec.AV1_OBU_TILE_GROUP, 4},
		{"METADATA", codec.AV1_OBU_METADATA, 5},
		{"FRAME", codec.AV1_OBU_FRAME, 6},
		{"REDUNDANT_FRAME_HEADER", codec.AV1_OBU_REDUNDANT_FRAME_HEADER, 7},
		{"TILE_LIST", codec.AV1_OBU_TILE_LIST, 8},
		{"PADDING", codec.AV1_OBU_PADDING, 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.obuType != tt.expected {
				t.Errorf("OBU type %s: expected %d, got %d", tt.name, tt.expected, tt.obuType)
			}
		})
	}
}

// TestAV1Integration tests the full integration of AV1 codec
func TestAV1Integration(t *testing.T) {
	t.Run("create AV1 context and parse OBUs", func(t *testing.T) {
		// Create AV1 codec context
		ctx := &codec.AV1Ctx{
			ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
		}

		// Verify context properties
		if ctx.GetInfo() != "AV1" {
			t.Errorf("Expected 'AV1', got '%s'", ctx.GetInfo())
		}

		if ctx.FourCC() != codec.FourCC_AV1 {
			t.Error("FourCC should be AV1")
		}

		// Create a sample with OBUs
		sample := &Sample{
			ICodecCtx:  ctx,
			BaseSample: &BaseSample{},
		}

		// Add some OBUs
		obus := sample.GetOBUs()
		obu := obus.GetNextPointer()
		obu.PushOne([]byte{0x0A, 0x01, 0x02, 0x03})

		// Verify OBU count
		if obus.Count() != 1 {
			t.Errorf("Expected 1 OBU, got %d", obus.Count())
		}
	})
}

// TestAV1OBUHeaderParsing tests parsing of actual AV1 OBU headers
func TestAV1OBUHeaderParsing(t *testing.T) {
	tests := []struct {
		name       string
		headerByte byte
		obuType    uint
		hasSize    bool
	}{
		{
			name:       "Sequence Header with size",
			headerByte: 0b00001010, // type=1, has_size=1
			obuType:    1,
			hasSize:    true,
		},
		{
			name:       "Frame with size",
			headerByte: 0b00110010, // type=6, has_size=1
			obuType:    6,
			hasSize:    true,
		},
		{
			name:       "Temporal Delimiter with size",
			headerByte: 0b00010010, // type=2, has_size=1
			obuType:    2,
			hasSize:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var header av1.OBUHeader
			err := header.Unmarshal([]byte{tt.headerByte})
			if err != nil {
				t.Fatalf("Failed to unmarshal OBU header: %v", err)
			}

			if uint(header.Type) != tt.obuType {
				t.Errorf("Expected OBU type %d, got %d", tt.obuType, header.Type)
			}

			if header.HasSize != tt.hasSize {
				t.Errorf("Expected HasSize %v, got %v", tt.hasSize, header.HasSize)
			}
		})
	}
}

// BenchmarkParseAV1OBUs benchmarks the OBU parsing performance
func BenchmarkParseAV1OBUs(b *testing.B) {
	// Prepare test data
	mem := gomem.Memory{}
	for i := 0; i < 10; i++ {
		obuHeader := byte(0b00110010) // Frame OBU
		obuSize := byte(10)
		payload := make([]byte, 10)
		for j := range payload {
			payload[j] = byte(j)
		}
		mem.PushOne([]byte{obuHeader, obuSize})
		mem.PushOne(payload)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sample := &BaseSample{}
		reader := mem.NewReader()
		_ = sample.ParseAV1OBUs(&reader)
	}
}

// TestOBUsReuseArray tests the reuse array functionality with OBUs
func TestOBUsReuseArray(t *testing.T) {
	t.Run("reuse OBU memory", func(t *testing.T) {
		obus := &OBUs{}

		// First allocation
		obu1 := obus.GetNextPointer()
		obu1.PushOne([]byte{1, 2, 3})

		if obus.Count() != 1 {
			t.Errorf("Expected count 1, got %d", obus.Count())
		}

		// Second allocation
		obu2 := obus.GetNextPointer()
		obu2.PushOne([]byte{4, 5, 6})

		if obus.Count() != 2 {
			t.Errorf("Expected count 2, got %d", obus.Count())
		}

		// Reset and reuse
		obus.Reset()
		if obus.Count() != 0 {
			t.Errorf("Expected count 0 after reset, got %d", obus.Count())
		}

		// Reuse memory
		obu3 := obus.GetNextPointer()
		obu3.PushOne([]byte{7, 8, 9})

		if obus.Count() != 1 {
			t.Errorf("Expected count 1 after reuse, got %d", obus.Count())
		}
	})
}
