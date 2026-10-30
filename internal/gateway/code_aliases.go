package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/protocols/sse"
)

func (a *codeAttempt) copyModelResponse(w http.ResponseWriter, response *http.Response) error {
	reader, closeReader, err := codeModelReader(response.Body, response.Header.Get("Content-Encoding"))
	if err != nil {
		return err
	}
	defer closeReader()
	if !strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		body, err := io.ReadAll(io.LimitReader(reader, codewire.MaxBody+1))
		if err != nil || len(body) > codewire.MaxBody {
			return codemode.Refuse(502, "code_upstream_invalid")
		}
		a.observe(a.observer.Unary(body))
		rewritten, err := codewire.RewriteResponseModel(body, a.model, a.publicModel)
		if err != nil {
			return err
		}
		_, err = w.Write(rewritten)
		return err
	}
	decoder := sse.NewDecoder(reader, codewire.MaxBody)
	decoder.OnComment = func(line string) error {
		_, err := io.WriteString(w, line+"\n")
		http.NewResponseController(w).Flush()
		return err
	}
	for {
		frame, err := decoder.NextFrame()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		a.observe(a.observer.Event([]byte(frame.Data)))
		data := codeModelEvent([]byte(frame.Data), a.model, a.publicModel)
		rewritten := sse.Frame{Event: frame.Event, Data: string(data), ID: frame.ID, RetryMS: frame.RetryMS, Control: frame.Control}
		if _, err := w.Write(rewritten.Encode()); err != nil {
			return err
		}
		http.NewResponseController(w).Flush()
	}
}

func codeModelEvent(body []byte, from, to string) []byte {
	if !bytes.HasPrefix(bytes.TrimSpace(body), []byte("{")) {
		return body
	}
	rewritten, err := codewire.RewriteResponseModel(body, from, to)
	if err != nil {
		return body
	}
	return rewritten
}

func codeModelReader(body io.Reader, encoding string) (io.Reader, func(), error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return body, func() {}, nil
	case "gzip":
		reader, err := gzip.NewReader(body)
		if err != nil {
			return nil, func() {}, codemode.Refuse(502, "code_upstream_invalid")
		}
		return reader, func() { reader.Close() }, nil
	case "zstd":
		reader, err := zstd.NewReader(body, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(codewire.MaxBody))
		if err != nil {
			return nil, func() {}, codemode.Refuse(502, "code_upstream_invalid")
		}
		return reader, reader.Close, nil
	default:
		return nil, func() {}, codemode.Refuse(502, "code_upstream_invalid")
	}
}
