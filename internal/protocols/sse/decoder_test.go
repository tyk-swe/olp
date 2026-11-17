package sse

import (
	"bufio"
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

// This checks framing only. The provider codec tests own the canonical
// text and usage expectations in the same corpus.

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
					var frames []Frame
					err := Decode(fragmented([]byte(wire), width), limit, func(frame Frame) error {
						frames = append(frames, frame)
						return nil
					})
					if limit < len(withoutBOM) {
						if err == nil || len(frames) != 0 {
							t.Fatalf("width=%d limit=%d: frames=%+v error=%v; want rejection before dispatch", width, limit, frames, err)
						}
					} else if want := []Frame{{Data: tc.data}, {Data: tc.data}}; err != nil || !reflect.DeepEqual(frames, want) {
						t.Fatalf("width=%d limit=%d: frames=%+v error=%v; want %+v", width, limit, frames, err, want)
					}
				}
			}
		})
	}
}

func TestFragmentedLineSearchWorkIsBounded(t *testing.T) {
	line := "data: " + strings.Repeat("x", 8<<10)
	wire := line + "\r\n\r\n"
	scanner := bufio.NewScanner(fragmented([]byte(wire), 1))
	examined := 0
	scanner.Split(lines(len(wire), func(data []byte, delimiters string) int {
		examined += len(data)
		return bytes.IndexAny(data, delimiters)
	}))
	if !scanner.Scan() || scanner.Text() != line || !scanner.Scan() || len(scanner.Bytes()) != 0 {
		t.Fatalf("failed to read fragmented event: %v", scanner.Err())
	}
	if budget := 2 * len(wire); examined > budget {
		t.Fatalf("line ending search examined %d bytes for %d wire bytes, want at most %d", examined, len(wire), budget)
	}
}

func TestDecoderRetainsFragmentedFrameValues(t *testing.T) {
	const keepalive = ": keepalive"
	firstData := strings.Repeat("héllo ", 1024)
	secondData := strings.Repeat("goodbye", 1024)
	firstWire := keepalive + "\r\nevent: delta\r\nid: first\r\nretry: 0010\r\ndata: " + firstData + "\r\n\r"
	wire := []byte("\ufeff" + firstWire + "data: " + secondData + "\n\n")
	event, id, retry := "delta", "first", uint64(10)
	wantFirst := Frame{Event: &event, ID: &id, RetryMS: &retry, Data: firstData}
	wantSecond := Frame{Data: secondData}
	for _, width := range []int{1, 4096} {
		decoder := NewDecoder(fragmented(wire, width), len(firstWire))
		var comment string
		decoder.OnComment = func(line string) error {
			comment = line
			return nil
		}
		first, err := decoder.NextFrame()
		if err != nil {
			t.Fatalf("width %d: first frame: %v", width, err)
		}
		second, err := decoder.NextFrame()
		if err != nil || !reflect.DeepEqual(second, wantSecond) {
			t.Fatalf("width %d: unexpected second frame, error %v", width, err)
		}
		if !reflect.DeepEqual(first, wantFirst) || comment != keepalive {
			t.Fatalf("width %d: first frame or comment changed after reading the second frame", width)
		}
	}
	oversized := NewDecoder(fragmented(wire, 1), len(firstWire)-1)
	if _, err := oversized.NextFrame(); err != ErrEventTooLarge {
		t.Fatalf("limit one byte below the wire size: got %v, want %v", err, ErrEventTooLarge)
	}
}

func TestRawFramesPreserveLiteralFramingAndDataRanges(t *testing.T) {
	wire := "\xef\xbb\xbf: before\r\nid: one\r\nretry: 0010\r\nx-vendor: yes\r\ndata: {\r\n: middle\r\ndata:\t\"model\" : \"native\"}\r\n\r\n: after\n"
	for _, width := range []int{1, 2, 7, len(wire)} {
		decoder := NewRawDecoder(fragmented([]byte(wire), width), 4096)
		var output strings.Builder
		for {
			frame, err := decoder.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			if !frame.HasData {
				output.Write(frame.Bytes)
				continue
			}
			if string(frame.Data) != "{\n\t\"model\" : \"native\"}" {
				t.Fatalf("width %d changed joined data %q", width, frame.Data)
			}
			start := bytes.Index(frame.Data, []byte(`"native"`))
			rewritten, err := frame.ReplaceData(start, start+len(`"native"`), []byte(`"alias"`))
			if err != nil {
				t.Fatal(err)
			}
			output.Write(rewritten)
		}
		if output.String() != strings.Replace(wire, `"native"`, `"alias"`, 1) {
			t.Fatalf("width %d changed raw framing %q", width, output.String())
		}
	}
}

func TestRawFrameReplacementKeepsDataLineBoundaries(t *testing.T) {
	const wire = "id: event\r\ndata: first\r\n: keepalive\r\ndata: last\r\n\r\n"
	for _, test := range []struct {
		name, want string
		start, end int
	}{
		{"whole first line", strings.Replace(wire, "first", "alias", 1), 0, 5},
		{"whole last line", strings.Replace(wire, "last", "alias", 1), 6, 10},
		{"insert at start", strings.Replace(wire, "first", "aliasfirst", 1), 0, 0},
		{"insert at end", strings.Replace(wire, "last", "lastalias", 1), 10, 10},
		{"crosses lines", "", 4, 7},
		{"reversed", "", 4, 2},
		{"before data", "", -1, 2},
		{"after data", "", 7, 11},
	} {
		t.Run(test.name, func(t *testing.T) {
			decoder := NewRawDecoder(strings.NewReader(wire), len(wire))
			control, controlErr := decoder.Next()
			frame, frameErr := decoder.Next()
			if controlErr != nil || frameErr != nil || control.HasData || string(control.Bytes) != "id: event\r\n" || !frame.HasData || string(frame.Data) != "first\nlast" {
				t.Fatalf("invalid raw frames %+v %+v, errors %v %v", control, frame, controlErr, frameErr)
			}
			replaced, err := frame.ReplaceData(test.start, test.end, []byte("alias"))
			if test.want == "" {
				if err == nil {
					t.Fatalf("invalid replacement produced %q, error %v", replaced, err)
				}
				return
			}
			if err != nil || string(control.Bytes)+string(replaced) != test.want {
				t.Fatalf("replacement changed framing %q, error %v", replaced, err)
			}
		})
	}
}
