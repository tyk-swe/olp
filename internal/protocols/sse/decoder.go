// Package sse decodes bounded server-sent events independently of provider codecs.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

type Frame struct {
	Event   *string `json:"event"`
	Data    string  `json:"data"`
	ID      *string `json:"id"`
	RetryMS *uint64 `json:"retry_ms"`
}

// DecodeError identifies invalid SSE framing without classifying reader or
// emit errors, which belong to the transport and consumer respectively.
type DecodeError struct{ Detail string }

func (e *DecodeError) Error() string { return e.Detail }

// ErrEventTooLarge marks an event whose wire bytes exceed the decoder limit.
var ErrEventTooLarge = &DecodeError{Detail: "SSE event exceeds byte limit"}

// Decode handles CR/LF/CRLF, comments, a leading BOM and UTF-8 fragmented at
// arbitrary byte boundaries. EOF does not dispatch an unterminated event.
func Decode(r io.Reader, maxEventBytes int, emit func(Frame) error) error {
	if maxEventBytes < 1 {
		return errors.New("SSE event limit must be positive")
	}
	scanner := bufio.NewScanner(r)
	// Allow a leading BOM and one byte of lookahead at the event limit.
	scanner.Buffer(make([]byte, min(4096, maxEventBytes+4)), maxEventBytes+4)
	scanner.Split(lines(maxEventBytes))
	var id *string
	var event *string
	var retry *uint64
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if !utf8.ValidString(line) {
			return &DecodeError{Detail: "invalid UTF-8 in SSE stream"}
		}
		if line == "" {
			if len(data) > 0 {
				if err := emit(Frame{Event: event, Data: strings.Join(data, "\n"), ID: id, RetryMS: retry}); err != nil {
					return err
				}
			}
			event, retry, data = nil, nil, nil
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		name, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "data":
			data = append(data, value)
		case "event":
			if value != "" {
				event = &value
			} else {
				event = nil
			}
		case "id":
			if !strings.ContainsRune(value, '\x00') {
				id = &value
			}
		case "retry":
			if value != "" && strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
				if n, err := strconv.ParseUint(value, 10, 64); err == nil {
					retry = &n
				}
			}
		}
	}
	return scanner.Err()
}

// The splitter owns wire-byte accounting, including blank lines and CRLF.
func lines(maxEventBytes int) bufio.SplitFunc {
	size := 0
	first := true
	skipLF := false
	provisionalLF := false
	return func(data []byte, eof bool) (int, []byte, error) {
		advance := 0
		if skipLF {
			if len(data) == 0 && !eof {
				return 0, nil, nil
			}
			if len(data) > 0 && data[0] == '\n' {
				advance++
			} else if provisionalLF {
				size--
			}
			skipLF, provisionalLF = false, false
		}
		data = data[advance:]
		if first {
			bom := []byte("\uFEFF")
			if !eof && len(data) < len(bom) && bytes.HasPrefix(bom, data) {
				return advance, nil, nil
			}
			if bytes.HasPrefix(data, bom) {
				advance += len(bom)
				data = data[len(bom):]
			}
			first = false
		}
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			n := i + 1
			trailingCR := data[i] == '\r' && n == len(data) && !eof
			if data[i] == '\r' && n < len(data) && data[n] == '\n' {
				n++
			}
			if n > maxEventBytes-size {
				return 0, nil, ErrEventTooLarge
			}
			if trailingCR {
				// Dispatch now if a possible LF also fits. Only the exact
				// limit needs another byte or EOF to distinguish CR from CRLF.
				if n == maxEventBytes-size {
					return advance, nil, nil
				}
				size++
				skipLF = true
				provisionalLF = i != 0
			}
			size += n
			if i == 0 {
				size = 0
			}
			return advance + n, data[:i], nil
		}
		if len(data) > maxEventBytes-size {
			return 0, nil, ErrEventTooLarge
		}
		if eof && len(data) > 0 {
			return advance + len(data), data, nil
		}
		return advance, nil, nil
	}
}
