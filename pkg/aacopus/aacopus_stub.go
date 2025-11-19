//go:build !aac_opus_transcode

package aacopus

import (
    "errors"

    "m7s.live/v5/pkg/codec"
)

type AACToOpus struct{}

func NewAACToOpus(_ *codec.AACCtx, _ int) (*AACToOpus, error) {
    return nil, errors.New("aac_opus_transcode build tag not enabled")
}

func (t *AACToOpus) Transcode(_ []byte) ([][]byte, error) {
    return nil, errors.New("aac_opus_transcode build tag not enabled")
}

func (t *AACToOpus) Close() {}

