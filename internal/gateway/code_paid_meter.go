package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/protocols"
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
