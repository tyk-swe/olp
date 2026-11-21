package kernel

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/domain/protocols"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

const cacheReadTimeout = 100 * time.Millisecond

type cacheSubject struct {
	attempt   routing.Attempt
	provider  routing.Provider
	document  oif.Document
	generates bool
	segments  []guardrails.Segment
}

type cacheAddress struct {
	space     cache.Space
	canonical []byte
	query     string
}

type cacheSearch struct {
	ticket     cache.Ticket
	entry      *cache.Entry
	vector     []float32
	similarity *float64
	bypass     bool
	reason     usage.CacheLookupReason
}

func (s *Server) lookupCache(ctx context.Context, x *Call) *fault.Error {
	c := x.cache
	subject, refusal := x.cacheSubject()
	if refusal != nil {
		return refusal
	}
	if subject.generates {
		x.guard.input = subject.segments
	}
	if cacheRetentionRequested(subject.document) || x.bypassNonSingleCacheCandidate(subject.document) {
		c.result = "bypass"
		return nil
	}
	c.target = subject.attempt.TargetID
	address, ok := x.cacheAddress(subject)
	if !ok {
		c.result = "bypass"
		return nil
	}
	parallel := s.startParallel(ctx, x, func() {})
	if parallel != nil {
		defer parallel.cancel()
	}
	if err := parallel.join(x, true); err != nil {
		return guardrailError(err)
	}
	if parallel != nil {
		c.parallelEngine = x.guard.engine
		c.parallelInput = slices.Clone(x.guard.input)
	}
	found := s.searchCache(ctx, x, address)
	c.ticket, c.vector, c.similarity = found.ticket, found.vector, found.similarity
	if found.reason != "" {
		x.noteCacheLookupReason(found.reason)
	}
	if found.bypass {
		c.result, c.backendIssue = "bypass", true
		return nil
	}
	if found.entry == nil {
		return nil
	}
	metadata, replayable := subject.replays(found.entry, x.OperationName())
	c.metadata = metadata
	if !replayable {
		c.result = "bypass"
		return nil
	}
	c.entry = found.entry
	c.age = int64(max(s.Now().Sub(found.entry.CreatedAt)/time.Second, 0))
	c.budgetCost, c.budgetCharge = s.cacheHitSpend(x, subject, metadata)
	return nil
}

func (x *Call) cacheSubject() (cacheSubject, *fault.Error) {
	attempt := x.attempts[0]
	provider := x.Snapshot().Provider(attempt.ProviderID)
	if x.unary != nil {
		plan, err := x.unaryPlan(&provider, attempt.TargetID, attempt.UpstreamModel)
		if err != nil {
			return cacheSubject{}, RequestError(err)
		}
		document, err := oif.ParseJSON(plan.Body(), x.unary.source.Document().Limits())
		if err != nil {
			return cacheSubject{}, RequestError(err)
		}
		return cacheSubject{attempt: attempt, provider: provider, document: document}, nil
	}
	prepared, err := x.generationRequest(&provider, attempt.TargetID, attempt.UpstreamModel)
	if err != nil {
		return cacheSubject{}, RequestError(err)
	}
	return cacheSubject{attempt: attempt, provider: provider, document: prepared.document, generates: true, segments: prepared.guardrailSegments}, nil
}

func (x *Call) cacheAddress(subject cacheSubject) (cacheAddress, bool) {
	c := x.cache
	project := x.captureProject()
	fingerprint, ok := x.cacheFingerprint(subject, project)
	if !ok {
		return cacheAddress{}, false
	}
	sum := sha256.Sum256(fingerprint)
	space := cache.Space{Project: project, Route: x.Route.ID, NamedRoute: x.named().ID, Scope: x.cacheScope(project), Fingerprint: hex.EncodeToString(sum[:])}
	if c.policy.Mode != "semantic" {
		return cacheAddress{space: space, canonical: cache.Canonical(subject.document)}, true
	}
	settings, query, ok := semanticInput(subject.document, c.policy.AllowMultiturn)
	if !ok {
		return cacheAddress{}, false
	}
	// Match every non-query setting exactly, including prior conversation.
	space.Fingerprint = space.Fingerprint + ":" + hexDigest(settings)
	return cacheAddress{space: space, canonical: append(settings, []byte(query)...), query: query}, true
}

func (x *Call) cacheScope(project string) string {
	switch x.cache.policy.Scope {
	case "project":
		return project
	case "route":
		return x.Route.ID
	}
	return x.keyID
}

func (x *Call) cacheFingerprint(subject cacheSubject, project string) ([]byte, bool) {
	// Effective entries pin immutable definition revisions and capture attachment
	// changes at every scope, without hashing unrelated tenants' policy registries.
	entries, err := x.Snapshot().GuardrailEntries(guardrails.Boundary{ProjectID: cmp.Or(project, guardrails.InstallationProject), RouteID: x.Route.ID, KeyID: x.keyID})
	if err != nil {
		return nil, false
	}
	var dialect oif.Identity
	if x.unary != nil {
		dialect = x.unary.dialect.Identity
	}
	includeUsage := x.mode == "streaming" && x.family == wire.Chat && x.generation.parsed.IncludeUsage
	// Entries already contain the caller's projected response, including its
	// native dialect, named model, and usage preference. Upstream preparation
	// always requests stream usage for accounting, even when the caller does not.
	fingerprint, _ := json.Marshal([]any{x.Route.RevisionID, x.Route, entries, subject.provider.RevisionID, subject.attempt.TargetID, x.surfaceName(), x.family, dialect, x.named().ID, x.named().RevisionID, x.named().Slug, x.mode, x.cachedProviderHeaders(), x.semanticQuery, x.cachedEncoding(subject.provider), includeUsage})
	if x.cache.policy.Mode != "semantic" {
		return fingerprint, true
	}
	embedding, ok := x.Snapshot().FindRoute(x.cache.policy.EmbeddingRoute)
	if !ok {
		return nil, false
	}
	revision, _ := json.Marshal(embedding)
	fingerprint = append(fingerprint, revision...)
	for _, target := range embedding.Targets {
		fingerprint = append(fingerprint, []byte(x.Snapshot().Provider(target.ProviderID).RevisionID)...)
	}
	return fingerprint, true
}

func (x *Call) cachedProviderHeaders() map[string][]string {
	headers := map[string][]string{}
	for _, name := range []string{"Anthropic-Version", "Anthropic-Beta", "OpenAI-Organization", "OpenAI-Project", "OpenAI-Beta", "X-OLP-Client-Contract"} {
		if v := x.semanticHeaders.Values(name); len(v) > 0 {
			headers[name] = v
		}
	}
	return headers
}

func (x *Call) cachedEncoding(provider routing.Provider) string {
	if x.unary != nil || x.family != wire.Embeddings {
		return ""
	}
	// Native provider documents can omit this client response control, but
	// the cache stores the response after projection to the caller's format.
	return cmp.Or(protocols.EmbeddingEncoding(x.generation.parsed, provider.ParameterDefaults), "float")
}

func (s *Server) searchCache(ctx context.Context, x *Call, address cacheAddress) cacheSearch {
	c := x.cache
	exact, err := s.exactCache(ctx, c, address)
	if err != nil {
		return cacheSearch{bypass: true, reason: usage.CacheLookupUnavailable}
	}
	if c.policy.Mode != "semantic" || exact.entry != nil {
		return exact
	}
	vector, vectorSpace, err := s.cacheEmbedding(ctx, x, address.query)
	ticket := exact.ticket
	ticket.VectorSpace = vectorSpace
	if err != nil {
		return cacheSearch{ticket: ticket, vector: vector, bypass: true}
	}
	if c.controls.NoCache {
		return cacheSearch{ticket: ticket, vector: vector}
	}
	lookup, cancel := context.WithTimeout(ctx, cacheReadTimeout)
	defer cancel()
	entry, similarity, err := s.Deps.Cache.Nearest(lookup, ticket, vector, c.policy, c.controls)
	if err != nil {
		return cacheSearch{ticket: ticket, vector: vector, bypass: true}
	}
	if entry == nil {
		return cacheSearch{ticket: ticket, vector: vector}
	}
	return cacheSearch{ticket: ticket, vector: vector, entry: entry, similarity: &similarity}
}

func (s *Server) exactCache(ctx context.Context, c *responseCacheCall, address cacheAddress) (cacheSearch, error) {
	lookup, cancel := context.WithTimeout(ctx, cacheReadTimeout)
	defer cancel()
	if c.controls.NoCache {
		ticket, err := s.Deps.Cache.Begin(lookup, address.space, address.canonical)
		if err != nil {
			return cacheSearch{}, err
		}
		return cacheSearch{ticket: ticket, reason: usage.CacheLookupRefresh}, nil
	}
	ticket, entry, err := s.Deps.Cache.Lookup(lookup, address.space, address.canonical, c.controls)
	return cacheSearch{ticket: ticket, entry: entry}, err
}

func (subject cacheSubject) replays(entry *cache.Entry, operation string) (cacheMetadata, bool) {
	var metadata cacheMetadata
	if json.Unmarshal(entry.Metadata, &metadata) != nil {
		return metadata, false
	}
	produced := metadata.ProviderID == subject.attempt.ProviderID && metadata.ProviderRevisionID == subject.provider.RevisionID && metadata.UpstreamModel == subject.attempt.UpstreamModel
	metered := metadata.UsageComplete && (metadata.Usage != nil || operation == "moderation")
	return metadata, produced && metered
}

func (s *Server) cacheHitSpend(x *Call, subject cacheSubject, metadata cacheMetadata) (string, string) {
	attempt, provider := subject.attempt, subject.provider
	fact := events.AttemptFact{Class: ClassSuccess, Status: 200, Usage: metadata.Usage, UsageObserved: metadata.Usage != nil, UsageComplete: metadata.UsageComplete}
	used := events.AttemptUsage(&fact, x.OperationName())
	at := s.Now()
	cost, priced := cacheHitCost(x, attempt, used, at)
	spend := "0"
	var costed *rates.Cost
	if priced {
		spend, costed = cost.String(), &cost
	}
	revision := x.attemptCardRevision(attempt.ProviderID, at)
	if revision == nil {
		return spend, spend
	}
	charge, ok := revision.Charge(used, at, true, costed, string(provider.Kind), provider.VendorID, attempt.UpstreamModel, x.OperationName())
	if !ok {
		return spend, spend
	}
	return spend, charge.String()
}

func cacheHitCost(x *Call, attempt routing.Attempt, used usage.AttemptUsage, at time.Time) (rates.Cost, bool) {
	if attempt.Price == nil {
		return rates.Cost{}, false
	}
	cost, ok := attempt.Price.Cost(used, at, true, attempt.Discount)
	if !ok {
		return rates.Cost{}, false
	}
	if x.committed(attempt.ProviderID) {
		return rates.Cost{}, true
	}
	return cost, true
}
