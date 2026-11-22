package bedrock

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/usage"
)

func bedrockErrorBody(body []byte) *wire.UpstreamError {
	var encodedWire struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
	}
	if json.Unmarshal(body, &encodedWire) != nil || encodedWire.Message == "" {
		return openai.ParseErrorBody(body)
	}
	code := encodedWire.Type
	if i := strings.LastIndex(code, "#"); i >= 0 {
		code = code[i+1:]
	}
	return &wire.UpstreamError{Type: "upstream_error", Code: code, Message: encodedWire.Message}
}

func validateResult(exchange *kernel.PinnedExchange, p *kernel.Pin, upstreamOp string, body []byte) *fault.Error {
	model := p.Provider.Connector().Model(p.Model)
	switch upstreamOp {
	case "converse":
		completion, err := protocols.Decode(wire.Bedrock, wire.Bedrock, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Converse response.")
		}
		exchange.ObserveUsage(completion.Usage, completion.Usage != nil)
		return nil
	case "invoke":
		return validateInvoke(exchange, model, body)
	}
	return nil
}

func validateInvoke(exchange *kernel.PinnedExchange, model string, body []byte) *fault.Error {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Claude response.")
		}
		exchange.ObserveUsage(completion.Usage, completion.Usage != nil)
		return nil
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		completion, err := protocols.Decode(wire.BedrockEmbeddings, wire.BedrockEmbeddings, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan embeddings response.")
		}
		exchange.ObserveUsage(completion.Usage, completion.Usage != nil)
		return nil
	case strings.HasPrefix(model, "amazon.titan-image-generator-"):
		if err := validateTitanImage(body); err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan image response.")
		}
		return nil
	}
	return fault.ServerError(http.StatusUnprocessableEntity, "unsupported_invoke_model",
		"The Invoke operation supports only qualified Anthropic Claude, Titan embeddings, and Titan image models; `"+model+"` is not qualified.")
}

func validateTitanImage(body []byte) error {
	var encodedWire struct {
		Images []string `json:"images"`
		Error  *string  `json:"error"`
	}
	if json.Unmarshal(body, &encodedWire) != nil {
		return errors.New("invalid Titan image response")
	}
	if encodedWire.Error != nil && *encodedWire.Error != "" {
		return errors.New("provider image error")
	}
	if len(encodedWire.Images) == 0 {
		return errors.New("missing images")
	}
	for _, image := range encodedWire.Images {
		if image == "" {
			return errors.New("empty image payload")
		}
	}
	return nil
}

func bedrockReadFailure(ctx context.Context) (*fault.Error, string) {
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return kernel.AttemptClassFault(kernel.ClassCancelled), kernel.ClassCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return kernel.AttemptClassFault(kernel.ClassTimeout), kernel.ClassTimeout
	}
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream was malformed or truncated."), kernel.ClassProtocol
}

type bedrockStreamState struct {
	usage              *wire.Usage
	failure            *fault.Error
	class              string
	terminal           bool
	authoritativeUsage bool
	initialUsage       bool
	finalUsage         bool
}

func (st *bedrockStreamState) observeUsage(observation protocols.BedrockObservation) bool {
	observed := observation.Usage
	if observed.Value == nil {
		return true
	}
	merged, err := observed.Merge(st.usage)
	if err != nil {
		return false
	}
	st.usage = merged
	st.authoritativeUsage = st.authoritativeUsage || observed.Authoritative
	st.initialUsage = st.initialUsage || observation.Kind == "message_start" && observed.InputPresent
	st.finalUsage = st.finalUsage || observation.Kind == "message_delta" && observed.OutputPresent
	return true
}

func (s surface) relayStream(ctx context.Context, w http.ResponseWriter, x *kernel.Call, resp *kernel.PinnedResponse) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.amazon.eventstream"
	}
	w.Header().Set("Content-Type", contentType)
	x.ResponseHeaders(w.Header(), true)
	w.WriteHeader(http.StatusOK)
	resp.Exchange.Delivery(usage.ClientUnobserved)
	rc := http.NewResponseController(w)
	encoder := eventstream.NewEncoder()
	limit := int(s.kernel.Config.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	st := bedrockStreamState{class: kernel.ClassProtocol}
	for s.relayStreamFrame(ctx, w, rc, encoder, x, resp, limit, &st) {
	}
	completeUsage := st.terminal && (st.authoritativeUsage || st.initialUsage && st.finalUsage)
	resp.Exchange.ObserveUsage(st.usage, completeUsage)
	resp.Exchange.RecordEvidence()
	if st.failure != nil {
		x.Failure = st.failure
		resp.Exchange.Interrupt(st.class, st.class == kernel.ClassCancelled)
	}
	s.kernel.Finish(x, &kernel.Outcome{Committed: true, Err: st.failure, Cancelled: st.class == kernel.ClassCancelled}, http.StatusOK)
}

func (s surface) relayStreamFrame(ctx context.Context, w http.ResponseWriter, rc *http.ResponseController, encoder *eventstream.Encoder, x *kernel.Call, resp *kernel.PinnedResponse, limit int, st *bedrockStreamState) bool {
	message, err := protocols.ReadBedrockEvent(resp.Body, limit)
	if errors.Is(err, io.EOF) {
		if !st.terminal {
			st.failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream ended without a terminal event.")
		}
		return false
	}
	if err != nil {
		st.failure, st.class = bedrockReadFailure(ctx)
		if st.class == kernel.ClassProtocol {
			resp.Exchange.InvalidNative()
		}
		return false
	}
	observation, err := protocols.ObserveBedrockEvent(&message, limit)
	if err != nil {
		resp.Exchange.InvalidNative()
		st.failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream contained an invalid event.")
		return false
	}
	if !st.observeUsage(observation) {
		resp.Exchange.InvalidNative()
		st.failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream contained invalid usage counts.")
		return false
	}
	st.terminal = st.terminal || observation.Terminal
	if observation.Failed {
		resp.Exchange.Complete(kernel.ClassUpstreamServer)
		if err := redactBedrockFailure(x, &message, observation); err != nil {
			st.failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native error could not be safely delivered.")
			return false
		}
		st.failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider terminated the native stream with an error.")
		st.class = kernel.ClassUpstreamServer
	}
	// Observe completion and usage before a downstream write can fail.
	if observation.Terminal && !observation.Failed {
		resp.Exchange.Complete(kernel.ClassSuccess)
	}
	_ = rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	if err := encoder.Encode(w, message); err != nil {
		st.failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
		st.class = kernel.ClassCancelled
		return false
	}
	if err := rc.Flush(); err != nil {
		resp.Exchange.Delivery(usage.ClientPartial)
		st.failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
		st.class = kernel.ClassCancelled
		return false
	}
	x.Delivered(s.kernel.Now())
	if st.terminal {
		resp.Exchange.Delivery(usage.ClientTerminal)
	} else {
		resp.Exchange.Delivery(usage.ClientPartial)
	}
	return !observation.Failed
}

func redactBedrockFailure(x *kernel.Call, message *eventstream.Message, observation protocols.BedrockObservation) error {
	sensitive := x.Sensitive()
	source := observation.Source
	if observation.Native.Valid() {
		redacted, err := fault.RedactNativeFailureDocument(observation.Native, sensitive)
		if err != nil {
			return err
		}
		if redacted != observation.Native {
			encoded, _ := json.Marshal(base64.StdEncoding.EncodeToString(redacted.Bytes()))
			source, err = oif.Apply(source, []oif.Change{{Pointer: "/bytes", Value: string(encoded), Origin: oif.ExplicitTransform, Reason: "provider error credential redaction"}})
			if err != nil {
				return err
			}
		}
	} else {
		var err error
		source, err = fault.RedactNativeFailureDocument(source, sensitive)
		if err != nil {
			return err
		}
	}
	message.Payload = source.Bytes()
	for index, header := range message.Headers {
		if sensitive.Contains(header.Name) {
			return errors.New("native error has a credential-bearing header name")
		}
		if value, ok := header.Value.(eventstream.StringValue); ok {
			message.Headers[index].Value = eventstream.StringValue(sensitive.Redact(string(value)))
		}
	}
	return nil
}
