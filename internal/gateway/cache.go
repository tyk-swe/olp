package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/usage"
)

type responseCacheCall struct {
	policy                   cache.Policy
	controls                 cache.Controls
	ticket                   cache.Ticket
	entry                    *cache.Entry
	admitted                 bool // Set only after the replay passes admission.
	embeddingDispatched      bool // An embedding consumed the parent's shared request reservation.
	result                   string
	reason                   string
	age                      int64
	similarity               *float64
	vector                   []float32
	body                     []byte
	frames                   [][]byte
	size                     int
	storedBytes              int
	target                   string
	metadata                 cacheMetadata
	budgetCost, budgetCharge string
	parallelEngine           *guardrails.Engine
	parallelInput            []guardrails.Segment
}
type cacheMetadata struct {
	Usage              *wire.Usage `json:"usage"`
	ProviderID         string      `json:"provider_id"`
	ProviderRevisionID string      `json:"provider_revision_id"`
	UpstreamModel      string      `json:"upstream_model"`
	UsageComplete      bool        `json:"usage_complete"`
}

func (x *call) cacheHit() bool { return x.cache != nil && x.cache.admitted }
func (x *call) cacheHeaders(h http.Header) {
	if x.cache == nil {
		return
	}
	h.Set("X-OLP-Cache", x.cache.result)
	if x.cacheHit() {
		h.Set("Age", strconv.FormatInt(x.cache.age, 10))
	}
}

func (s *Server) initializeCache(x *call) *Error {
	// New endpoint contracts require explicit cache qualification.
	if x.family == wire.Completions || operationregistry.TransformedServing(x.operationName()) {
		return nil
	}
	if x.route.Cache == nil && x.request.cacheDirectives == "" {
		return nil
	}
	c, err := cache.ParseControls(x.request.cacheDirectives)
	if err != nil {
		return invalidRequest("invalid_cache_control", err.Error(), nil)
	}
	x.cache = &responseCacheCall{controls: c, result: "bypass"}
	if x.route.Cache == nil || s.Cache == nil || x.origin != "" || x.authority.Policy.ForbidResponseCache || x.request.callerCredential != nil || x.providerState || x.generation.resources != nil || x.generation.continuation != nil {
		return nil
	}
	p := x.route.Cache.Effective()
	x.cache.policy = p
	if c.NoStore || !slices.Contains(p.Operations, x.operationName()) || x.mode == "streaming" && p.Streaming != "replay" || len(x.attempts) == 0 {
		return nil
	}
	if x.guardrailEngine.HasAction(guardrails.Input, guardrails.Mask) {
		return nil
	}
	x.prepareSource()
	doc := x.source
	if !doc.Valid() {
		return nil
	}
	for _, field := range []string{"previous_response_id", "conversation", "file_id", "file_ids", "batch_id"} {
		if v, ok := doc.Root().Lookup(field); ok && v.Kind() != oif.Null {
			return nil
		}
	}
	if cacheRetentionRequested(doc) {
		return nil
	}
	for _, field := range []string{"n", "candidate_count"} {
		if v, ok := doc.Root().Lookup(field); ok && v.Raw() != "1" {
			return nil
		}
	}
	// Retained Responses defaults to store=true. Require explicit stateless
	// semantics even when the key is permitted to retain provider resources.
	if x.family == wire.Responses && x.authority.Policy.AllowProviderState {
		v, ok := doc.Root().Lookup("store")
		if !ok || v.Raw() != "false" {
			return nil
		}
	}
	if containsCacheReference(doc.Root()) {
		return nil
	}
	x.cache.result = "miss"
	return nil
}

func cacheRetentionRequested(doc oif.Document) bool {
	for _, field := range []string{"store", "background"} {
		if v, ok := doc.Root().Lookup(field); ok && v.Raw() != "false" && v.Kind() != oif.Null {
			return true
		}
	}
	return false
}

func containsCacheReference(v oif.Value) bool {
	switch v.Kind() {
	case oif.Object:
		for _, m := range v.Members() {
			if (m.Name == "candidateCount" || m.Name == "candidate_count") && m.Value.Raw() != "1" {
				return true
			}
			if m.Name == "type" && slices.Contains([]string{`"image"`, `"image_url"`, `"input_image"`, `"audio"`, `"input_audio"`, `"video"`, `"document"`, `"file"`, `"input_file"`, `"item_reference"`}, m.Value.Raw()) {
				return true
			}
			if m.Name == "file_id" || m.Name == "vector_store_ids" || m.Name == "cachedContent" || m.Name == "file_uri" || m.Name == "file_data" || m.Name == "image_url" || m.Name == "input_audio" || m.Name == "inlineData" || m.Name == "fileData" || m.Name == "image" || m.Name == "audio" || m.Name == "video" || m.Name == "document" {
				return true
			}
			if containsCacheReference(m.Value) {
				return true
			}
		}
	case oif.Array:
		for _, e := range v.Elements() {
			if containsCacheReference(e) {
				return true
			}
		}
	}
	return false
}

// admitCacheAware takes only request/concurrency reservations before lookup.
// Misses then reserve the remaining dimensions; both leases settle once.
func (s *Server) admitCacheAware(ctx context.Context, x *call, authority *access.Authority, estimate int64, ttl time.Duration, hold costReservation) *Error {
	if e := s.initializeCache(x); e != nil {
		return e
	}
	if x.cache == nil || x.cache.result == "bypass" {
		var e *Error
		x.lease, e = s.Admission.reserveKeyCosted(ctx, authority, x.clientSurface(), estimate, ttl, hold, x.limitRoute())
		return e
	}
	rates := cacheAuthority(*authority, true, false, false)
	var e *Error
	x.cacheRateLease, e = s.Admission.reserveKeyCosted(ctx, &rates, x.clientSurface(), 0, ttl, costReservation{}, x.limitRoute())
	if e != nil {
		return e
	}
	if e = s.lookupCache(ctx, x); e != nil {
		return e
	}
	tokens, budgets := true, true
	if x.cache.entry != nil {
		tokens = slices.Contains(x.cache.policy.CountHitsAgainst, "tokens")
		budgets = slices.Contains(x.cache.policy.CountHitsAgainst, "budgets")
		estimate = 0
		if tokens {
			if actual := totalTokens(x.cache.metadata.Usage); actual != nil {
				estimate = *actual
			}
			// Final zero-token usage needs no token reservation. The limiter
			// requires a positive allowance and cannot reconcile it downward.
			tokens = estimate > 0
		}
		hold = costReservation{amount: x.cache.budgetCost, charge: x.cache.budgetCharge, requestID: x.request.accountingID()}
	}
	remainder := cacheAuthority(*authority, false, tokens, budgets)
	x.lease, e = s.Admission.reserveKeyCosted(ctx, &remainder, x.clientSurface(), estimate, ttl, hold, x.limitRoute())
	if e == nil && x.cache.entry != nil {
		x.cache.admitted = true
		x.cache.result = "hit"
		if x.cache.similarity != nil {
			x.cache.result = "semantic-hit"
		}
	}
	return e
}

func cacheAuthority(a access.Authority, rates, tokens, budgets bool) access.Authority {
	filter := func(p access.AdmissionLimits) access.AdmissionLimits {
		if !rates {
			p.RequestsPerMinute = nil
			p.MaxConcurrency = nil
		}
		if !tokens {
			p.TokensPerMinute = nil
		}
		if !budgets {
			p.DailyCostLimit = nil
			p.WeeklyCostLimit = nil
			p.MonthlyCostLimit = nil
		}
		return p
	}
	if !rates {
		a.Policy.RequestsPerMinute = nil
		a.Policy.MaxConcurrency = nil
		a.BudgetGroupRPM = nil
		a.BudgetGroupConcurrency = nil
	}
	if !tokens {
		a.Policy.TokensPerMinute = nil
		a.BudgetGroupTPM = nil
	}
	if !budgets {
		a.Policy.DailyCostLimit = nil
		a.Policy.WeeklyCostLimit = nil
		a.Policy.MonthlyCostLimit = nil
		a.BudgetGroupDailyCostLimit = nil
		a.BudgetGroupWeeklyCostLimit = nil
		a.BudgetGroupMonthlyCostLimit = nil
		a.InstallationBudget = nil
		a.OrganizationBudget = nil
		a.ProjectBudget = nil
		a.ProjectAttributionBudgets = nil
	}
	if a.Policy.RouteLimits != nil {
		copy := access.RouteLimits{}
		for k, p := range a.Policy.RouteLimits {
			copy[k] = filter(p)
		}
		a.Policy.RouteLimits = copy
	}
	endUser := func(p *access.EndUserPolicy) *access.EndUserPolicy {
		if p == nil {
			return nil
		}
		out := *p
		out.Defaults = filter(p.Defaults)
		out.Overrides = map[string]access.AdmissionLimits{}
		for k, v := range p.Overrides {
			out.Overrides[k] = filter(v)
		}
		return &out
	}
	a.Policy.EndUserPolicy = endUser(a.Policy.EndUserPolicy)
	a.ProjectEndUserPolicy = endUser(a.ProjectEndUserPolicy)
	return a
}

func (s *Server) lookupCache(ctx context.Context, x *call) *Error {
	c := x.cache
	a := x.attempts[0]
	p := x.snapshot().Providers[a.ProviderID]
	var doc oif.Document
	if x.unary != nil {
		plan, err := x.unaryPlan(&p, a.TargetID, a.UpstreamModel)
		if err != nil {
			return requestError(err)
		}
		var err2 error
		doc, err2 = oif.ParseJSON(plan.Body(), x.unary.source.Document().Limits())
		if err2 != nil {
			return requestError(err2)
		}
	} else {
		prepared, err := x.preparedProvider(&p, a.TargetID, a.UpstreamModel)
		if err != nil {
			return requestError(err)
		}
		doc = prepared.invocation.Prepared.Document()
		x.guardrailInput = prepared.guardrailSegments
	}
	// Defaults and protocol preparation can add retention even when the caller
	// omitted it. Only the effective upstream request can establish eligibility.
	if cacheRetentionRequested(doc) {
		c.result = "bypass"
		return nil
	}
	c.target = a.TargetID
	project := x.captureProject()
	scope := x.keyID
	if c.policy.Scope == "project" {
		scope = project
	} else if c.policy.Scope == "route" {
		scope = x.route.ID
	}
	var entries any
	guardrailProject := project
	if guardrailProject == "" {
		guardrailProject = "installation"
	}
	if registry := x.snapshot().Guardrails; registry != nil {
		var err error
		entries, err = registry.Entries(guardrails.Boundary{ProjectID: guardrailProject, OrganizationID: registry.ProjectOrganizations[guardrailProject], RouteID: x.route.ID, KeyID: x.keyID})
		if err != nil {
			c.result = "bypass"
			return nil
		}
	}
	headers := map[string][]string{}
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "X-OLP-Client-Contract"} {
		if v := x.semanticHeaders.Values(name); len(v) > 0 {
			headers[name] = v
		}
	}
	// Effective entries pin immutable definition revisions and capture attachment
	// changes at every scope, without hashing unrelated tenants' policy registries.
	encoding := ""
	if x.unary == nil && x.family == wire.Embeddings {
		// Native provider documents can omit this client response control, but
		// the cache stores the response after projection to the caller's format.
		encoding = protocols.EmbeddingEncoding(x.generation.parsed, p.ParameterDefaults)
		if encoding == "" {
			encoding = "float"
		}
	}
	var dialect oif.Identity
	if x.unary != nil {
		dialect = x.unary.dialect.Identity
	}
	includeUsage := x.mode == "streaming" && x.family == wire.Chat && x.generation.parsed.IncludeUsage
	// Entries already contain the caller's projected response, including its
	// native dialect, named model, and usage preference. Upstream preparation
	// always requests stream usage for accounting, even when the caller does not.
	fingerprint, _ := json.Marshal([]any{x.route.RevisionID, x.route, entries, p.RevisionID, a.TargetID, x.surfaceName(), x.family, dialect, x.named().ID, x.named().RevisionID, x.named().Slug, x.mode, headers, x.semanticQuery, encoding, includeUsage})
	if c.policy.Mode == "semantic" {
		er, ok := x.snapshot().Routes[c.policy.EmbeddingRoute]
		if !ok {
			c.result = "bypass"
			return nil
		}
		embeddingRevision, _ := json.Marshal(er)
		fingerprint = append(fingerprint, embeddingRevision...)
		for _, t := range er.Targets {
			fingerprint = append(fingerprint, []byte(x.snapshot().Providers[t.ProviderID].RevisionID)...)
		}
	}
	sum := sha256.Sum256(fingerprint)
	space := cache.Space{Project: project, Route: x.route.ID, NamedRoute: x.named().ID, Scope: scope, Fingerprint: hex.EncodeToString(sum[:])}
	canonical := cache.Canonical(doc)
	var query string
	if c.policy.Mode == "semantic" {
		var ok bool
		canonical, query, ok = semanticInput(doc, c.policy.AllowMultiturn)
		if !ok {
			c.result = "bypass"
			return nil
		}
		// Match every non-query setting exactly, including prior conversation.
		space.Fingerprint = space.Fingerprint + ":" + hexDigest(canonical)
		canonical = append(canonical, []byte(query)...)
	}
	parallel := x.startParallel(ctx, func() {})
	if parallel != nil {
		defer parallel.cancel()
	}
	if err := parallel.join(x, true); err != nil {
		return guardrailError(err)
	}
	if parallel != nil {
		c.parallelEngine = x.guardrailEngine
		c.parallelInput = slices.Clone(x.guardrailInput)
	}
	lookup, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	var ticket cache.Ticket
	var entry *cache.Entry
	var err error
	if c.controls.NoCache {
		ticket, err = s.Cache.Begin(lookup, space, canonical)
	} else {
		ticket, entry, err = s.Cache.Lookup(lookup, space, canonical, c.controls)
	}
	cancel()
	if err != nil {
		c.result = "bypass"
		c.reason = "unavailable"
		return nil
	}
	c.ticket = ticket
	if c.controls.NoCache {
		if c.policy.Mode == "semantic" {
			c.vector, c.ticket.VectorSpace, err = s.cacheEmbedding(ctx, x, query)
			if err != nil {
				c.result = "bypass"
				c.reason = "embedding"
			}
		}
		return nil
	}
	if entry == nil && c.policy.Mode == "semantic" {
		c.vector, c.ticket.VectorSpace, err = s.cacheEmbedding(ctx, x, query)
		if err != nil {
			c.result = "bypass"
			c.reason = "embedding"
			return nil
		}
		lookup, cancel = context.WithTimeout(ctx, 100*time.Millisecond)
		var similarity float64
		entry, similarity, err = s.Cache.Nearest(lookup, c.ticket, c.vector, c.policy, c.controls)
		cancel()
		if err != nil {
			c.result = "bypass"
			c.reason = "unavailable"
			return nil
		}
		if entry != nil {
			c.similarity = &similarity
		}
	}
	if entry == nil {
		return nil
	}
	if json.Unmarshal(entry.Metadata, &c.metadata) != nil || c.metadata.ProviderID != a.ProviderID || c.metadata.ProviderRevisionID != p.RevisionID || c.metadata.UpstreamModel != a.UpstreamModel || !c.metadata.UsageComplete || c.metadata.Usage == nil && x.operationName() != "moderation" {
		c.result = "bypass"
		return nil
	}
	c.entry = entry
	c.age = int64(max(s.now().Sub(entry.CreatedAt)/time.Second, 0))
	c.budgetCost, c.budgetCharge = "0", "0"
	fact := AttemptFact{Class: classSuccess, Status: 200, Usage: c.metadata.Usage, UsageObserved: c.metadata.Usage != nil, UsageComplete: c.metadata.UsageComplete}
	u := attemptUsage(&fact, x.operationName())
	var costPtr *usage.Cost
	at := s.now()
	if a.Price != nil {
		if cost, ok := a.Price.Cost(u, at, true, a.Discount); ok {
			if x.committed(a.ProviderID) {
				cost = usage.Cost{}
			}
			costPtr = &cost
			c.budgetCost = cost.String()
			c.budgetCharge = cost.String()
		}
	}
	if rev := x.attemptCardRevision(a.ProviderID, at); rev != nil {
		if charge, ok := rev.Charge(u, at, true, costPtr, p.Kind, p.VendorID, a.UpstreamModel, x.operationName()); ok {
			c.budgetCharge = charge.String()
		}
	}
	return nil
}
func hexDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }

func (s *Server) recordCacheHit(x *call) {
	c := x.cache
	a := x.attempts[0]
	currency := ""
	if a.Price != nil {
		currency = a.Price.Currency
	}
	e := &usage.CacheEvidence{Result: c.result, AgeSeconds: c.age, Similarity: c.similarity, AvoidedCost: c.budgetCost, Currency: currency}
	if slices.Contains(c.policy.CountHitsAgainst, "budgets") {
		e.BudgetCost = c.budgetCost
		e.BudgetCharge = c.budgetCharge
	}
	f := AttemptFact{Cache: e, Class: "cache_hit", Status: 200, StartedAt: s.now(), ProviderID: a.ProviderID, ProviderRevisionID: a.ProviderRevisionID, TargetID: a.TargetID, UpstreamModel: a.UpstreamModel, Usage: c.metadata.Usage, UsageObserved: c.metadata.Usage != nil, UsageComplete: c.metadata.UsageComplete, Mode: x.mode}
	x.facts = append(x.facts, f)
	x.mirrors = false
}
func (s *Server) replayCache(ctx context.Context, x *call) *outcome {
	s.recordCacheHit(x)
	c := x.cache
	fact := &x.facts[len(x.facts)-1]
	defer func() { fact.Duration = s.now().Sub(fact.StartedAt) }()
	if x.generation.parsed.Stream {
		for _, frame := range c.entry.Frames {
			if err := ctx.Err(); err != nil {
				return &outcome{err: (&attemptFailure{class: classCancelled}).toError(), cancelled: true}
			}
			fact.Committed = true
			if err := x.generation.emit(frame); err != nil {
				return &outcome{err: (&attemptFailure{class: classCancelled}).toError(), committed: true, cancelled: true}
			}
			x.captureDeliveredFrame(frame)
		}
		return &outcome{committed: true}
	}
	return &outcome{completion: &wire.Completion{Body: c.entry.Body, Usage: c.metadata.Usage}}
}
func (x *call) captureCacheFrame(frame []byte) {
	c := x.cache
	if c == nil || c.result != "miss" || c.controls.NoStore {
		return
	}
	c.size += len(frame)
	if c.size+32*(len(c.frames)+1) > c.policy.MaxEntryBytes {
		c.result = "bypass"
		c.frames = nil
		return
	}
	c.frames = append(c.frames, slices.Clone(frame))
}
func (s *Server) storeCache(x *call, out *outcome, status int) {
	c := x.cache
	if c == nil || c.result != "miss" || c.controls.NoStore || out == nil || out.err != nil || status < 200 || status >= 300 || len(x.facts) == 0 {
		return
	}
	f := x.facts[len(x.facts)-1]
	if f.Class != classSuccess || f.TargetID != c.target || !f.UsageComplete || f.Usage == nil && x.operationName() != "moderation" {
		return
	}
	if out.completion != nil {
		c.body = out.completion.Body
	}
	if len(c.body) == 0 && len(c.frames) == 0 {
		return
	}
	metadata, _ := json.Marshal(cacheMetadata{Usage: f.Usage, ProviderID: f.ProviderID, ProviderRevisionID: f.ProviderRevisionID, UpstreamModel: f.UpstreamModel, UsageComplete: f.UsageComplete})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Cache.Put(ctx, c.ticket, cache.Entry{Body: c.body, Frames: c.frames, Metadata: metadata}, c.policy, c.controls, c.vector); err != nil {
		if !errors.Is(err, cache.ErrNotStored) {
			c.reason = "unavailable"
		}
	} else {
		c.storedBytes = len(c.body) + c.size
	}
}

// Semantic matching varies only the final user text; everything else, including
// a multi-turn prefix, tools, and output controls, remains in the exact bucket.
func semanticInput(doc oif.Document, multiturn bool) ([]byte, string, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(doc.Bytes(), &root) != nil {
		return nil, "", false
	}
	if !multiturn {
		if _, ok := root["tools"]; ok {
			return nil, "", false
		}
		if _, ok := root["functions"]; ok {
			return nil, "", false
		}
	}
	field := "messages"
	if _, ok := root[field]; !ok {
		field = "input"
		if _, ok := root[field]; !ok {
			field = "contents"
		}
	}
	var scalar string
	if field == "input" && json.Unmarshal(root[field], &scalar) == nil && scalar != "" {
		root[field] = json.RawMessage(`""`)
		raw, _ := json.Marshal(root)
		d, err := oif.ParseJSON(raw, doc.Limits())
		if err != nil {
			return nil, "", false
		}
		return cache.Canonical(d), scalar, true
	}

	var messages []map[string]json.RawMessage
	if json.Unmarshal(root[field], &messages) != nil || len(messages) == 0 {
		return nil, "", false
	}
	if !multiturn {
		for _, message := range messages[:len(messages)-1] {
			if role := string(message["role"]); role != `"system"` && role != `"developer"` {
				return nil, "", false
			}
		}
	}
	last := messages[len(messages)-1]
	if string(last["role"]) != `"user"` {
		return nil, "", false
	}
	var text string
	contentField := "content"
	if field == "contents" {
		contentField = "parts"
	}
	if json.Unmarshal(last[contentField], &text) != nil {
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(last[contentField], &blocks) != nil || len(blocks) != 1 {
			return nil, "", false
		}
		kind := string(blocks[0]["type"])
		if kind != "" && kind != `"text"` && kind != `"input_text"` || json.Unmarshal(blocks[0]["text"], &text) != nil {
			return nil, "", false
		}
		blocks[0]["text"] = json.RawMessage(`""`)
		last[contentField], _ = json.Marshal(blocks)
	} else {
		last[contentField] = json.RawMessage(`""`)
	}
	if text == "" {
		return nil, "", false
	}
	root[field], _ = json.Marshal(messages)
	raw, _ := json.Marshal(root)
	context, err := oif.ParseJSON(raw, doc.Limits())
	if err != nil {
		return nil, "", false
	}
	return cache.Canonical(context), text, true
}
