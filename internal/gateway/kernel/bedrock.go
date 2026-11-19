package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/openai"
	"github.com/tyk-swe/olp/internal/domain/routeidentity"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/quota"
	"github.com/tyk-swe/olp/internal/usage"
)

func bedrockQualified(p *routing.Provider, model, operation, mode string) bool {
	return p.Kind == "bedrock" &&
		p.Connector().Supports(operation, "bedrock", mode) &&
		p.Supports(model, operation, "bedrock", mode)
}

func (s *Server) bedrockConverse(w http.ResponseWriter, r *http.Request) {
	if s.preparedBedrockConverse(w, r) {
		return
	}
	s.bedrockServe(w, r, wire.Bedrock, "generation", "unary", "converse")
}

func (s *Server) bedrockConverseStream(w http.ResponseWriter, r *http.Request) {
	if s.preparedBedrockConverse(w, r) {
		return
	}
	s.bedrockServe(w, r, wire.Bedrock, "generation", "streaming", "converse-stream")
}

// Strict and cache-enabled Converse routes use the shared prepared invocation
// and Attempt owner. Its native codec retains AWS event-stream framing.
func (s *Server) preparedBedrockConverse(w http.ResponseWriter, r *http.Request) bool {
	release := s.Deps.Runtime.Release()
	if release == nil {
		return false
	}
	route, ok := release.Snapshot().FindRoute(r.PathValue("model"))
	if !ok || !route.Fidelity.Strict() && route.Cache == nil && route.PromptCache == nil {
		return false
	}
	r = r.Clone(r.Context())
	if token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key")); token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	s.inference(wire.Bedrock)(w, r)
	return true
}

func (s *Server) bedrockInvoke(w http.ResponseWriter, r *http.Request) {
	s.bedrockServe(w, r, wire.BedrockInvoke, "bedrock_invoke", "unary", "invoke")
}

func (s *Server) bedrockInvokeStream(w http.ResponseWriter, r *http.Request) {
	s.bedrockServe(w, r, wire.BedrockInvoke, "bedrock_invoke", "streaming", "invoke-with-response-stream")
}

func (s *Server) bedrockServe(w http.ResponseWriter, r *http.Request, family wire.Format, operation, mode, upstreamOp string) {
	x := &Call{request: s.begin(w, r), family: family, actor: "api_key"}
	x.mode = mode
	authority, ok := s.enter(w, r, x, s.Gate.BedrockAuthenticate)
	if !ok {
		return
	}
	defer s.Gate.Release(r.Context())
	fail := func(e *fault.Error) { s.Refuse(x, w, e) }
	slug := r.PathValue("model")
	if !routeidentity.ValidSlug(slug) {
		fail(fault.InvalidRequest("invalid_value", "model must name a published route slug.", nil))
		return
	}
	snapshot := x.request.release.Snapshot()
	route, ok := snapshot.FindRoute(slug)
	if !ok {
		fail(fault.ModelNotFound(slug))
		return
	}
	x.Route = &route
	if x.Strict() {
		// A release may have changed between the dispatch wrapper and begin.
		// Never let that race serve strict work through the transformed resource path.
		fail(fault.InvalidRequest("target_capability", "This interaction requires the compiled generation runner; retry against the current route.", nil))
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.Now()) {
		fail(fault.PermissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if !slices.Contains(route.Operations, operation) {
		fail(fault.InvalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow this operation.", nil))
		return
	}
	if e := x.GuardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	body, e := s.Gate.ReadBody(r)
	if e != nil {
		fail(e)
		return
	}
	// Bedrock inference is metered like any other generation: the request
	// size bounds the reservation and the reported usage settles it.
	sized := int64(len(body)) / 4
	x.estimate, x.sizedInput = max(resourceEstimate, sized), &sized
	defer s.settleCaps(r.Context(), x)
	p, e := s.SelectPinSurface(r.Context(), x, &route, operation, "bedrock", mode, func(provider *routing.Provider, model string) bool {
		return bedrockQualified(provider, model, operation, mode)
	})
	if e != nil {
		fail(e)
		return
	}
	defer func() {
		actual := x.settledTokens()
		s.settlePinnedHold(r.Context(), p.Hold)
		s.recordCost(x)
		quota.SettleKey(r.Context(), x.lease, x.dispatched, actual, s.Log)
	}()
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if e := s.reserveState(r.Context(), x, authority, overall); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.StateDeadline(r.Context(), &route)
	defer cancel()
	if e := s.ReserveFallbackCost(ctx, x); e != nil {
		fail(e)
		return
	}
	endpoint := strings.TrimRight(p.Provider.Endpoint, "/") + "/model/" + url.PathEscape(p.Provider.Connector().Model(p.Model)) + "/" + upstreamOp
	if _, err := s.Egress.ValidateEndpoint(endpoint); err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}
	resp, failure := s.bedrockCall(ctx, x, p, endpoint, body, mode == "streaming")
	if failure != nil {
		fail(UpstreamError(failure))
		return
	}
	defer resp.Body.Close()

	if mode == "streaming" {
		s.relayBedrockStream(ctx, w, x, p, resp)
		return
	}
	result, err := resp.ReadBody(s.Config.MaxResponseBytes)
	if err != nil {
		fail(fault.ServerError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	if e := s.validateBedrockResult(resp.Exchange, p, upstreamOp, result); e != nil {
		fail(e)
		return
	}
	resp.Exchange.Complete(ClassSuccess)
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	x.ResponseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(fault.ResponseWriteTimeout))
	n, writeErr := w.Write(result)
	if writeErr == nil && n == len(result) {
		writeErr = rc.Flush()
	}
	if writeErr != nil || n != len(result) {
		x.Failure = fault.ServerError(http.StatusBadGateway, "client_delivery_failed", "The native result could not be delivered.")
		resp.Exchange.Delivery(DeliveryState(n))
		s.Finish(x, &Outcome{Err: x.Failure, Committed: true, Cancelled: true}, 0)
		return
	}
	x.Delivered(s.Now())
	resp.Exchange.Delivery(usage.ClientTerminal)
	s.Finish(x, &Outcome{Committed: true}, http.StatusOK)
}

func (s *Server) bedrockCall(ctx context.Context, x *Call, p *Pin, endpoint string, body []byte, stream bool) (*PinnedResponse, *AttemptFailure) {
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
		return nil, exchange.fail(classCredential, nil)
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

func (s *Server) validateBedrockResult(exchange *PinnedExchange, p *Pin, upstreamOp string, body []byte) *fault.Error {
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

func (s *Server) relayBedrockStream(ctx context.Context, w http.ResponseWriter, x *Call, p *Pin, resp *PinnedResponse) {
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
