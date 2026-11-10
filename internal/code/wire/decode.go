package wire

import (
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/tyk-swe/olp/internal/code"
	"github.com/tyk-swe/olp/internal/oif"
)

const MaxBody = 16 << 20

func Decode(raw []byte, encoding string, limit int64) ([]byte, error) {
	if int64(len(raw)) > limit {
		return nil, code.Refuse(413, "code_body_too_large")
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
		return nil, code.Refuse(400, "code_encoding_invalid")
	}
	if int64(len(body)) > limit {
		return nil, code.Refuse(413, "code_body_too_large")
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
			return nil, code.Refuse(400, "code_encoding_invalid")
		}
		return decoded, nil
	case "zstd":
		decoded, err := zstd.NewReader(reader, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(limit)))
		if err != nil {
			return nil, code.Refuse(400, "code_encoding_invalid")
		}
		return decoded.IOReadCloser(), nil
	default:
		return nil, code.Refuse(415, "code_encoding_unsupported")
	}
}

func RejectionBody(body []byte) []byte {
	if len(body) == 0 || len(body) > 64<<10 {
		return nil
	}
	return append([]byte(nil), body...)
}

func field(value oif.Value, name string) oif.Value {
	child, _ := value.Lookup(name)
	return child
}

func object(data []byte) (oif.Value, bool) {
	document, err := oif.ParseJSON(data, oif.Limits{MaxBytes: MaxBody})
	if err != nil || document.Root().Kind() != oif.Object {
		return oif.Value{}, false
	}
	return document.Root(), true
}

func count(value oif.Value) *int64 {
	if value.Kind() != oif.Number {
		return nil
	}
	n, err := strconv.ParseInt(value.Raw(), 10, 64)
	if err != nil || n < 0 || n > 1<<53-1 {
		return nil
	}
	return &n
}

func estimateTokens(body []byte, root oif.Value, capFields ...string) int64 {
	estimate := max(1, int64(len(body))/4)
	for _, name := range capFields {
		if capTokens, err := strconv.ParseInt(field(root, name).Raw(), 10, 64); err == nil && capTokens > 0 && capTokens <= 1<<32 {
			return estimate + capTokens
		}
	}
	return estimate
}
