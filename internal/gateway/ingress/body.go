package ingress

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/oif"
)

// ReadBody bounds both the encoded and decoded body before parsing.
func (g *Gate) ReadBody(r *http.Request) ([]byte, *fault.Error) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, &fault.Error{Status: http.StatusUnsupportedMediaType, Type: "invalid_request_error", Code: "unsupported_media_type", Message: "Request bodies must be application/json."}
	}
	limit := g.maxBody
	encoded := &io.LimitedReader{R: r.Body, N: limit + 1}
	var reader io.Reader = encoded
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(reader)
		if encoded.N == 0 {
			return nil, BodyTooLarge()
		}
		if err != nil {
			return nil, BodyReadError(err)
		}
		defer gz.Close()
		reader = io.LimitReader(gz, limit+1)
	default:
		return nil, &fault.Error{Status: http.StatusUnsupportedMediaType, Type: "invalid_request_error", Code: "unsupported_content_encoding", Message: "Only identity and gzip request encodings are supported."}
	}
	data, err := io.ReadAll(reader)
	// Check the wire cap even when gzip has decoded a smaller body or reports
	// truncation after reaching the encoded limit.
	if encoded.N == 0 || int64(len(data)) > limit {
		return nil, BodyTooLarge()
	}
	if err != nil {
		return nil, BodyReadError(err)
	}
	if measured, ok := r.Body.(*MeasuredBody); ok {
		measured.Decoded = int64(len(data))
	}
	return data, nil
}

func BodyTooLarge() *fault.Error {
	return &fault.Error{Status: http.StatusRequestEntityTooLarge, Type: "invalid_request_error", Code: "request_too_large", Message: "The request body exceeds the configured limit."}
}

func BodyReadError(err error) *fault.Error {
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return &fault.Error{Status: http.StatusRequestTimeout, Type: "invalid_request_error", Code: "request_timeout", Message: "The request body was not received within the deadline."}
	}
	return fault.InvalidRequest("invalid_request", "The request body could not be read.", nil)
}

// ParseObjectRequest rejects an invalid envelope before allocating its source
// index. Accepted native source, including whitespace, remains unchanged.
func ParseObjectRequest(body []byte, bounds oif.Limits) (oif.Document, error) {
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return oif.Document{}, &oif.SourceError{Code: "expected_object"}
	}
	return oif.ParseJSON(body, bounds)
}

// MeasuredBody counts ingress bytes only when a pinned release has route body
// limits. It retains no content. Decoded JSON and declared lengths are checked
// independently so gzip and chunked uploads cannot evade the bound.
type MeasuredBody struct {
	io.ReadCloser
	read, Declared, Decoded int64
}

func (b *MeasuredBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.read += int64(n)
	return n, err
}

func (b *MeasuredBody) Size() int64 {
	if b == nil {
		return 0
	}
	return max(b.read, b.Declared, b.Decoded)
}
