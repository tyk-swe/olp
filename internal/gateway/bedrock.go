package gateway

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

	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
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
	release := s.Runtime.Release()
	if release == nil {
		return false
	}
	route, ok := release.Snapshot.Routes[r.PathValue("model")]
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
	x := &call{request: s.begin(w, r), family: family, actor: "api_key"}
	x.mode = mode
	fail := func(e *Error) {
		x.failure = e
		s.finish(x, nil, e.Status)
		writeSurfaceError(w, e, "bedrock")
	}
	if !s.admit(r.Context()) {
		fail(overloaded)
		return
	}
	defer s.release(r.Context())
	authority, e := s.bedrockAuthenticate(r)
	if e != nil {
		fail(e)
		return
	}
	s.bindAuthority(x, authority)
	if x.attribution, e = s.parseAttribution(r, authority); e != nil {
		fail(e)
		return
	}
	slug := r.PathValue("model")
	if !openai.RouteSlug.MatchString(slug) {
		fail(invalidRequest("invalid_value", "model must name a published route slug.", nil))
		return
	}
	snapshot := x.request.release.Snapshot
	route, ok := snapshot.Routes[slug]
	if !ok {
		fail(modelNotFound(slug))
		return
	}
	x.route = &route
	if x.strict() {
		// A release may have changed between the dispatch wrapper and begin.
		// Never let that race serve strict work through the transformed resource path.
		fail(invalidRequest("target_capability", "This interaction requires the compiled generation runner; retry against the current route.", nil))
		return
	}
	if !authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		fail(permissionError("route_forbidden", "This API key is not allowed to use the model `"+route.Slug+"`."))
		return
	}
	if !slices.Contains(route.Operations, operation) {
		fail(invalidRequest("invalid_request", "The model `"+route.Slug+"` does not allow this operation.", nil))
		return
	}
	if e := x.guardrailSurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	body, e := s.readBody(r)
	if e != nil {
		fail(e)
		return
	}
	// Bedrock inference is metered like any other generation: the request
	// size bounds the reservation and the reported usage settles it.
	sized := int64(len(body)) / 4
	x.estimate, x.sizedInput = max(resourceEstimate, sized), &sized
	defer s.settleCaps(r.Context(), x)
	p, e := s.selectPinSurface(r.Context(), x, &route, operation, "bedrock", mode, func(provider *routing.Provider, model string) bool {
		return bedrockQualified(provider, model, operation, mode)
	})
	if e != nil {
		fail(e)
		return
	}
	defer func() {
		actual := x.settledTokens()
		s.settlePinHold(r.Context(), x, p.hold, actual)
		s.recordCost(x)
		settleKey(r.Context(), x.lease, x.dispatched, actual, s.log)
	}()
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if e := s.reserveState(r.Context(), x, authority, overall); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	if e := s.reserveFallbackCost(ctx, x); e != nil {
		fail(e)
		return
	}
	endpoint := strings.TrimRight(p.provider.Endpoint, "/") + "/model/" + url.PathEscape(p.provider.Connector().Model(p.model)) + "/" + upstreamOp
	if _, err := s.egress.ValidateEndpoint(endpoint); err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}
	resp, failure := s.bedrockCall(ctx, x, p, endpoint, body, mode == "streaming")
	if failure != nil {
		x.dispatched = failure.dispatched
		fail(upstreamError(failure))
		return
	}
	defer resp.Body.Close()
	x.dispatched = true
	if mode == "streaming" {
		s.relayBedrockStream(ctx, w, x, p, resp)
		return
	}
	result, err := readBounded(resp.Body, s.cfg.MaxResponseBytes)
	if err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider response could not be read."))
		return
	}
	if e := s.validateBedrockResult(x, p, upstreamOp, result, w); e != nil {
		fail(e)
		return
	}
	x.completePinned()
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	x.responseHeaders(w.Header(), false)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
	n, writeErr := w.Write(result)
	if writeErr == nil && n == len(result) {
		writeErr = rc.Flush()
	}
	if writeErr != nil || n != len(result) {
		x.failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The native result could not be delivered.")
		x.facts[len(x.facts)-1].Class = classCancelled
		s.finish(x, &outcome{err: x.failure, committed: true, cancelled: true}, 0)
		return
	}
	x.delivered(s.now())
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func (s *Server) bedrockCall(ctx context.Context, x *call, p *pin, endpoint string, body []byte, stream bool) (*http.Response, *attemptFailure) {
	exchange := s.beginPinned(ctx, x, p, http.MethodPost)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, exchange.fail(s, x, classConnect, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, x, req, p.provider.Connector(), p.slot, body, &p.provider, p.model); err != nil {
		if ctx.Err() != nil {
			return nil, exchange.fail(s, x, classCancelled, nil)
		}
		return nil, exchange.fail(s, x, classCredential, nil)
	}
	return s.doPinnedRequest(ctx, x, p, exchange, req, bedrockErrorBody)
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

func (s *Server) validateBedrockResult(x *call, p *pin, upstreamOp string, body []byte, w http.ResponseWriter) *Error {
	model := p.provider.Connector().Model(p.model)
	switch upstreamOp {
	case "converse":
		completion, err := protocols.Decode(wire.Bedrock, wire.Bedrock, body, model, "")
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Converse response.")
		}
		s.recordBedrockUsage(x, completion.Usage)
		return nil
	case "invoke":
		return s.validateBedrockInvoke(x, model, body)
	}
	return nil
}

func (s *Server) validateBedrockInvoke(x *call, model string, body []byte) *Error {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		completion, err := protocols.Decode(wire.Anthropic, wire.Anthropic, body, model, "")
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Claude response.")
		}
		s.recordBedrockUsage(x, completion.Usage)
		return nil
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		completion, err := protocols.Decode(wire.BedrockEmbeddings, wire.BedrockEmbeddings, body, model, "")
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan embeddings response.")
		}
		s.recordBedrockUsage(x, completion.Usage)
		return nil
	case strings.HasPrefix(model, "amazon.titan-image-generator-"):
		if err := validateTitanImage(body); err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan image response.")
		}
		// Titan image responses report no token usage, so the invoke keeps
		// its request-sized reservation instead of settling at zero.
		if len(x.facts) > 0 {
			fact := &x.facts[len(x.facts)-1]
			fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = false, false, true
		}
		return nil
	}
	return serverError(http.StatusUnprocessableEntity, "unsupported_invoke_model",
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

func (s *Server) recordBedrockUsage(x *call, usage *wire.Usage) {
	if usage == nil || len(x.facts) == 0 {
		return
	}
	fact := &x.facts[len(x.facts)-1]
	fact.observeUsage(usage, true)
	fact.recordEvidence(false)
}

func (s *Server) relayBedrockStream(ctx context.Context, w http.ResponseWriter, x *call, p *pin, resp *http.Response) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.amazon.eventstream"
	}
	w.Header().Set("Content-Type", contentType)
	x.responseHeaders(w.Header(), true)
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	encoder := eventstream.NewEncoder()
	limit := int(s.cfg.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	var usage *wire.Usage
	var failure *Error
	class := classProtocol
	terminal, authoritativeUsage, initialUsage, finalUsage := false, false, false, false
	for {
		message, err := protocols.ReadBedrockEvent(resp.Body, limit)
		if errors.Is(err, io.EOF) {
			if !terminal {
				failure = serverError(http.StatusBadGateway, "upstream_error", "The native stream ended without a terminal event.")
			}
			break
		}
		if err != nil {
			failure = serverError(http.StatusBadGateway, "upstream_error", "The native stream was malformed or truncated.")
			break
		}
		observation, err := protocols.ObserveBedrockEvent(&message, limit)
		if err != nil {
			failure = serverError(http.StatusBadGateway, "upstream_error", "The native stream contained an invalid event.")
			break
		}
		if observed := observation.Usage; observed.Value != nil {
			merged, err := observed.Merge(usage)
			if err != nil {
				failure = serverError(http.StatusBadGateway, "upstream_error", "The native stream contained invalid usage counts.")
				break
			}
			usage = merged
			authoritativeUsage = authoritativeUsage || observed.Authoritative
			initialUsage = initialUsage || observation.Kind == "message_start" && observed.InputPresent
			finalUsage = finalUsage || observation.Kind == "message_delta" && observed.OutputPresent
		}
		terminal = terminal || observation.Terminal
		if observation.Failed {
			if err := x.redactBedrockFailure(&message, observation); err != nil {
				failure = serverError(http.StatusBadGateway, "upstream_error", "The native error could not be safely delivered.")
				break
			}
			failure = serverError(http.StatusBadGateway, "upstream_error", "The provider terminated the native stream with an error.")
			class = classUpstreamServer
		}
		// Observe completion and usage before a downstream write can fail.
		if terminal && !observation.Failed {
			x.completePinned()
		}
		_ = rc.SetWriteDeadline(time.Now().Add(responseWriteTimeout))
		if err := encoder.Encode(w, message); err != nil {
			failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
			class = classCancelled
			break
		}
		if err := rc.Flush(); err != nil {
			failure = serverError(http.StatusBadGateway, "client_delivery_failed", "The native stream could not be delivered.")
			class = classCancelled
			break
		}
		x.delivered(s.now())
		if observation.Failed {
			break
		}
	}
	completeUsage := terminal && (authoritativeUsage || initialUsage && finalUsage)
	fact := &x.facts[len(x.facts)-1]
	fact.observeUsage(usage, completeUsage)
	fact.recordEvidence(!completeUsage)
	if failure != nil {
		x.failure = failure
		x.facts[len(x.facts)-1].Class = class
	}
	s.finish(x, &outcome{committed: true, err: failure, cancelled: class == classCancelled}, http.StatusOK)
}
