package sse_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func TestRetainedFragmentedSSE(t *testing.T) {
	expected := testutil.JSON[struct {
		FragmentBytes int         `json:"fragment_bytes"`
		Frames        []sse.Frame `json:"frames"`
	}](t, fixtures.Files, "streams/generic-fragmented.expected.json")
	data, err := fixtures.Files.ReadFile("streams/generic-fragmented.sse")
	if err != nil {
		t.Fatal(err)
	}
	var frames []sse.Frame
	err = sse.Decode(testutil.Fragmented(data, expected.FragmentBytes), 4096, func(f sse.Frame) error { frames = append(frames, f); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(frames, expected.Frames) {
		t.Fatalf("frames: %#v; want %#v", frames, expected.Frames)
	}
}

func TestRetainedProviderStreamsHaveValidJSONFrames(t *testing.T) {
	// This qualifies framing only. Canonical text/usage expectations belong to
	// the provider codec milestones and remain untouched in the same corpus.
	files, err := fs.Glob(fixtures.Files, "streams/*.sse")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.Contains(name, "generic") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			data, _ := fixtures.Files.ReadFile(name)
			frames := 0
			err := sse.Decode(testutil.Fragmented(data, 1), 1<<20, func(f sse.Frame) error {
				frames++
				if f.Data != "[DONE]" && !json.Valid([]byte(f.Data)) {
					t.Errorf("invalid JSON: %s", f.Data)
				}
				return nil
			})
			if err != nil || frames == 0 {
				t.Fatalf("frames=%d error=%v", frames, err)
			}
		})
	}
}

func TestFramingLimitsAndTermination(t *testing.T) {
	for _, input := range []string{strings.Repeat("x", 20), "data: abc\ndata: def\n\n", "data: \xff\n\n"} {
		if err := sse.Decode(strings.NewReader(input), 15, func(sse.Frame) error { return nil }); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
	var frames []sse.Frame
	err := sse.Decode(testutil.Fragmented([]byte("\ufeffdata: first\r\rid: x\r\ndata: second\r\n\r\ndata: incomplete"), 1), 100, func(f sse.Frame) error { frames = append(frames, f); return nil })
	if err != nil || len(frames) != 2 || frames[0].Data != "first" || frames[1].Data != "second" || *frames[1].ID != "x" {
		t.Fatalf("frames=%+v error=%v", frames, err)
	}
	sentinel := errors.New("stop")
	if err := sse.Decode(strings.NewReader("data: x\n\n"), 100, func(sse.Frame) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
}

func TestCRDispatchesBeforeAnotherReadOrEOF(t *testing.T) {
	for _, input := range []string{"data: x\r\r", "data: x\r\n\r"} {
		for _, width := range []int{1, len(input)} {
			t.Run(fmt.Sprintf("%q/width=%d", input, width), func(t *testing.T) {
				reader, writer := io.Pipe()
				defer reader.Close()
				defer writer.Close()
				stop := errors.New("frame received while stream is open")
				done := make(chan error, 1)
				go func() {
					defer reader.Close()
					done <- sse.Decode(reader, 100, func(frame sse.Frame) error {
						if frame.Data != "x" {
							return fmt.Errorf("data = %q; want x", frame.Data)
						}
						return stop
					})
				}()
				for offset := 0; offset < len(input); offset += width {
					if _, err := io.WriteString(writer, input[offset:min(offset+width, len(input))]); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case err := <-done:
					if !errors.Is(err, stop) {
						t.Fatal(err)
					}
				case <-time.After(time.Second):
					t.Fatal("complete CR-terminated event waited for another byte or EOF")
				}
			})
		}
	}
}

func TestEventLimitCountsWireBytesAcrossFragmentation(t *testing.T) {
	for _, tc := range []struct {
		name, wire, data string
	}{
		{"lf", "data: x\n\n", "x"},
		{"cr", "data: x\r\r", "x"},
		{"crlf", "data: x\r\n\r\n", "x"},
		{"mixed", "data: x\r\n\r", "x"},
		{"multiline", "data: x\rdata: y\n\r\n", "x\ny"},
		{"comments and ignored fields", ": keepalive\r\nunknown: value\r\ndata: x\r\n\r\n", "x"},
		{"empty data", "data:\r\n\r\n", ""},
		{"utf8", "data: héllo 🌍\r\n\r\n", "héllo 🌍"},
		{"bom", "\ufeffdata: x\r\n\r\n", "x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A second event verifies that accounting resets and a delayed LF
			// belongs to the preceding CR, even at the exact event limit.
			withoutBOM := strings.TrimPrefix(tc.wire, "\ufeff")
			wire := tc.wire + withoutBOM
			for width := 1; width <= len(wire); width++ {
				for _, limit := range []int{len(withoutBOM) - 1, len(withoutBOM), len(withoutBOM) + 1} {
					var frames []sse.Frame
					err := sse.Decode(testutil.Fragmented([]byte(wire), width), limit, func(frame sse.Frame) error {
						frames = append(frames, frame)
						return nil
					})
					if limit < len(withoutBOM) {
						if err == nil || len(frames) != 0 {
							t.Fatalf("width=%d limit=%d: frames=%+v error=%v; want rejection before dispatch", width, limit, frames, err)
						}
					} else if want := []sse.Frame{{Data: tc.data}, {Data: tc.data}}; err != nil || !reflect.DeepEqual(frames, want) {
						t.Fatalf("width=%d limit=%d: frames=%+v error=%v; want %+v", width, limit, frames, err, want)
					}
				}
			}
		})
	}
}

func TestEventLimitCountsUnterminatedInputWithoutAddingDelimiters(t *testing.T) {
	for _, wire := range []string{"data: x", "data: x\n", "data: x\r", "data: x\r\n", "data: x\npending", strings.Repeat("x", 8192)} {
		for _, width := range []int{1, len(wire)} {
			for _, limit := range []int{len(wire) - 1, len(wire)} {
				frames := 0
				err := sse.Decode(testutil.Fragmented([]byte(wire), width), limit, func(sse.Frame) error {
					frames++
					return nil
				})
				if (err != nil) != (limit < len(wire)) || frames != 0 {
					t.Fatalf("wire bytes=%d width=%d limit=%d: frames=%d error=%v", len(wire), width, limit, frames, err)
				}
			}
		}
	}
}

func TestControlledUpstreamFragmentationAndCancellation(t *testing.T) {
	finished := make(chan error, 1)
	upstream := testutil.Upstream(t, func(w http.ResponseWriter, r *http.Request) {
		finished <- testutil.Stream(w, r, []byte(strings.Repeat("data: héllo 🌍\n\n", 1000)), 8, time.Millisecond)
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := upstream.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("first frame received")
	err = sse.Decode(response.Body, 4096, func(frame sse.Frame) error {
		if frame.Data != "héllo 🌍" {
			t.Errorf("fragmented UTF-8: %q", frame.Data)
		}
		return stop
	})
	response.Body.Close()
	cancel()
	if !errors.Is(err, stop) {
		t.Fatalf("stream: %v", err)
	}
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("upstream kept writing after cancellation")
		}
	case <-time.After(time.Second):
		t.Fatal("canceled upstream did not stop")
	}
}
