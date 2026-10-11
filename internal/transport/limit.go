package transport

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"golang.org/x/time/rate"

	"filesync/internal/proto"
)

// limitSlice is the largest piece of a frame handed to the rate limiter
// at once. With burst = chunk size (1 MiB), a 64 KiB slice is always
// within burst, so WaitN never fails for size reasons.
const limitSlice = 64 << 10

// writeLimited frames m into a buffer (which also validates it), then
// writes it in slices, waiting for limiter tokens before each slice.
// Send time for S bytes at rate R with burst B is therefore at least
// (S-B)/R once S exceeds B, as the spec's bandwidth test requires.
func writeLimited(ctx context.Context, w io.Writer, m proto.Message, lim *rate.Limiter) error {
	var buf bytes.Buffer
	if err := proto.WriteMessage(&buf, m); err != nil {
		return err
	}
	b := buf.Bytes()
	for len(b) > 0 {
		n := limitSlice
		if len(b) < n {
			n = len(b)
		}
		if err := lim.WaitN(ctx, n); err != nil {
			return fmt.Errorf("transport: bandwidth wait: %w", err)
		}
		if _, err := w.Write(b[:n]); err != nil {
			return fmt.Errorf("transport: write frame slice: %w", err)
		}
		b = b[n:]
	}
	return nil
}
