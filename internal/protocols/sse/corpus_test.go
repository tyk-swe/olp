package sse

import (
	"bytes"
	"io"
)

type fragments struct {
	reader io.Reader
	size   int
}

func (f fragments) Read(p []byte) (int, error) { return f.reader.Read(p[:min(len(p), f.size)]) }
func fragmented(data []byte, size int) io.Reader {
	if size < 1 {
		panic("fragment size must be positive")
	}
	return fragments{bytes.NewReader(data), size}
}
