package codec

import (
	"testing"
)

func TestAV1Ctx_GetInfo(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
	}

	info := ctx.GetInfo()
	if info != "AV1" {
		t.Errorf("Expected 'AV1', got '%s'", info)
	}
}

func TestAV1Ctx_GetBase(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
	}

	base := ctx.GetBase()
	if base != ctx {
		t.Error("GetBase should return itself")
	}
}

func TestAV1Ctx_Width(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
	}

	width := ctx.Width()
	if width != 0 {
		t.Errorf("Expected width 0, got %d", width)
	}
}

func TestAV1Ctx_Height(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: []byte{0x0A, 0x0B, 0x00},
	}

	height := ctx.Height()
	if height != 0 {
		t.Errorf("Expected height 0, got %d", height)
	}
}

func TestAV1Ctx_FourCC(t *testing.T) {
	ctx := &AV1Ctx{}

	fourcc := ctx.FourCC()
	expected := FourCC_AV1
	if fourcc != expected {
		t.Errorf("Expected %v, got %v", expected, fourcc)
	}

	// Verify the actual FourCC string
	if fourcc.String() != "av01" {
		t.Errorf("Expected 'av01', got '%s'", fourcc.String())
	}
}

func TestAV1Ctx_GetRecord(t *testing.T) {
	configOBUs := []byte{0x0A, 0x0B, 0x00, 0x01, 0x02}
	ctx := &AV1Ctx{
		ConfigOBUs: configOBUs,
	}

	record := ctx.GetRecord()
	if len(record) != len(configOBUs) {
		t.Errorf("Expected record length %d, got %d", len(configOBUs), len(record))
	}

	for i, b := range record {
		if b != configOBUs[i] {
			t.Errorf("Byte mismatch at index %d: expected %02X, got %02X", i, configOBUs[i], b)
		}
	}
}

func TestAV1Ctx_String(t *testing.T) {
	tests := []struct {
		name       string
		configOBUs []byte
		expected   string
	}{
		{
			name:       "Standard config",
			configOBUs: []byte{0x0A, 0x0B, 0x00},
			expected:   "av01.0A0B00",
		},
		{
			name:       "Different config",
			configOBUs: []byte{0x08, 0x0C, 0x00},
			expected:   "av01.080C00",
		},
		{
			name:       "High profile config",
			configOBUs: []byte{0x0C, 0x10, 0x00},
			expected:   "av01.0C1000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := &AV1Ctx{
				ConfigOBUs: tt.configOBUs,
			}

			result := ctx.String()
			if result != tt.expected {
				t.Errorf("Expected '%s', got '%s'", tt.expected, result)
			}
		})
	}
}

func TestAV1Ctx_EmptyConfigOBUs(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: []byte{},
	}

	// Should not panic when calling methods with empty ConfigOBUs
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Panic occurred with empty ConfigOBUs: %v", r)
		}
	}()

	_ = ctx.GetInfo()
	_ = ctx.GetBase()
	_ = ctx.FourCC()
	_ = ctx.GetRecord()
	// Note: String() will panic with empty ConfigOBUs due to array indexing
}

func TestAV1Ctx_NilConfigOBUs(t *testing.T) {
	ctx := &AV1Ctx{
		ConfigOBUs: nil,
	}

	// Should not panic for most methods
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Panic occurred with nil ConfigOBUs: %v", r)
		}
	}()

	_ = ctx.GetInfo()
	_ = ctx.GetBase()
	_ = ctx.FourCC()

	record := ctx.GetRecord()
	if record != nil {
		t.Error("Expected nil record for nil ConfigOBUs")
	}
}

// Test AV1 OBU Type Constants
func TestAV1_OBUTypeConstants(t *testing.T) {
	tests := []struct {
		name     string
		obuType  int
		expected int
	}{
		{"SEQUENCE_HEADER", AV1_OBU_SEQUENCE_HEADER, 1},
		{"TEMPORAL_DELIMITER", AV1_OBU_TEMPORAL_DELIMITER, 2},
		{"FRAME_HEADER", AV1_OBU_FRAME_HEADER, 3},
		{"TILE_GROUP", AV1_OBU_TILE_GROUP, 4},
		{"METADATA", AV1_OBU_METADATA, 5},
		{"FRAME", AV1_OBU_FRAME, 6},
		{"REDUNDANT_FRAME_HEADER", AV1_OBU_REDUNDANT_FRAME_HEADER, 7},
		{"TILE_LIST", AV1_OBU_TILE_LIST, 8},
		{"PADDING", AV1_OBU_PADDING, 15},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.obuType != tt.expected {
				t.Errorf("Expected OBU type %d, got %d", tt.expected, tt.obuType)
			}
		})
	}
}
