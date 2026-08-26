package store

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// CompressionThreshold is the size above which a payload is zstd-compressed
// (spec 002 #8). Below it the frame header costs more than the compression
// saves, so tiny values are stored verbatim and the `compression` column says
// so.
const CompressionThreshold = 128

// Compression values stored in payloads.compression and raw_batches.
const (
	CompressionNone = "none"
	CompressionZstd = "zstd"
)

// One encoder and one decoder for the process: both are safe for concurrent
// use through EncodeAll/DecodeAll and keep their buffers pooled, which is the
// point — ingest compresses on every request path.
var (
	zstdEncoder *zstd.Encoder
	zstdDecoder *zstd.Decoder
)

func init() {
	var err error
	// SpeedDefault, not SpeedBestCompression: ingest latency is the
	// budget, and OTLP protobuf and JSON payloads already compress an
	// order of magnitude at this level.
	zstdEncoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		panic("store: zstd encoder: " + err.Error())
	}
	zstdDecoder, err = zstd.NewReader(nil)
	if err != nil {
		panic("store: zstd decoder: " + err.Error())
	}
}

// compress returns the stored form of a value together with its compression
// label and original size.
func compress(raw []byte) (compression string, body []byte, sizeRaw int) {
	if len(raw) <= CompressionThreshold {
		return CompressionNone, raw, len(raw)
	}
	return CompressionZstd, zstdEncoder.EncodeAll(raw, nil), len(raw)
}

// Decompress restores a stored payload or raw batch body.
func Decompress(compression string, body []byte) ([]byte, error) {
	switch compression {
	case CompressionNone:
		return body, nil
	case CompressionZstd:
		out, err := zstdDecoder.DecodeAll(body, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress payload: %w", err)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unknown compression %q", compression)
}
