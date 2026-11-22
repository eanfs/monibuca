package storage

import (
	"context"
	"errors"
	"io"

	"golang.org/x/time/rate"
)

// maxThrottleChunk is the largest piece a single limiter reservation covers.
// The configured burst is clamped to at least this value so a chunk-sized
// reservation always fits in the bucket.
const maxThrottleChunk = 1 << 20

// uploadReadLimiter is the process-wide upload read token bucket; nil means
// unconfigured (passthrough).
var uploadReadLimiter *rate.Limiter

// NewUploadThrottledReadSeeker wraps rs with the global upload read limiter.
// With no limiter configured it returns rs unchanged (zero overhead).
//
// The static return type is io.ReadSeeker, but the dynamic type keeps ReadAt
// when rs supports it: the AWS SDK only uses concurrent io.NewSectionReader
// range reads for a Body implementing both io.ReaderAt and io.ReadSeeker.
// Returning a plain ReadSeeker would silently serialize multipart reads.
func NewUploadThrottledReadSeeker(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	if uploadReadLimiter == nil {
		return rs
	}
	w := &uploadThrottledReadSeeker{ctx: ctx, rs: rs}
	if ra, ok := rs.(io.ReaderAt); ok {
		w.ra = ra
	}
	return w
}

type uploadThrottledReadSeeker struct {
	ctx context.Context
	rs  io.ReadSeeker
	ra  io.ReaderAt // nil when the source cannot read at offsets
}

func (r *uploadThrottledReadSeeker) Read(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := uploadReadLimiter.WaitN(r.ctx, chunk); err != nil {
			return total, err
		}
		n, err := r.rs.Read(p[total : total+chunk])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func (r *uploadThrottledReadSeeker) ReadAt(p []byte, off int64) (int, error) {
	if r.ra == nil {
		return 0, errors.New("storage: source does not support ReadAt")
	}
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := uploadReadLimiter.WaitN(r.ctx, chunk); err != nil {
			return total, err
		}
		n, err := r.ra.ReadAt(p[total:total+chunk], off+int64(total))
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
	return total, nil
}

func (r *uploadThrottledReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.rs.Seek(offset, whence)
}
