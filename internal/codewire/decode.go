// Package codewire observes code-mode wire messages without rewriting them:
// bounded body decoding, header forwarding, SSE event splitting, and the
// Anthropic Messages and Chat Completions observers and client identities.
package codewire

import (
	"bytes"
	"compress/gzip"
	"io"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/oif"
)

const MaxBody = 16 << 20

func Decode(raw []byte, encoding string, limit int64) ([]byte, error) {
	if int64(len(raw)) > limit {
		return nil, codemode.Refuse(413, "code_body_too_large")
	}
	var reader io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		defer gz.Close()
		reader = gz
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(limit)))
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		defer zr.Close()
		reader = zr
	default:
		return nil, codemode.Refuse(415, "code_encoding_unsupported")
	}
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, codemode.Refuse(400, "code_encoding_invalid")
	}
	if int64(len(body)) > limit {
		return nil, codemode.Refuse(413, "code_body_too_large")
	}
	return body, nil
}

func field(value oif.Value, name string) oif.Value { child, _ := value.Lookup(name); return child }
