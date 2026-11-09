package upstream

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"

	"github.com/tyk-swe/olp/internal/batches"
)

type LineParser func(line []byte, number int64) (batches.Result, error)

type PageCommit func([]batches.Result, batches.Cursor) error

var emptyPrefixDigest = func() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}()

func readPages(r io.Reader, cursor batches.Cursor, file string, phase batches.Phase, parse LineParser, commit PageCommit, maxBytes int64) (batches.Cursor, int64, string, error) {
	if r == nil || file == "" || phase != batches.PhaseOutput && phase != batches.PhaseError {
		return cursor, 0, "", errors.New("invalid batch result stream")
	}
	resuming := cursor.Phase == phase && cursor.File != ""
	if resuming && cursor.File != file {
		return cursor, 0, "", batches.ErrCursorConflict
	}
	startLine, expectedPrefix := int64(0), ""
	if resuming {
		startLine, expectedPrefix = cursor.Line, cursor.PrefixSHA256
	}
	if startLine == 0 && expectedPrefix != "" && expectedPrefix != emptyPrefixDigest || startLine > 0 && expectedPrefix == "" {
		return cursor, 0, "", batches.ErrCursorConflict
	}

	reader := bufio.NewReaderSize(r, 64<<10)
	prefix, full := sha256.New(), sha256.New()
	var bytesRead, lineNumber int64
	var page []batches.Result
	var pageBytes int
	next := batches.Cursor{Phase: phase, File: file, OutputPrefixSHA256: cursor.OutputPrefixSHA256}
	for {
		raw, ok, err := readLine(reader)
		if err != nil {
			return cursor, bytesRead, "", err
		}
		if !ok {
			break
		}
		bytesRead += int64(len(raw))
		if bytesRead > maxBytes {
			return cursor, bytesRead, "", batches.ErrBatchTooLarge
		}
		full.Write(raw)
		lineNumber++
		if lineNumber <= startLine {
			prefix.Write(raw)
			if lineNumber == startLine && hex.EncodeToString(prefix.Sum(nil)) != expectedPrefix {
				return cursor, bytesRead, "", batches.ErrCursorConflict
			}
			continue
		}
		decoded := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\r'})
		if len(decoded) == 0 {
			return cursor, bytesRead, "", errors.New("batch result line is empty")
		}
		result, err := parse(decoded, lineNumber)
		if err != nil {
			return cursor, bytesRead, "", err
		}
		prefix.Write(raw)
		next.Line = lineNumber
		next.PrefixSHA256 = hex.EncodeToString(prefix.Sum(nil))
		page = append(page, result)
		pageBytes += len(raw)
		if len(page) == batches.PageSize || pageBytes >= batches.MaxPageBytes {
			if err = commit(page, next); err != nil {
				return cursor, bytesRead, "", err
			}
			page, pageBytes = nil, 0
		}
	}
	if lineNumber < startLine {
		return cursor, bytesRead, "", batches.ErrCursorConflict
	}
	if page != nil {
		if err := commit(page, next); err != nil {
			return cursor, bytesRead, "", err
		}
	}
	if lineNumber == startLine {
		next.Line, next.PrefixSHA256 = lineNumber, expectedPrefix
	}
	return next, bytesRead, hex.EncodeToString(full.Sum(nil)), nil
}

func hashLines(r io.Reader, maxBytes int64) (string, int64, error) {
	reader := bufio.NewReaderSize(r, 64<<10)
	hash := sha256.New()
	var total int64
	for {
		raw, ok, err := readLine(reader)
		if err != nil {
			return "", total, err
		}
		if !ok {
			return hex.EncodeToString(hash.Sum(nil)), total, nil
		}
		total += int64(len(raw))
		if total > maxBytes {
			return "", total, batches.ErrBatchTooLarge
		}
		hash.Write(raw)
	}
}

func readLine(reader *bufio.Reader) ([]byte, bool, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > batches.MaxLineBytes+1 {
			return nil, false, batches.ErrLineTooLarge
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
			if len(bytes.TrimSuffix(line, []byte{'\r'})) > batches.MaxLineBytes {
				return nil, false, batches.ErrLineTooLarge
			}
			return line, true, nil
		default:
			return nil, false, err
		}
	}
}
