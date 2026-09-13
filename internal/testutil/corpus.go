// Package testutil provides behavioral fixtures without constructing application
// composition. Service tests have their own explicit integration build tag.
package testutil

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func JSON[T any](t testing.TB, files fs.FS, name string) T {
	t.Helper()
	data, err := fs.ReadFile(files, name)
	if err != nil {
		t.Fatal(err)
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value
}

type fragments struct {
	reader io.Reader
	size   int
}

func (f fragments) Read(p []byte) (int, error) { return f.reader.Read(p[:min(len(p), f.size)]) }
func Fragmented(data []byte, size int) io.Reader {
	if size < 1 {
		panic("fragment size must be positive")
	}
	return fragments{bytes.NewReader(data), size}
}

// Upstream installs a controlled HTTP transport boundary, automatically cleaned
// up even when the calling test fails.
func Upstream(t testing.TB, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(handler)
	t.Cleanup(s.Close)
	return s
}

// Stream writes actual HTTP chunks, stopping immediately on client cancellation.
func Stream(w http.ResponseWriter, r *http.Request, data []byte, size int, delay time.Duration) error {
	if size < 1 {
		panic("fragment size must be positive")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for len(data) > 0 {
		if err := r.Context().Err(); err != nil {
			return err
		}
		n := min(size, len(data))
		if _, err := w.Write(data[:n]); err != nil {
			return err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return err
		}
		data = data[n:]
		if delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return r.Context().Err()
			case <-timer.C:
			}
		}
	}
	return nil
}
