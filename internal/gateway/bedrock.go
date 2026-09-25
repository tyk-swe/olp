package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
)

func (s *Server) bedrockAuthenticate(r *http.Request) (access.Authority, *Error) {
	token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key"))
	if token == "" {
		header := r.Header.Get("Authorization")
		if !strings.Contains(header, "AWS4-HMAC-SHA256") && len(header) >= 7 && strings.EqualFold(header[:7], "Bearer ") {
			token = strings.TrimSpace(header[7:])
		}
	}
	if token == "" {
		return access.Authority{}, authenticationError("missing_authorization", "Provide the X-OLP-API-Key header or a bearer API key.")
	}
	authority, err := s.Runtime.Authenticate(token)
	switch {
	case errors.Is(err, runtime.ErrStaleAuthority):
		return access.Authority{}, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	case err != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "Incorrect API key provided.")
	case authority.RevokedAt != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has been revoked.")
	case authority.ExpiresAt != nil && !authority.ExpiresAt.After(s.now()):
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has expired.")
	case !slices.Contains(authority.Policy.Scopes, "inference"):
		return access.Authority{}, permissionError("permission_denied", "This API key does not have the inference scope.")
	}
	return authority, nil
}

func bedrockQualified(p *runtime.Provider, model, operation, mode string) bool {
	return p.Kind == "bedrock" &&
		p.Connector().Supports(operation, "bedrock", mode) &&
		p.Supports(model, operation, "bedrock", mode)
}

func (s *Server) bedrockConverse(w http.ResponseWriter, r *http.Request) {
	if s.strictBedrockConverse(w, r) {
		return
	}
	s.bedrockServe(w, r, openai.FamilyBedrock, "generation", "unary", "converse")
}

func (s *Server) bedrockConverseStream(w http.ResponseWriter, r *http.Request) {
	if s.strictBedrockConverse(w, r) {
		return
	}
	s.bedrockServe(w, r, openai.FamilyBedrock, "generation", "streaming", "converse-stream")
}

// Strict Converse uses the same prepared invocation and Attempt owner as the
// other generation dialects. AWS wire framing is retained by its native codec.
func (s *Server) strictBedrockConverse(w http.ResponseWriter, r *http.Request) bool {
	release := s.Runtime.Release()
	if release == nil {
		return false
	}
	route, ok := release.Snapshot.Routes[r.PathValue("model")]
	if !ok || runtime.FidelityMode(route.Fidelity) != runtime.FidelityStrict {
		return false
	}
	r = r.Clone(r.Context())
	if token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key")); token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	s.inference(openai.FamilyBedrock)(w, r)
	return true
}

func (s *Server) bedrockInvoke(w http.ResponseWriter, r *http.Request) {
	s.bedrockServe(w, r, openai.FamilyBedrockInvoke, "bedrock_invoke", "unary", "invoke")
}

func (s *Server) bedrockInvokeStream(w http.ResponseWriter, r *http.Request) {
	s.bedrockServe(w, r, openai.FamilyBedrockInvoke, "bedrock_invoke", "streaming", "invoke-with-response-stream")
}

func (s *Server) bedrockServe(w http.ResponseWriter, r *http.Request, family openai.Family, operation, mode, upstreamOp string) {
	x := &execution{request: s.begin(w, r), family: family, actor: "api_key"}
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
	x.keyID, x.affinity = authority.ID, []byte(authority.ID)
	x.budgetGroupID = authority.BudgetGroupID
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
		// Never let that race serve strict work through the legacy resource path.
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
	if e := policySurfaceGate(&route); e != nil {
		fail(e)
		return
	}
	body, e := s.readBody(r)
	if e != nil {
		fail(e)
		return
	}
	p, e := s.selectPinSurface(r.Context(), x, &route, operation, "bedrock", mode, func(provider *runtime.Provider, model string) bool {
		return bedrockQualified(provider, model, operation, mode)
	})
	if e != nil {
		fail(e)
		return
	}
	defer s.resourceSettle(r.Context(), x, p)
	overall := time.Duration(route.OverallTimeout) * time.Millisecond
	if e := s.reserveState(r.Context(), x, authority, overall); e != nil {
		fail(e)
		return
	}
	ctx, cancel := s.stateDeadline(r.Context(), &route)
	defer cancel()
	endpoint := strings.TrimRight(p.provider.Endpoint, "/") + "/model/" + url.PathEscape(p.provider.Connector().Model(p.model)) + "/" + upstreamOp
	if _, err := s.egress.ValidateEndpoint(endpoint); err != nil {
		fail(serverError(http.StatusBadGateway, "upstream_error", "The provider address is not permitted by egress policy."))
		return
	}
	resp, failure := s.bedrockCall(ctx, x, p, endpoint, body, mode == "streaming")
	if failure != nil {
		x.dispatched = true
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
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
	x.delivered(s.now())
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func (s *Server) bedrockCall(ctx context.Context, x *execution, p *pin, endpoint string, body []byte, stream bool) (*http.Response, *attemptFailure) {
	fact := s.newFact(x, p.attempt, p.slot, len(x.facts)+1)
	fact.Mode = x.mode
	finish := func(class string, f *attemptFailure) *attemptFailure {
		if f == nil {
			f = &attemptFailure{}
		}
		f.class = class
		fact.Class = class
		fact.Committed = f.committed
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, finish(classConnect, nil)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if stream {
		req.Header.Set("Accept", "application/vnd.amazon.eventstream")
	}
	req.Header.Set("User-Agent", "olp-go/gateway")
	var secret []byte
	if p.slot.CredentialID != nil {
		secret, _ = x.request.release.Credential(*p.slot.CredentialID)
	}
	if _, err := s.auth.Apply(ctx, req, p.provider.Connector(), secret, body); err != nil {
		if ctx.Err() != nil {
			return nil, finish(classCancelled, nil)
		}
		return nil, finish(classCredential, nil)
	}
	client, err := s.providerClient(ctx, x.request.release, &p.provider, p.slot)
	if err != nil {
		return nil, finish(classCredential, nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		class := classConnect
		if ctx.Err() != nil {
			class = classCancelled
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				class = classTimeout
			}
		}
		return nil, finish(class, &attemptFailure{dispatched: true})
	}
	received := s.now().Sub(fact.StartedAt)
	fact.FirstByte = &received
	fact.Status = resp.StatusCode
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		fact.Class = "success"
		fact.Committed = true
		fact.Duration = s.now().Sub(fact.StartedAt)
		fact.recordEvidence(true)
		x.facts = append(x.facts, fact)
		return resp, nil
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
	resp.Body.Close()
	f := &attemptFailure{status: resp.StatusCode, upstream: bedrockErrorBody(raw), dispatched: true}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		f.class = classCredential
	case resp.StatusCode == http.StatusTooManyRequests:
		f.retryAfter = retryAfter(resp.Header.Get("Retry-After"), s.now())
		f.class = classRateLimit
	case resp.StatusCode >= 500:
		f.class = classUpstreamServer
	default:
		f.class = classUpstreamClient
	}
	return nil, finish(f.class, f)
}

func bedrockErrorBody(body []byte) *openai.UpstreamError {
	var wire struct {
		Message string `json:"message"`
		Type    string `json:"__type"`
	}
	if json.Unmarshal(body, &wire) != nil || wire.Message == "" {
		return openai.ParseErrorBody(body)
	}
	code := wire.Type
	if i := strings.LastIndex(code, "#"); i >= 0 {
		code = code[i+1:]
	}
	return &openai.UpstreamError{Type: "upstream_error", Code: code, Message: wire.Message}
}

func (s *Server) validateBedrockResult(x *execution, p *pin, upstreamOp string, body []byte, w http.ResponseWriter) *Error {
	model := p.provider.Connector().Model(p.model)
	switch upstreamOp {
	case "converse":
		completion, err := protocols.Decode(openai.FamilyBedrock, openai.FamilyBedrock, body, model, "")
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

func (s *Server) validateBedrockInvoke(x *execution, model string, body []byte) *Error {
	switch {
	case strings.Contains(model, "anthropic.claude"):
		completion, err := protocols.Decode(openai.FamilyAnthropic, openai.FamilyAnthropic, body, model, "")
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Claude response.")
		}
		s.recordBedrockUsage(x, completion.Usage)
		return nil
	case strings.HasPrefix(model, "amazon.titan-embed-text-"):
		completion, err := protocols.Decode(openai.FamilyBedrockEmbeddings, openai.FamilyBedrockEmbeddings, body, model, "")
		if err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan embeddings response.")
		}
		s.recordBedrockUsage(x, completion.Usage)
		return nil
	case strings.HasPrefix(model, "amazon.titan-image-generator-"):
		if err := validateTitanImage(body); err != nil {
			return serverError(http.StatusBadGateway, "upstream_error", "The provider returned a malformed Titan image response.")
		}
		return nil
	}
	return serverError(http.StatusUnprocessableEntity, "unsupported_invoke_model",
		"The Invoke operation supports only qualified Anthropic Claude, Titan embeddings, and Titan image models; `"+model+"` is not qualified.")
}

func validateTitanImage(body []byte) error {
	var wire struct {
		Images []string `json:"images"`
		Error  *string  `json:"error"`
	}
	if json.Unmarshal(body, &wire) != nil {
		return errors.New("invalid Titan image response")
	}
	if wire.Error != nil && *wire.Error != "" {
		return errors.New("provider image error")
	}
	if len(wire.Images) == 0 {
		return errors.New("missing images")
	}
	for _, image := range wire.Images {
		if image == "" {
			return errors.New("empty image payload")
		}
	}
	return nil
}

func (s *Server) recordBedrockUsage(x *execution, usage *openai.Usage) {
	if usage == nil || len(x.facts) == 0 {
		return
	}
	fact := &x.facts[len(x.facts)-1]
	fact.Usage = usage
	fact.UsageObserved, fact.UsageComplete, fact.BillingUncertain = true, true, false
}

func (s *Server) relayBedrockStream(ctx context.Context, w http.ResponseWriter, x *execution, p *pin, resp *http.Response) {
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/vnd.amazon.eventstream"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	x.delivered(s.now())
	rc := http.NewResponseController(w)
	encoder := eventstream.NewEncoder()
	limit := int(s.cfg.MaxEventBytes)
	if limit <= 0 {
		limit = 1 << 20
	}
	var usage *openai.Usage
	truncated := false
	for {
		message, err := protocols.ReadBedrockEvent(resp.Body, limit)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			truncated = true
			break
		}
		if u := bedrockStreamUsage(&message); u != nil {
			usage = u
		}
		if err := encoder.Encode(w, message); err != nil {
			truncated = true
			break
		}
		if err := rc.Flush(); err != nil {
			truncated = true
			break
		}
	}
	if usage != nil {
		s.recordBedrockUsage(x, usage)
	}
	if len(x.facts) > 0 && (truncated || usage == nil) {
		fact := &x.facts[len(x.facts)-1]
		if truncated {
			fact.Class = classUpstreamServer
		}
		fact.UsageComplete = false
		fact.BillingUncertain = true
	}
	s.finish(x, &outcome{committed: true}, http.StatusOK)
}

func bedrockStreamUsage(message *eventstream.Message) *openai.Usage {
	kind := ""
	if v := message.Headers.Get(":event-type"); v != nil {
		kind = v.String()
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(message.Payload, &payload) != nil {
		return nil
	}
	switch kind {
	case "metadata":
		var wire struct {
			Usage *struct {
				InputTokens           int64 `json:"inputTokens"`
				OutputTokens          int64 `json:"outputTokens"`
				CacheReadInputTokens  int64 `json:"cacheReadInputTokens"`
				CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(message.Payload, &wire) != nil || wire.Usage == nil {
			return nil
		}
		// Converse inputTokens counts only uncached input; cache reads and
		// writes are reported beside it, while canonical input includes both.
		input, read, write := wire.Usage.InputTokens, wire.Usage.CacheReadInputTokens, wire.Usage.CacheWriteInputTokens
		if input < 0 || wire.Usage.OutputTokens < 0 || read < 0 || write < 0 ||
			read > math.MaxInt64-input || write > math.MaxInt64-input-read {
			return nil
		}
		usage := &openai.Usage{InputTokens: input + read + write, OutputTokens: wire.Usage.OutputTokens}
		if read > 0 {
			usage.CachedInputTokens = &read
		}
		if write > 0 {
			usage.CacheWriteInputTokens = &write
		}
		return usage
	case "message_delta", "message_stop":
		var wire struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(message.Payload, &wire) != nil || wire.Usage == nil ||
			wire.Usage.InputTokens < 0 || wire.Usage.OutputTokens < 0 {
			return nil
		}
		return &openai.Usage{InputTokens: wire.Usage.InputTokens, OutputTokens: wire.Usage.OutputTokens}
	case "message_start":
		var wire struct {
			Message *struct {
				Usage *struct {
					InputTokens  int64 `json:"input_tokens"`
					OutputTokens int64 `json:"output_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if json.Unmarshal(message.Payload, &wire) != nil || wire.Message == nil || wire.Message.Usage == nil ||
			wire.Message.Usage.InputTokens < 0 || wire.Message.Usage.OutputTokens < 0 {
			return nil
		}
		return &openai.Usage{InputTokens: wire.Message.Usage.InputTokens, OutputTokens: wire.Message.Usage.OutputTokens}
	}
	return nil
}
