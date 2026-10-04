//go:build bench

package mockupstream

import (
	"bytes"
	"io"
	"sync"
)

const (
	// readSize is the drain buffer. A 100K-token prompt is about 400 KB, so a
	// request takes a handful of reads.
	readSize = 64 << 10
	// maxModelLen bounds the model name the scan reports.
	maxModelLen = 200
	// maxSpace bounds the whitespace tolerated around a key's colon.
	maxSpace = 16
	// carry is how much of one window the next one re-reads, so a key split
	// across reads is still found. It holds the longest match: a key, its
	// colon with whitespace on both sides, a quote, a full model name and the
	// closing quote, with a little to spare.
	carry = len(`"model"`) + 2*maxSpace + 2 + maxModelLen + 1 + 8
)

var (
	keyModel  = []byte(`"model"`)
	keyStream = []byte(`"stream"`)
	valTrue   = []byte("true")
	valFalse  = []byte("false")
)

var readPool = sync.Pool{New: func() any { b := make([]byte, readSize); return &b }}

// bodyScan drains a request body and, on the way, finds the first top-level
// looking `"model": "<name>"` and `"stream": true|false`. The body is never
// parsed or retained: a 100K-token prompt only passes through a pooled buffer.
//
// OLP encodes its upstream request from a map, so those two keys follow the
// whole prompt, and a client may write them with spaces after the colon. A key
// that is merely a string value cannot match, because a quote inside a JSON
// string is escaped and the key is followed by a colon. A nested object with a
// `model` or `stream` member before the real one would be mistaken for it,
// which the bench workloads do not produce.
type bodyScan struct {
	model      [maxModelLen]byte
	modelLen   int
	haveModel  bool
	stream     bool
	haveStream bool
}

// Model is the model name the body declared, if any. It is only valid until
// the scan is reused.
func (s *bodyScan) Model() []byte {
	if !s.haveModel {
		return nil
	}
	return s.model[:s.modelLen]
}

func (s *bodyScan) done() bool { return s.haveModel && s.haveStream }

// drain reads r to the end and returns the number of bytes it held. With scan
// set it also fills in the scan's fields.
func (s *bodyScan) drain(r io.Reader, scan bool) (int64, error) {
	bp := readPool.Get().(*[]byte)
	defer readPool.Put(bp)
	buf := *bp
	var total int64
	keep := 0
	for {
		n, err := r.Read(buf[keep:])
		total += int64(n)
		if scan && n > 0 && !s.done() {
			window := buf[:keep+n]
			s.feed(window)
			if s.done() {
				keep = 0
			} else {
				keep = min(len(window), carry)
				copy(buf, window[len(window)-keep:])
			}
		}
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return total, err
		}
	}
}

// feed looks for the fields it has not yet found in one window. A match that
// the window ends in the middle of stays in the carried tail and completes on
// the next call.
func (s *bodyScan) feed(p []byte) {
	if !s.haveStream {
		for from := 0; from < len(p); {
			i := bytes.Index(p[from:], keyStream)
			if i < 0 {
				break
			}
			i += from
			from = i + len(keyStream)
			rest, ok := afterColon(p[from:])
			if !ok {
				continue
			}
			if bytes.HasPrefix(rest, valTrue) {
				s.stream, s.haveStream = true, true
				break
			}
			if bytes.HasPrefix(rest, valFalse) {
				s.haveStream = true
				break
			}
		}
	}
	if !s.haveModel {
		for from := 0; from < len(p); {
			i := bytes.Index(p[from:], keyModel)
			if i < 0 {
				break
			}
			from += i + len(keyModel)
			rest, ok := afterColon(p[from:])
			if !ok || len(rest) == 0 || rest[0] != '"' {
				continue
			}
			value := rest[1:]
			end := bytes.IndexByte(value, '"')
			if end < 0 || end > maxModelLen || !safeModel(value[:end]) {
				continue
			}
			s.modelLen, s.haveModel = copy(s.model[:], value[:end]), true
			break
		}
	}
}

// afterColon skips optional whitespace, a colon and more whitespace.
func afterColon(p []byte) ([]byte, bool) {
	p = skipSpace(p)
	if len(p) == 0 || p[0] != ':' {
		return nil, false
	}
	return skipSpace(p[1:]), true
}

func skipSpace(p []byte) []byte {
	for i := 0; i < len(p) && i < maxSpace; i++ {
		if c := p[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
			return p[i:]
		}
	}
	return p[min(len(p), maxSpace):]
}

// safeModel accepts what can be copied into a JSON string unescaped.
func safeModel(b []byte) bool {
	if len(b) == 0 || len(b) > maxModelLen {
		return false
	}
	for _, c := range b {
		if c < 0x20 || c == '"' || c == '\\' || c >= 0x7f {
			return false
		}
	}
	return true
}
