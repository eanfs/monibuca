//go:build aac_opus_transcode

package aacopus

/*
#cgo linux CFLAGS: -I${SRCDIR}/../../3rd/ffmpeg6/linux/include -I${SRCDIR}/../../3rd
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/../../3rd/ffmpeg6/linux/lib/arm64 -lavcodec -lavutil -lswresample
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../3rd/ffmpeg6/linux/lib/amd64 -lavcodec -lavutil -lswresample

#cgo darwin CFLAGS: -I${SRCDIR}/../../3rd/ffmpeg6/mac/include -I${SRCDIR}/../../3rd
#cgo darwin LDFLAGS: -L${SRCDIR}/../../3rd/ffmpeg6/mac/lib -lavcodec -lavutil -lswresample

#cgo windows CFLAGS: -I${SRCDIR}/../../3rd/ffmpeg6/windows/include -I${SRCDIR}/../../3rd
#cgo windows LDFLAGS: -L${SRCDIR}/../../3rd/ffmpeg6/windows/lib -lavcodec -lavutil -lswresample

#include "aacopus/aacopus.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"

	"m7s.live/v5/pkg/codec"
)

type AACToOpus struct {
	ctx *C.AACOpusTranscoder
}

func NewAACToOpus(aac *codec.AACCtx, bitrate int) (*AACToOpus, error) {
	if aac == nil {
		return nil, errors.New("nil AACCtx")
	}
	asc := aac.GetRecord()
	if len(asc) == 0 {
		return nil, errors.New("empty AAC config")
	}
	sr := 48000
	ch := aac.GetChannels()
	if ch <= 0 {
		ch = 2
	}
	if bitrate <= 0 {
		bitrate = 64000
	}
	ctx := C.aacopus_new(
		C.int(sr),
		C.int(ch),
		C.int(bitrate),
		(*C.uint8_t)(unsafe.Pointer(&asc[0])),
		C.int(len(asc)),
	)
	if ctx == nil {
		return nil, errors.New("aacopus_new failed")
	}
	return &AACToOpus{ctx: ctx}, nil
}

func (t *AACToOpus) Close() {
	if t == nil || t.ctx == nil {
		return
	}
	C.aacopus_free(t.ctx)
	t.ctx = nil
}

func (t *AACToOpus) Transcode(aacAU []byte) ([][]byte, error) {
	if t == nil || t.ctx == nil {
		return nil, errors.New("transcoder not initialized")
	}
	if len(aacAU) == 0 {
		return nil, nil
	}
	if ret := C.aacopus_feed(t.ctx, (*C.uint8_t)(unsafe.Pointer(&aacAU[0])), C.int(len(aacAU))); ret < 0 {
		return nil, fmt.Errorf("aacopus_feed failed: %d", int(ret))
	}
	var out [][]byte
	buf := make([]byte, 1500)
	for {
		n := C.aacopus_get(t.ctx, (*C.uint8_t)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
		if n <= 0 {
			break
		}
		out = append(out, append([]byte(nil), buf[:int(n)]...))
	}
	return out, nil
}
