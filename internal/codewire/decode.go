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
	if strings.TrimSpace(encoding) == "" || strings.EqualFold(strings.TrimSpace(encoding), "identity") {
		return raw, nil
	}
	reader, err := OpenDecoder(bytes.NewReader(raw), encoding, limit)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, codemode.Refuse(400, "code_encoding_invalid")
	}
	if int64(len(body)) > limit {
		return nil, codemode.Refuse(413, "code_body_too_large")
	}
	return body, nil
}

func OpenDecoder(reader io.Reader, encoding string, limit int64) (io.ReadCloser, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return io.NopCloser(reader), nil
	case "gzip":
		decoded, err := gzip.NewReader(reader)
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		return decoded, nil
	case "zstd":
		decoded, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(limit)))
		if err != nil {
			return nil, codemode.Refuse(400, "code_encoding_invalid")
		}
		return decoded.IOReadCloser(), nil
	default:
		return nil, codemode.Refuse(415, "code_encoding_unsupported")
	}
}

func field(value oif.Value, name string) oif.Value { child, _ := value.Lookup(name); return child }
