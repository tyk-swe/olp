package sse

import (
	"bufio"
	"bytes"
	"io"
	"slices"
	"unicode/utf8"
)

type RawFrame struct {
	Bytes, Data []byte
	HasData     bool
	ranges      []dataRange
}

type dataRange struct{ start, end, wireStart int }

func (frame RawFrame) ReplaceData(start, end int, replacement []byte) ([]byte, error) {
	index := slices.IndexFunc(frame.ranges, func(part dataRange) bool { return start >= part.start && end <= part.end })
	if index < 0 || start > end {
		return nil, &DecodeError{Detail: "replacement crosses SSE data lines"}
	}
	part := frame.ranges[index]
	wireStart := part.wireStart + start - part.start
	wireEnd := part.wireStart + end - part.start
	return slices.Concat(frame.Bytes[:wireStart], replacement, frame.Bytes[wireEnd:]), nil
}

type RawDecoder struct {
	scanner    *bufio.Scanner
	wire, data []byte
	ranges     []dataRange
}

func NewRawDecoder(reader io.Reader, limit int) *RawDecoder {
	decoder := &RawDecoder{scanner: bufio.NewScanner(reader)}
	decoder.scanner.Buffer(make([]byte, min(4096, max(1, limit)+4)), max(1, limit)+4)
	split := lines(max(1, limit))
	decoder.scanner.Split(func(data []byte, eof bool) (int, []byte, error) {
		advance, token, err := split(data, eof)
		decoder.wire = append(decoder.wire, data[:advance]...)
		return advance, token, err
	})
	return decoder
}

func (decoder *RawDecoder) take(hasData bool) RawFrame {
	frame := RawFrame{Bytes: decoder.wire, Data: decoder.data, HasData: hasData, ranges: decoder.ranges}
	decoder.wire, decoder.data, decoder.ranges = nil, nil, nil
	return frame
}

func (decoder *RawDecoder) Next() (RawFrame, error) {
	for decoder.scanner.Scan() {
		line := decoder.scanner.Bytes()
		if !utf8.Valid(line) {
			return RawFrame{}, &DecodeError{Detail: "invalid UTF-8 in SSE stream"}
		}
		if len(line) == 0 {
			return decoder.take(len(decoder.ranges) > 0), nil
		}
		name, value, _ := bytes.Cut(line, []byte(":"))
		if !bytes.Equal(name, []byte("data")) {
			if len(decoder.ranges) == 0 {
				return decoder.take(false), nil
			}
			continue
		}
		content := bytes.TrimPrefix(value, []byte(" "))
		if len(decoder.ranges) > 0 {
			decoder.data = append(decoder.data, '\n')
		}
		start := len(decoder.data)
		wireStart := bytes.LastIndex(decoder.wire, line) + len(line) - len(content)
		decoder.data = append(decoder.data, content...)
		decoder.ranges = append(decoder.ranges, dataRange{start: start, end: len(decoder.data), wireStart: wireStart})
	}
	if err := decoder.scanner.Err(); err != nil {
		return RawFrame{}, err
	}
	if len(decoder.wire) > 0 {
		return decoder.take(false), nil
	}
	return RawFrame{}, io.EOF
}
