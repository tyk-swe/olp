package batches

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
)

const (
	MaxLineBytes  = 1 << 20
	MaxBatchBytes = 512 << 20
	PageSize      = 100
	MaxPageBytes  = 1 << 20
)

var (
	ErrCursorConflict = errors.New("batch result prefix changed")
	ErrLineTooLarge   = errors.New("batch result line exceeds the limit")
	ErrBatchTooLarge  = errors.New("batch result files exceed the limit")
)

type Cursor struct {
	Phase              string `json:"phase,omitempty"`
	File               string `json:"file,omitempty"`
	Line               int64  `json:"line,omitempty"`
	PrefixSHA256       string `json:"prefix_sha256,omitempty"`
	OutputPrefixSHA256 string `json:"output_prefix_sha256,omitempty"`
}

type ResultLineParser func([]byte) (Result, error)

type ResultPageCommit func([]Result, Cursor) error

// checkpointCursor advances result progress without losing submission identity.
func checkpointCursor(previous []byte, next any) ([]byte, error) {
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	var old, progress map[string]json.RawMessage
	if len(previous) > 0 {
		if err = json.Unmarshal(previous, &old); err != nil {
			return nil, err
		}
	}
	if err = json.Unmarshal(encoded, &progress); err != nil {
		return nil, err
	}
	if submission, ok := old["submission"]; ok {
		progress["submission"] = submission
	}
	return json.Marshal(progress)
}

func ReadJSONLPages(r io.Reader, cursor Cursor, file, phase string, parse ResultLineParser, commit ResultPageCommit) (Cursor, int64, string, error) {
	if r == nil || file == "" || phase != "output" && phase != "error" || parse == nil || commit == nil {
		return cursor, 0, "", errors.New("invalid batch result stream")
	}
	if cursor.Phase == phase && cursor.File != "" && cursor.File != file {
		return cursor, 0, "", ErrCursorConflict
	}
	startLine := int64(0)
	expectedPrefix := ""
	if cursor.Phase == phase && cursor.File == file {
		startLine = cursor.Line
		expectedPrefix = cursor.PrefixSHA256
	}
	hash := sha256.New()
	if startLine == 0 {
		if expectedPrefix != "" && expectedPrefix != hex.EncodeToString(hash.Sum(nil)) {
			return cursor, 0, "", ErrCursorConflict
		}
	} else if expectedPrefix == "" {
		return cursor, 0, "", ErrCursorConflict
	}

	reader := bufio.NewReaderSize(r, 64<<10)
	var bytesRead int64
	var lineNumber int64
	var page []Result
	var pageBytes int
	next := Cursor{Phase: phase, File: file, OutputPrefixSHA256: cursor.OutputPrefixSHA256}
	fullHash := sha256.New()
	for {
		raw, ok, err := readJSONLLine(reader)
		if err != nil {
			return cursor, bytesRead, "", err
		}
		if !ok {
			break
		}
		bytesRead += int64(len(raw))
		if bytesRead > MaxBatchBytes {
			return cursor, bytesRead, "", ErrBatchTooLarge
		}
		fullHash.Write(raw)
		lineNumber++
		if lineNumber <= startLine {
			hash.Write(raw)
			if lineNumber == startLine && hex.EncodeToString(hash.Sum(nil)) != expectedPrefix {
				return cursor, bytesRead, "", ErrCursorConflict
			}
			continue
		}
		decoded := bytes.TrimSuffix(raw, []byte{'\n'})
		decoded = bytes.TrimSuffix(decoded, []byte{'\r'})
		if len(decoded) == 0 {
			return cursor, bytesRead, "", errors.New("batch result line is empty")
		}
		result, err := parse(decoded)
		if err != nil {
			return cursor, bytesRead, "", err
		}
		hash.Write(raw)
		next.Line = lineNumber
		next.PrefixSHA256 = hex.EncodeToString(hash.Sum(nil))
		page = append(page, result)
		pageBytes += len(raw)
		if len(page) == PageSize || pageBytes >= MaxPageBytes {
			if err = commit(page, next); err != nil {
				return cursor, bytesRead, "", err
			}
			page = nil
			pageBytes = 0
		}
	}
	if lineNumber < startLine {
		return cursor, bytesRead, "", ErrCursorConflict
	}
	if page != nil {
		if err := commit(page, next); err != nil {
			return cursor, bytesRead, "", err
		}
	}
	if lineNumber == startLine {
		next.Line, next.PrefixSHA256 = lineNumber, expectedPrefix
	}
	return next, bytesRead, hex.EncodeToString(fullHash.Sum(nil)), nil
}

func readJSONLLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > MaxLineBytes+1 {
			return nil, false, ErrLineTooLarge
		}
		line = append(line, part...)
		switch err {
		case nil:
			return line, true, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if len(line) == 0 {
				return nil, false, nil
			}
			if len(bytes.TrimSuffix(line, []byte{'\r'})) > MaxLineBytes {
				return nil, false, ErrLineTooLarge
			}
			return line, true, nil
		default:
			return nil, false, err
		}
	}
}
