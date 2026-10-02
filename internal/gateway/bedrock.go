package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"

	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/openai"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/upstream"
)

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
	if !ok || !route.Fidelity.Strict() {
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
	if e := policySurfaceGate(&route); e != nil {
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
	p, e := s.selectPinSurface(r.Context(), x, &route, operation, "bedrock", mode, func(provider *runtime.Provider, model string) bool {
		return bedrockQualified(provider, model, operation, mode)
	})
	if e != nil {
		fail(e)
		return
	}
	defer func() {
		actual := x.settledTokens()
		s.settlePinHold(r.Context(), x, p.hold, actual)
		settleKey(r.Context(), x.lease, x.dispatched, actual, s.log)
	}()
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
	req.Header.Set("User-Agent", "olp/gateway")
	if err := s.applySlotCredential(ctx, x, req, p.provider.Connector(), p.slot, body); err != nil {
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
		class := upstream.Classifier{}.Classify(upstream.Evidence{Reached: true, Interrupted: ctx.Err(), Err: err}).Class
		return nil, finish(string(class), &attemptFailure{dispatched: true})
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
	f := &attemptFailure{status: resp.StatusCode, upstream: x.redacted(bedrockErrorBody(raw)), dispatched: true}
	f.class = string(upstream.Classifier{}.Classify(upstream.Evidence{Reached: true, Status: resp.StatusCode, Error: f.upstream}).Class)
	if f.class == classRateLimit {
		f.retryAfter = upstream.RetryAfter(resp.Header.Get("Retry-After"), s.now())
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
		if u, authoritative := bedrockStreamUsage(&message); u != nil {
			if authoritative {
				usage = u
			} else {
				usage = mergeBedrockUsage(usage, u)
			}
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

// bedrockStreamUsage reads the token usage one Bedrock eventstream message
// carries. Converse's metadata event and the invocation metrics on an invoke
// stream's final chunk are authoritative totals. The Anthropic message_start
// and message_delta usage inside an invoke chunk is partial: it merges into
// what earlier chunks reported.
func bedrockStreamUsage(message *eventstream.Message) (usage *openai.Usage, authoritative bool) {
	kind := ""
	if v := message.Headers.Get(":event-type"); v != nil {
		kind = v.String()
	}
	switch kind {
	case "metadata":
		var wire struct {
			Usage *struct {
				InputTokens           int64  `json:"inputTokens"`
				OutputTokens          int64  `json:"outputTokens"`
				TotalTokens           *int64 `json:"totalTokens"`
				CacheReadInputTokens  int64  `json:"cacheReadInputTokens"`
				CacheWriteInputTokens int64  `json:"cacheWriteInputTokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(message.Payload, &wire) != nil || wire.Usage == nil {
			return nil, false
		}
		u := wire.Usage
		if u.TotalTokens != nil && *u.TotalTokens < 0 {
			return nil, false
		}
		usage := bedrockTokenUsage(u.InputTokens, u.OutputTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens)
		if usage != nil && u.TotalTokens != nil {
			usage.TotalTokens = max(usage.TotalTokens, *u.TotalTokens)
		}
		return usage, usage != nil
	case "chunk":
		return bedrockChunkUsage(message.Payload)
	}
	return nil, false
}

// bedrockTokenUsage normalizes Bedrock's split counts: inputTokens excludes
// cache reads and writes, while input usage here includes them.
func bedrockTokenUsage(input, output, cacheRead, cacheWrite int64) *openai.Usage {
	if input < 0 || output < 0 || cacheRead < 0 || cacheWrite < 0 {
		return nil
	}
	in := addBounded(addBounded(input, cacheRead), cacheWrite)
	usage := &openai.Usage{InputTokens: in, OutputTokens: output, TotalTokens: addBounded(in, output)}
	if cacheRead > 0 {
		usage.CachedInputTokens = &cacheRead
	}
	if cacheWrite > 0 {
		usage.CacheWriteInputTokens = &cacheWrite
	}
	return usage
}

type bedrockAnthropicUsage struct {
	InputTokens   *int64 `json:"input_tokens"`
	OutputTokens  *int64 `json:"output_tokens"`
	CacheRead     *int64 `json:"cache_read_input_tokens"`
	CacheCreation *int64 `json:"cache_creation_input_tokens"`
}

// bedrockChunkUsage decodes an InvokeModelWithResponseStream chunk, whose
// payload wraps the model's native event as base64 bytes.
func bedrockChunkUsage(payload []byte) (*openai.Usage, bool) {
	var chunk struct {
		Bytes string `json:"bytes"`
	}
	if json.Unmarshal(payload, &chunk) != nil || chunk.Bytes == "" {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(chunk.Bytes)
	if err != nil {
		return nil, false
	}
	var event struct {
		Type    string `json:"type"`
		Message *struct {
			Usage *bedrockAnthropicUsage `json:"usage"`
		} `json:"message"`
		Usage   *bedrockAnthropicUsage `json:"usage"`
		Metrics *struct {
			InputTokenCount           *int64 `json:"inputTokenCount"`
			OutputTokenCount          *int64 `json:"outputTokenCount"`
			CacheReadInputTokenCount  int64  `json:"cacheReadInputTokenCount"`
			CacheWriteInputTokenCount int64  `json:"cacheWriteInputTokenCount"`
		} `json:"amazon-bedrock-invocationMetrics"`
	}
	if json.Unmarshal(decoded, &event) != nil {
		return nil, false
	}
	if m := event.Metrics; m != nil && m.InputTokenCount != nil && m.OutputTokenCount != nil {
		if usage := bedrockTokenUsage(*m.InputTokenCount, *m.OutputTokenCount, m.CacheReadInputTokenCount, m.CacheWriteInputTokenCount); usage != nil {
			return usage, true
		}
	}
	var native *bedrockAnthropicUsage
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			native = event.Message.Usage
		}
	case "message_delta":
		native = event.Usage
	}
	if native == nil {
		return nil, false
	}
	value := func(v *int64) int64 {
		if v == nil {
			return 0
		}
		return *v
	}
	for _, v := range []*int64{native.InputTokens, native.OutputTokens, native.CacheRead, native.CacheCreation} {
		if v != nil && *v < 0 {
			return nil, false
		}
	}
	// A partial event carries only the counts it names; the merge keeps the
	// rest from earlier events.
	usage := &openai.Usage{OutputTokens: value(native.OutputTokens)}
	if native.InputTokens != nil {
		usage.InputTokens = addBounded(addBounded(*native.InputTokens, value(native.CacheRead)), value(native.CacheCreation))
	}
	if native.CacheRead != nil {
		read := *native.CacheRead
		usage.CachedInputTokens = &read
	}
	if native.CacheCreation != nil {
		write := *native.CacheCreation
		usage.CacheWriteInputTokens = &write
	}
	return usage, false
}

// mergeBedrockUsage folds a partial Anthropic usage event into the usage seen
// so far: input and cache counts come from message_start, and the cumulative
// output count from the latest message_delta.
func mergeBedrockUsage(prev, next *openai.Usage) *openai.Usage {
	if prev == nil {
		merged := *next
		merged.TotalTokens = addBounded(merged.InputTokens, merged.OutputTokens)
		return &merged
	}
	merged := *prev
	if next.InputTokens != 0 {
		merged.InputTokens = next.InputTokens
	}
	if next.OutputTokens != 0 {
		merged.OutputTokens = next.OutputTokens
	}
	if next.CachedInputTokens != nil {
		merged.CachedInputTokens = next.CachedInputTokens
	}
	if next.CacheWriteInputTokens != nil {
		merged.CacheWriteInputTokens = next.CacheWriteInputTokens
	}
	merged.TotalTokens = addBounded(merged.InputTokens, merged.OutputTokens)
	return &merged
}
