package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/sse"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

type paidUsageObservation struct {
	usage    *wire.Usage
	complete bool
}

type paidUsageWriter struct{ writer *io.PipeWriter }

func (w paidUsageWriter) Write(data []byte) (int, error) {
	_, _ = w.writer.Write(data)
	return len(data), nil
}

func (a *codeAttempt) meterPaidResponse(response *http.Response) (*http.Response, func()) {
	if a.paid == nil {
		return response, func() {}
	}
	reader, writer := io.Pipe()
	observed := make(chan paidUsageObservation, 1)
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoding := response.Header.Get("Content-Encoding")
	go func() {
		defer reader.Close()
		observed <- readPaidUsage(reader, a.paid.family, streaming, encoding, a.paid.route.Slug)
	}()
	forwarded := *response
	forwarded.Body = io.NopCloser(io.TeeReader(response.Body, paidUsageWriter{writer: writer}))
	return &forwarded, func() {
		_ = writer.Close()
		result := <-observed
		a.paid.usage = result.usage
		a.paid.usageComplete = result.complete
	}
}

func readPaidUsage(reader io.Reader, family wire.Format, streaming bool, encoding, route string) paidUsageObservation {
	if streaming && (encoding == "" || encoding == "identity") {
		completion, err := protocols.Stream(family, family, reader, codewire.MaxBody, route, true, func([]byte) error { return nil })
		if completion == nil {
			return paidUsageObservation{}
		}
		return paidUsageObservation{usage: completion.Usage, complete: err == nil && completion.UsageComplete}
	}
	body, err := io.ReadAll(io.LimitReader(reader, codewire.MaxBody+1))
	if err != nil || len(body) > codewire.MaxBody {
		return paidUsageObservation{}
	}
	decoded, err := codewire.Decode(body, encoding, codewire.MaxBody)
	if err != nil {
		return paidUsageObservation{}
	}
	if streaming {
		return readPaidUsage(bytes.NewReader(decoded), family, true, "", route)
	}
	completion, err := protocols.Decode(family, family, decoded, route, "")
	if completion == nil {
		return paidUsageObservation{}
	}
	return paidUsageObservation{usage: completion.Usage, complete: err == nil && completion.Usage != nil}
}

func (a *codeAttempt) safePaidResponse(response *http.Response) (*http.Response, error) {
	if a.paid == nil {
		return response, nil
	}
	streaming := strings.HasPrefix(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	encoding := response.Header.Get("Content-Encoding")
	if response.StatusCode < 400 && (!streaming || encoding != "" && encoding != "identity") {
		return response, nil
	}
	if streaming && response.StatusCode < 400 {
		reader, writer := io.Pipe()
		go func() { _ = writer.CloseWithError(a.scrubPaidStream(response.Body, writer)) }()
		safe := *response
		safe.Header = response.Header.Clone()
		safe.Header.Del("Content-Length")
		safe.ContentLength = -1
		safe.Body = reader
		return &safe, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, codewire.MaxBody+1))
	if err != nil || len(raw) > codewire.MaxBody {
		return nil, codemode.Refuse(502, "code_upstream_error_invalid")
	}
	decoded, err := codewire.Decode(raw, response.Header.Get("Content-Encoding"), codewire.MaxBody)
	if err != nil {
		return nil, codemode.Refuse(502, "code_upstream_error_invalid")
	}
	scrubbed, err := a.scrubPaidError(decoded, response.StatusCode >= 400)
	if err != nil {
		return nil, codemode.Refuse(502, "code_upstream_error_invalid")
	}
	safe := *response
	safe.Body = io.NopCloser(bytes.NewReader(raw))
	if !bytes.Equal(scrubbed, decoded) {
		safe.Header = response.Header.Clone()
		safe.Header.Del("Content-Encoding")
		safe.Header.Del("Content-Length")
		safe.ContentLength = int64(len(scrubbed))
		safe.Body = io.NopCloser(bytes.NewReader(scrubbed))
	}
	return &safe, nil
}

func (a *codeAttempt) scrubPaidError(data []byte, failed bool) ([]byte, error) {
	doc, err := oif.ParseJSON(data, oif.Limits{MaxBytes: codewire.MaxBody})
	if err != nil {
		if !failed {
			return data, nil
		}
		return []byte(a.paid.sensitive.Redact(string(data))), nil
	}
	_, hasError := doc.Root().Lookup("error")
	kind, _ := doc.Root().Lookup("type")
	text, _ := kind.Text()
	if !failed && !hasError && text != "error" && text != "response.failed" && text != "response.incomplete" {
		return data, nil
	}
	scrubbed, err := redactNativeFailureDocument(doc, a.paid.sensitive)
	if err != nil {
		return nil, err
	}
	return scrubbed.Bytes(), nil
}

func (a *codeAttempt) scrubPaidStream(reader io.Reader, writer io.Writer) error {
	var pending bytes.Buffer
	decoder := sse.NewRawDecoder(io.TeeReader(reader, &pending), codewire.MaxBody)
	for {
		frame, err := decoder.Next()
		if err != nil {
			if _, writeErr := writer.Write(pending.Bytes()); writeErr != nil {
				return writeErr
			}
			if err == io.EOF {
				return nil
			}
			_, copyErr := io.Copy(writer, reader)
			return copyErr
		}
		pending.Next(len(frame.Bytes))
		data, err := a.scrubPaidError(frame.Data, false)
		if err != nil {
			return err
		}
		raw, err := func() ([]byte, error) {
			if bytes.Equal(data, frame.Data) {
				return frame.Bytes, nil
			}
			return frame.ReplaceData(0, len(frame.Data), data)
		}()
		if err != nil {
			return err
		}
		if _, err := writer.Write(raw); err != nil {
			return err
		}
	}
}
