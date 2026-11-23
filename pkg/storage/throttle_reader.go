package storage

import (
	"context"
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
// The limiter is captured once per wrapper, so Read/ReadAt never touch the
// global variable: re-initialising the upload manager cannot swap a nil limiter
// under a wrapper that is mid-upload.
//
// The static return type is io.ReadSeeker, and the dynamic type adds ReadAt only
// when rs supports it: the AWS SDK switches to concurrent io.NewSectionReader
// range reads only for a Body implementing both io.ReaderAt and io.ReadSeeker.
// Hiding ReadAt for capable sources would silently serialize multipart reads;
// advertising it for sources that lack it would hand the SDK a ReaderAt it
// cannot honour.
func NewUploadThrottledReadSeeker(ctx context.Context, rs io.ReadSeeker) io.ReadSeeker {
	lim := uploadReadLimiter
	if lim == nil {
		return rs
	}
	w := &uploadThrottledReadSeeker{ctx: ctx, lim: lim, rs: rs}
	if ra, ok := rs.(io.ReaderAt); ok {
		return &uploadThrottledReadSeekerAt{uploadThrottledReadSeeker: w, ra: ra}
	}
	return w
}

// uploadThrottledReadSeeker paces reads through a shared token bucket.
// It deliberately does not implement io.ReaderAt; uploadThrottledReadSeekerAt
// does, for sources that support it.
type uploadThrottledReadSeeker struct {
	ctx context.Context
	lim *rate.Limiter
	rs  io.ReadSeeker
}

func (r *uploadThrottledReadSeeker) Read(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := r.lim.WaitN(r.ctx, chunk); err != nil {
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

func (r *uploadThrottledReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return r.rs.Seek(offset, whence)
}

// uploadThrottledReadSeekerAt adds ReadAt for sources that implement it.
type uploadThrottledReadSeekerAt struct {
	*uploadThrottledReadSeeker
	ra io.ReaderAt
}

func (r *uploadThrottledReadSeekerAt) ReadAt(p []byte, off int64) (int, error) {
	total := 0
	for total < len(p) {
		chunk := len(p) - total
		if chunk > maxThrottleChunk {
			chunk = maxThrottleChunk
		}
		if err := r.lim.WaitN(r.ctx, chunk); err != nil {
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
