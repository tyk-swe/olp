package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) BedrockCall(ctx context.Context, x *Call, p *Pin, endpoint string, body []byte, stream bool) (*PinnedResponse, *AttemptFailure) {
	exchange := s.beginPinned(ctx, x, p, http.MethodPost)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, exchange.fail(ClassConnect, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, x, req, p.Provider.Connector(), p.Slot, body, &p.Provider, p.Model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(ClassCancelled, nil)
		}
		return nil, exchange.fail(ClassCredential, nil)
	}
	return s.doPinnedRequest(ctx, p, exchange, req, bedrockErrorBody)
}

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

func (s *Server) ValidateBedrockResult(exchange *PinnedExchange, p *Pin, upstreamOp string, body []byte) *fault.Error {
	model := p.Provider.Connector().Model(p.Model)
	switch upstreamOp {
	case "converse":
		completion, err := protocols.Decode(wire.Bedrock, wire.Bedrock, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Converse response.")
		}
		exchange.fact().ObserveUsage(completion.Usage, completion.Usage != nil)
		return nil
	case "invoke":
		return s.validateBedrockInvoke(exchange, model, body)
	}
	return nil
}

func (s *Server) validateBedrockInvoke(exchange *PinnedExchange, model string, body []byte) *fault.Error {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Claude response.")
		}
		exchange.fact().ObserveUsage(completion.Usage, completion.Usage != nil)
		return nil
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		completion, err := protocols.Decode(wire.BedrockEmbeddings, wire.BedrockEmbeddings, body, model, "")
		if err != nil {
			return fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan embeddings response.")
		}
		exchange.fact().ObserveUsage(completion.Usage, completion.Usage != nil)
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
		return (&AttemptFailure{class: ClassCancelled}).Fault(), ClassCancelled
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return (&AttemptFailure{class: ClassTimeout}).Fault(), ClassTimeout
	}
	return fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream was malformed or truncated."), ClassProtocol
}

func (s *Server) RelayBedrockStream(ctx context.Context, w http.ResponseWriter, x *Call, resp *PinnedResponse) {
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
	limit := int(s.Config.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	var observedUsage *wire.Usage
	var failure *fault.Error
	class := ClassProtocol
	terminal, authoritativeUsage, initialUsage, finalUsage := false, false, false, false
	for {
		message, err := protocols.ReadBedrockEvent(resp.Body, limit)
		if errors.Is(err, io.EOF) {
			if !terminal {
				failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream ended without a terminal event.")
			}
			break
		}
		if err != nil {
			failure, class = bedrockReadFailure(ctx)
			if class == ClassProtocol {
				resp.Exchange.InvalidNative()
			}
			break
		}
		observation, err := protocols.ObserveBedrockEvent(&message, limit)
		if err != nil {
			resp.Exchange.InvalidNative()
			failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream contained an invalid event.")
			break
		}
		if observed := observation.Usage; observed.Value != nil {
			merged, err := observed.Merge(observedUsage)
			if err != nil {
				resp.Exchange.InvalidNative()
				failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native stream contained invalid usage counts.")
				break
			}
			observedUsage = merged
			authoritativeUsage = authoritativeUsage || observed.Authoritative
			initialUsage = initialUsage || observation.Kind == "message_start" && observed.InputPresent
			finalUsage = finalUsage || observation.Kind == "message_delta" && observed.OutputPresent
		}
		terminal = terminal || observation.Terminal
		if observation.Failed {
			resp.Exchange.Complete(ClassUpstreamServer)
			if err := x.redactBedrockFailure(&message, observation); err != nil {
				failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The native error could not be safely delivered.")
				break
			}
			failure = fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider terminated the native stream with an error.")
			class = ClassUpstreamServer
		}
		// Observe completion and usage before a downstream write can fail.
		if observation.Terminal && !observation.Failed {
			resp.Exchange.Complete(ClassSuccess)
		}
		_ = rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
		if err := encoder.Encode(w, message); err != nil {
			failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
			class = ClassCancelled
			break
		}
		if err := rc.Flush(); err != nil {
			resp.Exchange.Delivery(usage.ClientPartial)
			failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
			class = ClassCancelled
			break
		}
		x.Delivered(s.Now())
		if terminal {
			resp.Exchange.Delivery(usage.ClientTerminal)
		} else {
			resp.Exchange.Delivery(usage.ClientPartial)
		}
		if observation.Failed {
			break
		}
	}
	completeUsage := terminal && (authoritativeUsage || initialUsage && finalUsage)
	fact := resp.Exchange.fact()
	fact.ObserveUsage(observedUsage, completeUsage)
	resp.Exchange.recordEvidence()
	if failure != nil {
		x.Failure = failure
		resp.Exchange.Interrupt(class, class == ClassCancelled)
	}
	s.Finish(x, &Outcome{Committed: true, Err: failure, Cancelled: class == ClassCancelled}, http.StatusOK)
}
