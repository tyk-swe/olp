//go:build bench

package mockupstream

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

// chunked returns a reader that yields the data in reads of the given sizes,
// repeating the last size, so a test chooses exactly where each read ends.
func chunked(data []byte, sizes ...int) io.Reader {
	return &chunkReader{data: data, sizes: sizes}
}

type chunkReader struct {
	data  []byte
	sizes []int
	n     int
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	size := c.sizes[min(c.n, len(c.sizes)-1)]
	c.n++
	n := copy(p[:min(len(p), size)], c.data)
	c.data = c.data[n:]
	return n, nil
}

type scanResult struct {
	model  string
	stream bool
	bytes  int64
}

func scanOf(t *testing.T, r io.Reader) scanResult {
	t.Helper()
	var s bodyScan
	n, err := s.drain(r, true)
	if err != nil {
		t.Fatal(err)
	}
	return scanResult{string(s.Model()), s.stream, n}
}

func TestBodyScanFindsTheFields(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		model      string
		stream     bool
	}{
		{"compact", `{"model":"gpt-mock","stream":true,"messages":[]}`, "gpt-mock", true},
		{"sorted like OLP", `{"max_tokens":16,"messages":[{"role":"user","content":"hi"}],"model":"upstream-1","stream":false,"stream_options":{"include_usage":true}}`, "upstream-1", false},
		{"python separators", `{"model": "m", "stream": true, "messages": []}`, "m", true},
		{"space before the colon", `{"model" : "m" , "stream" : true}`, "m", true},
		{"newlines", "{\n  \"model\":\n  \"m\",\n  \"stream\":\n  true\n}", "m", true},
		{"stream false", `{"stream":false,"model":"m"}`, "m", false},
		{"no stream", `{"model":"m"}`, "m", false},
		{"no model", `{"stream":true}`, "", true},
		{"neither", `{"messages":[{"role":"user","content":"hello"}]}`, "", false},
		{"empty", ``, "", false},
		// A string value that merely reads "stream" or "model" is no key.
		{"values are not keys", `{"messages":[{"role":"user","content":"stream"},{"role":"model","content":"model"}],"model":"real","stream":true}`, "real", true},
		// A quote inside a string is escaped, so quoted JSON in a prompt never matches.
		{"escaped quotes", `{"messages":[{"role":"user","content":"{\"model\":\"decoy\",\"stream\":true}"}],"model":"real","stream":false}`, "real", false},
		{"stream_options is not stream", `{"stream_options":{"include_usage":true},"model":"m"}`, "m", false},
		{"nested object member skipped", `{"tools":[{"stream":{"type":"boolean"},"model":{"type":"string"}}],"model":"real","stream":true}`, "real", true},
		{"model that is not a string", `{"model":42,"stream":true}`, "", true},
		{"model with a backslash", `{"model":"a\\b","stream":true}`, "", true},
		{"model with a control character", "{\"model\":\"a\tb\",\"stream\":true}", "", true},
		{"unterminated model", `{"stream":true,"model":"abc`, "", true},
		{"empty model", `{"model":"","stream":true}`, "", true},
		{"first stream wins", `{"stream":true,"x":{"stream":false}}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scanOf(t, strings.NewReader(tc.body))
			if got != (scanResult{tc.model, tc.stream, int64(len(tc.body))}) {
				t.Fatalf("got %+v, want model %q stream %v over %d bytes", got, tc.model, tc.stream, len(tc.body))
			}
		})
	}
}

func TestBodyScanModelLength(t *testing.T) {
	for _, n := range []int{1, maxModelLen - 1, maxModelLen} {
		model := strings.Repeat("m", n)
		if got := scanOf(t, strings.NewReader(`{"model":"`+model+`"}`)); got.model != model {
			t.Errorf("a %d byte model came back as %d bytes", n, len(got.model))
		}
	}
	if got := scanOf(t, strings.NewReader(`{"model":"`+strings.Repeat("m", maxModelLen+1)+`"}`)); got.model != "" {
		t.Errorf("an over-long model was accepted: %d bytes", len(got.model))
	}
}

// TestBodyScanIsBoundarySafe moves every read boundary across every position
// of a body whose fields are written with whitespace, including the longest
// model name: the answer must not depend on where reads happen to end.
func TestBodyScanIsBoundarySafe(t *testing.T) {
	model := strings.Repeat("x", maxModelLen)
	for _, tail := range []string{
		`"model"` + strings.Repeat(" ", maxSpace) + `:` + strings.Repeat(" ", maxSpace) + `"` + model + `","stream":true}`,
		`"stream" : true, "model": "` + model + `"}`,
		`"stream":` + strings.Repeat("\n", maxSpace) + `false,"model":"short"}`,
	} {
		body := []byte(`{"filler":"` + strings.Repeat("a", 3000) + `",` + tail)
		want := scanOf(t, bytes.NewReader(body))
		if want.model == "" {
			t.Fatalf("the reference scan found no model in %q", tail[:40])
		}
		// Every split point of the tail, with the filler read in one piece.
		for split := 0; split <= len(body); split++ {
			if got := scanOf(t, chunked(body, split, len(body))); got != want {
				t.Fatalf("split at %d: %+v, want %+v", split, got, want)
			}
		}
	}
}

func TestBodyScanReadShapes(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("word ", 5000) + `"}],"model":"gpt-mock","stream": true}`)
	want := scanResult{"gpt-mock", true, int64(len(body))}
	for name, r := range map[string]io.Reader{
		"one byte":     iotest.OneByteReader(bytes.NewReader(body)),
		"half":         iotest.HalfReader(bytes.NewReader(body)),
		"data and eof": iotest.DataErrReader(bytes.NewReader(body)),
		"sizes 1,7,64": chunked(body, 1, 7, 64),
		"page":         chunked(body, 4096),
		"large":        chunked(body, 3*readSize),
	} {
		if got := scanOf(t, r); got != want {
			t.Errorf("%s: %+v, want %+v", name, got, want)
		}
	}
}

// TestBodyScanAcrossWindows places the keys at every offset around the
// window size, which is where a carried tail must reassemble them.
func TestBodyScanAcrossWindows(t *testing.T) {
	const tail = `,"model":"gpt-mock","stream": true}`
	for pad := readSize - 300; pad <= readSize+30; pad++ {
		body := []byte(`{"p":"` + strings.Repeat("a", pad) + `"` + tail)
		got := scanOf(t, bytes.NewReader(body))
		if got != (scanResult{"gpt-mock", true, int64(len(body))}) {
			t.Fatalf("padding %d: %+v", pad, got)
		}
	}
	// The same with a prompt several windows long, as a 100K-token prompt is.
	body := []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("the ", 400_000/4) + `"}]` + tail)
	if got := scanOf(t, bytes.NewReader(body)); got != (scanResult{"gpt-mock", true, int64(len(body))}) {
		t.Fatalf("large body: %+v", got)
	}
}

func TestDrainWithoutScanningCountsBytes(t *testing.T) {
	var s bodyScan
	body := strings.Repeat(`{"model":"m","stream":true}`, 10_000)
	n, err := s.drain(strings.NewReader(body), false)
	if err != nil || n != int64(len(body)) || s.haveModel || s.haveStream {
		t.Fatalf("%d bytes, error %v, scan %+v", n, err, s)
	}
}

func TestDrainReportsReadErrors(t *testing.T) {
	var s bodyScan
	boom := fmt.Errorf("connection reset")
	n, err := s.drain(io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(boom)), true)
	if err != boom || n != 3 {
		t.Fatalf("%d, %v", n, err)
	}
}
