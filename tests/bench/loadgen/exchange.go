//go:build bench

package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// maxErrorBody is how much of a non-2xx body is read, enough to leave the
// connection reusable.
const maxErrorBody = 64 << 10

// exchange sends request k and watches the answer to its end.
func (r *runner) exchange(ctx context.Context, k int64) result {
	req, n, err := r.bodies.request(k)
	res := result{stream: r.bodies.streams(k), reqBytes: n}
	if err != nil {
		res.end, res.failure = time.Now(), outTransport
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()
	resp, err := r.client.Do(req.WithContext(ctx))
	if err != nil {
		res.end, res.failure = time.Now(), classify(err)
		return res
	}
	defer resp.Body.Close()
	res.status = resp.StatusCode

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		got, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		res.respBytes, res.end, res.failure = got, time.Now(), outStatus
		return res
	}
	if !res.stream {
		got, err := io.Copy(io.Discard, resp.Body)
		res.respBytes, res.end = got, time.Now()
		switch {
		case err != nil:
			res.failure = classifyRead(err)
		case got == 0:
			res.failure = outBadBody
		}
		return res
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		got, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
		res.respBytes, res.end, res.failure = got, time.Now(), outBadBody
		return res
	}
	var body io.Reader = resp.Body
	if r.cfg.SlowRead.BytesPerSecond > 0 {
		body = newSlowReader(ctx, body, r.cfg.SlowRead.BytesPerSecond)
	}
	s, err := readSSE(body, r.cfg.Dialect)
	res.respBytes, res.first, res.end = s.bytes, s.first, time.Now()
	switch {
	case err != nil:
		res.failure = classifyRead(err)
	case s.errored:
		res.failure = outStreamErr
	case !s.terminal:
		res.failure = outTruncated
	case !s.done.IsZero():
		// A client has its answer when the closing event is complete, and
		// whatever the gateway does between that and closing the response, such
		// as settling a budget, is not the stream's latency. The body is still
		// read to its end, so the connection can be reused.
		res.end = s.done
	}
	return res
}

// classify names why a request got no response.
func classify(err error) outcome {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return outTimeout
	case errors.Is(err, context.Canceled):
		return outCanceled
	case errors.As(err, &netErr) && netErr.Timeout():
		return outTimeout
	}
	return outTransport
}

// classifyRead names why a response that began did not finish: a timeout or
// cancellation as such, and any other failure as a truncated stream.
func classifyRead(err error) outcome {
	if o := classify(err); o != outTransport {
		return o
	}
	return outTruncated
}

// sseResult is what a stream held.
type sseResult struct {
	// first is when the first data frame was complete; frames counts them.
	first  time.Time
	frames int
	bytes  int64
	// terminal is set once the dialect's closing event arrived, and errored
	// when an error event did. done is when the closing event was complete,
	// which is the blank line that ends it; it is zero when no blank line did,
	// as when the response ends in the middle of the event.
	terminal, errored bool
	done              time.Time

	// long is set while the rest of a Gemini data line longer than the read
	// buffer goes by, and scan holds the end of what has been read of it, so
	// that a finish reason beyond the first buffer, or split between two, is
	// still found.
	long bool
	scan []byte
}

var (
	dataPrefix  = []byte("data:")
	eventPrefix = []byte("event:")
	doneValue   = []byte("[DONE]")
	errorStart  = []byte(`{"error"`)
	finishKey   = []byte(`"finishReason"`)
)

// readSSE reads a stream to its end without parsing its payloads. The
// closing event is the dialect's own: [DONE] for OpenAI, message_stop for
// Anthropic, and the chunk that carries a finish reason for Gemini; an
// in-band error is an error event, or a data frame that is an error object.
// A response that ends before its closing event is truncated, which is how
// a mid-stream failure must not be mistaken for a success.
func readSSE(r io.Reader, d Dialect) (sseResult, error) {
	var s sseResult
	br := bufio.NewReaderSize(r, 4096)
	lineStart := true
	for {
		line, err := br.ReadSlice('\n')
		s.bytes += int64(len(line))
		partial := err == bufio.ErrBufferFull
		switch {
		case lineStart && len(line) > 0:
			s.line(line, d, partial)
		case !lineStart:
			s.more(line, partial)
		}
		lineStart = !partial
		if err != nil && !partial {
			if err == io.EOF {
				err = nil
			}
			return s, err
		}
	}
}

// line looks at one line, or the start of a long one, which partial says it is.
func (s *sseResult) line(raw []byte, d Dialect, partial bool) {
	line := bytes.TrimRight(raw, "\r\n")
	switch {
	case len(line) == 0:
		// The blank line that ends an event, which completes a closing one.
		if s.terminal && s.done.IsZero() {
			s.done = time.Now()
		}
	case bytes.HasPrefix(line, dataPrefix):
		value := bytes.TrimSpace(line[len(dataPrefix):])
		s.frames++
		if s.first.IsZero() {
			s.first = time.Now()
		}
		switch {
		case bytes.HasPrefix(value, errorStart):
			s.errored = true
		case d == OpenAI && bytes.Equal(value, doneValue):
			s.terminal = true
		case d == Gemini && bytes.Contains(value, finishKey):
			s.terminal = true
		}
		// A Gemini chunk carries its finish reason after its text, so for a long
		// one the rest of the line is read for it.
		if partial && d == Gemini {
			s.long = true
			s.scan = append(s.scan[:0], raw[max(0, len(raw)-len(finishKey)+1):]...)
		}
	case bytes.HasPrefix(line, eventPrefix):
		switch string(bytes.TrimSpace(line[len(eventPrefix):])) {
		case "error":
			s.errored = true
		case "message_stop":
			if d == Anthropic {
				s.terminal = true
			}
		}
	}
}

// more looks at the rest of a line longer than the read buffer, which only a
// Gemini data line has a use for. The end of what was read is carried into the
// next read, so a key split between two is found.
func (s *sseResult) more(chunk []byte, partial bool) {
	if !s.long {
		return
	}
	s.scan = append(s.scan, chunk...)
	if bytes.Contains(s.scan, finishKey) {
		s.terminal = true
	}
	if !partial {
		s.long, s.scan = false, s.scan[:0]
		return
	}
	keep := min(len(s.scan), len(finishKey)-1)
	s.scan = append(s.scan[:0], s.scan[len(s.scan)-keep:]...)
}
