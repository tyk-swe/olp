//go:build bench

package loadgen

import (
	"context"
	"io"
	"time"
)

// slowReader reads at a fixed pace: after n bytes it lets n/pace seconds
// pass, measured from the first read rather than per read so timer lateness
// does not accumulate. Each read is delivered at once and the wait comes
// before the next one, so a stream's first bytes are not held back.
type slowReader struct {
	r     io.Reader
	ctx   context.Context
	pace  int64 // bytes per second
	chunk int
	start time.Time
	read  int64
	timer *time.Timer
}

func newSlowReader(ctx context.Context, r io.Reader, bytesPerSecond int) *slowReader {
	// A tenth of a second of data per read, within a sensible range.
	chunk := min(max(bytesPerSecond/10, 1), 4096)
	return &slowReader{r: r, ctx: ctx, pace: int64(bytesPerSecond), chunk: chunk}
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.start.IsZero() {
		s.start = time.Now()
	} else if err := s.wait(); err != nil {
		return 0, err
	}
	if len(p) > s.chunk {
		p = p[:s.chunk]
	}
	n, err := s.r.Read(p)
	s.read += int64(n)
	return n, err
}

// wait sleeps until the bytes read so far are due.
func (s *slowReader) wait() error {
	due := s.start.Add(time.Duration(s.read * int64(time.Second) / s.pace))
	d := time.Until(due)
	if d <= 0 {
		return s.ctx.Err()
	}
	if s.timer == nil {
		s.timer = time.NewTimer(d)
	} else {
		s.timer.Reset(d)
	}
	select {
	case <-s.timer.C:
		return nil
	case <-s.ctx.Done():
		s.timer.Stop()
		return s.ctx.Err()
	}
}
