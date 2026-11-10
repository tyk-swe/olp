package providerinvoke

import (
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/protocols/wire"
)

var ErrResponseTooLarge = errors.New("upstream response exceeds byte limit")

type ResponseReader struct {
	config    connectors.Config
	wire      wire.Format
	streaming bool
	maxEvent  int
	maxBody   int64
}

type ResponseBody struct {
	Body          []byte
	Usage         *wire.Usage
	UsageComplete bool
}

func NewResponseReader(config connectors.Config, format wire.Format, streaming bool, maxEvent int, maxBody int64) ResponseReader {
	return ResponseReader{config: config, wire: format, streaming: streaming, maxEvent: maxEvent, maxBody: maxBody}
}

func (r ResponseReader) Streaming() bool {
	return r.streaming || r.config.ForcesStreaming()
}

func (r ResponseReader) Aggregated() bool {
	return !r.streaming && r.config.ForcesStreaming()
}

func (r ResponseReader) Accept() string {
	if !r.Streaming() {
		return "application/json"
	}
	if r.wire == wire.Bedrock || r.config.EventStream() {
		return "application/vnd.amazon.eventstream"
	}
	return "text/event-stream"
}

func (r ResponseReader) ValidateStream(header http.Header) error {
	mediaType, _, _ := mime.ParseMediaType(header.Get("Content-Type"))
	if mediaType == "text/event-stream" || !r.Aggregated() && (r.wire == wire.Bedrock || r.config.EventStream()) && mediaType == "application/vnd.amazon.eventstream" {
		return nil
	}
	if r.Aggregated() {
		return &wire.ProtocolError{Detail: "the forced stream is not an event stream"}
	}
	return &wire.ProtocolError{Detail: "the upstream response is not an event stream"}
}

func (r ResponseReader) Stream(body io.Reader) io.Reader {
	return r.config.StreamPayload(body, r.maxEvent)
}

func (r ResponseReader) Read(response *http.Response) (ResponseBody, error) {
	if r.Aggregated() {
		if err := r.ValidateStream(response.Header); err != nil {
			return ResponseBody{}, err
		}
		completion, err := openai.Aggregate(r.wire, r.Stream(response.Body), r.maxEvent, int(r.maxBody))
		if completion == nil {
			return ResponseBody{}, err
		}
		return ResponseBody{Body: completion.Body, Usage: completion.Usage, UsageComplete: completion.UsageComplete}, err
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, r.maxBody+1))
	if err != nil {
		return ResponseBody{}, err
	}
	if int64(len(body)) > r.maxBody {
		return ResponseBody{}, ErrResponseTooLarge
	}
	return ResponseBody{Body: r.config.UnwrapResponse(body)}, nil
}
