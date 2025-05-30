package pkg

import (
	"testing"

	"m7s.live/v5/pkg/codec"
	"m7s.live/v5/pkg/util"
)

func TestH26xFrame_Parse_VideoFrameDetection(t *testing.T) {
	// Test H264 IDR Picture (should not skip)
	t.Run("H264_IDR_Picture", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H264,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x65}), // IDR Picture NALU type
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err == ErrSkip {
			t.Error("Expected H264 IDR frame to not be skipped, but got ErrSkip")
		}
		if !track.Value.IDR {
			t.Error("Expected IDR flag to be set for H264 IDR frame")
		}
	})

	// Test H264 Non-IDR Picture (should not skip)
	t.Run("H264_Non_IDR_Picture", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H264,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x21}), // Non-IDR Picture NALU type
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err == ErrSkip {
			t.Error("Expected H264 Non-IDR frame to not be skipped, but got ErrSkip")
		}
	})

	// Test H264 metadata only (should skip)
	t.Run("H264_SPS_Only", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H264,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x67}), // SPS NALU type
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err != ErrSkip {
			t.Errorf("Expected H264 SPS-only frame to be skipped, but got: %v", err)
		}
	})

	// Test H264 PPS only (should skip)
	t.Run("H264_PPS_Only", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H264,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x68}), // PPS NALU type
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err != ErrSkip {
			t.Errorf("Expected H264 PPS-only frame to be skipped, but got: %v", err)
		}
	})

	// Test H265 IDR slice (should not skip)
	t.Run("H265_IDR_Slice", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H265,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x4E, 0x01}), // IDR_W_RADL slice type (19 << 1 = 38 = 0x26, so first byte should be 0x4C, but let's use a simpler approach)
				// Using NAL_UNIT_CODED_SLICE_IDR_W_RADL which should be type 19
			},
		}
		track := &AVTrack{}

		// Let's use the correct byte pattern for H265 IDR slice
		// NAL_UNIT_CODED_SLICE_IDR_W_RADL = 19
		// H265 header: (type << 1) | layer_id_bit
		idrSliceByte := byte(19 << 1) // 19 * 2 = 38 = 0x26
		frame.Nalus[0] = util.NewMemory([]byte{idrSliceByte})

		err := frame.Parse(track)
		if err == ErrSkip {
			t.Error("Expected H265 IDR slice to not be skipped, but got ErrSkip")
		}
		if !track.Value.IDR {
			t.Error("Expected IDR flag to be set for H265 IDR slice")
		}
	})

	// Test H265 metadata only (should skip)
	t.Run("H265_VPS_Only", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H265,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x40, 0x01}), // VPS NALU type (32 << 1 = 64 = 0x40)
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err != ErrSkip {
			t.Errorf("Expected H265 VPS-only frame to be skipped, but got: %v", err)
		}
	})

	// Test mixed H264 frame with SPS and IDR (should not skip)
	t.Run("H264_Mixed_SPS_And_IDR", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H264,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x67}), // SPS NALU type
				util.NewMemory([]byte{0x65}), // IDR Picture NALU type
			},
		}
		track := &AVTrack{}
		err := frame.Parse(track)
		if err == ErrSkip {
			t.Error("Expected H264 mixed SPS+IDR frame to not be skipped, but got ErrSkip")
		}
		if !track.Value.IDR {
			t.Error("Expected IDR flag to be set for H264 mixed frame with IDR")
		}
	})

	// Test mixed H265 frame with VPS and IDR (should not skip)
	t.Run("H265_Mixed_VPS_And_IDR", func(t *testing.T) {
		frame := &H26xFrame{
			FourCC: codec.FourCC_H265,
			Nalus: []util.Memory{
				util.NewMemory([]byte{0x40, 0x01}), // VPS NALU type (32 << 1)
				util.NewMemory([]byte{0x4C, 0x01}), // IDR_W_RADL slice type (19 << 1)
			},
		}
		track := &AVTrack{}

		// Fix the IDR slice byte for H265
		idrSliceByte := byte(19 << 1) // NAL_UNIT_CODED_SLICE_IDR_W_RADL = 19
		frame.Nalus[1] = util.NewMemory([]byte{idrSliceByte, 0x01})

		err := frame.Parse(track)
		if err == ErrSkip {
			t.Error("Expected H265 mixed VPS+IDR frame to not be skipped, but got ErrSkip")
		}
		if !track.Value.IDR {
			t.Error("Expected IDR flag to be set for H265 mixed frame with IDR")
		}
	})
}
