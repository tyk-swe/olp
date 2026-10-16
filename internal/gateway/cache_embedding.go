package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/operationplan"
	"github.com/tyk-swe/olp/internal/operationregistry"
	"github.com/tyk-swe/olp/internal/protocols"
	"github.com/tyk-swe/olp/internal/protocols/wire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

func (s *Server) cacheEmbedding(ctx context.Context, parent *call, text string) ([]float32, string, error) {
	route, ok := parent.snapshot().Routes[parent.cache.policy.EmbeddingRoute]
	if !ok || !parent.authority.Allows("inference", route.Slug, route.ProjectID, s.now()) {
		return nil, "", errors.New("embedding route unavailable")
	}
	authority := cacheAuthority(parent.authority, false, true, true)
	// The parent owns the shared caller concurrency slot. The distinct
	// embeddings route still enforces its own per-route request/concurrency caps.
	authority.Policy.RouteLimits = parent.authority.Policy.RouteLimits
	x := &call{request: request{id: uuid.Must(uuid.NewV7()).String(), minted: true, startedAt: s.now(), release: parent.request.release}, family: wire.Embeddings, actor: parent.actor, keyID: parent.keyID, authority: authority, attribution: parent.attribution, endUserDigest: parent.endUserDigest, budgetGroupID: parent.budgetGroupID, origin: usage.OriginCacheEmbedding, parent: parent.request.accountingID(), priority: parent.priority, mode: "unary", routeLeg: routeLeg{route: &route}}
	var out *outcome
	defer func() {
		status := http.StatusOK
		if out == nil {
			status = 500
		} else if out.err != nil {
			status = out.err.Status
		}
		s.finish(x, out, status)
		s.settleAdmission(context.WithoutCancel(ctx), x)
	}()
	body, _ := json.Marshal(map[string]any{"model": route.Slug, "input": text, "encoding_format": "float"})
	x.request.body = &measuredBody{decoded: int64(len(body))}
	var raw []byte
	if route.Fidelity.Strict() {
		codec, _ := operationregistry.Lookup("openai-embeddings")
		source, err := operationplan.Parse("openai-embeddings", body, int(s.cfg.MaxBodyBytes))
		if err != nil {
			return nil, "", err
		}
		x.unary = &unaryCall{source: source, dialect: codec, route: route.Slug, surface: "openai"}
		if e := s.prepareUnary(ctx, x); e != nil {
			out = &outcome{err: e}
			return nil, "", e
		}
		ctx, cancel := x.routeContext(ctx)
		defer cancel()
		if e := s.admitClassifier(ctx, x); e != nil {
			out = &outcome{err: e}
			return nil, "", e
		}
		result := runAttempts(ctx, s, x, attemptAdapter[operationplan.Result]{estimate: x.attemptReservation, dispatch: func(ctx context.Context, a routing.Attempt, p *routing.Provider, slot routing.Slot, fact AttemptFact) (AttemptFact, operationplan.Result, *attemptFailure) {
			return s.unaryAttempt(ctx, x, a, p, slot, fact)
		}})
		out = &outcome{err: result.err, committed: result.committed, cancelled: result.cancelled}
		raw = result.result.Body
	} else {
		parsed, err := protocols.Parse(wire.Embeddings, body, "")
		if err != nil {
			return nil, "", err
		}
		x.generation.parsed = parsed
		if e := s.prepare(ctx, x, s.keyAuthorizer(x.authority)); e != nil {
			out = &outcome{err: e}
			return nil, "", e
		}
		if e := s.admitClassifier(ctx, x); e != nil {
			out = &outcome{err: e}
			return nil, "", e
		}
		out = s.execute(ctx, x)
		if out.completion != nil {
			raw = out.completion.Body
		}
	}
	if out.err != nil {
		return nil, "", out.err
	}
	var response struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Data) != 1 {
		return nil, "", errors.New("invalid embedding response")
	}
	vector := response.Data[0].Embedding
	if err := cache.ValidateVector(vector); err != nil {
		return nil, "", err
	}
	if len(x.facts) == 0 || x.facts[len(x.facts)-1].Class != classSuccess {
		return nil, "", errors.New("missing embedding provenance")
	}
	// The successful attempt may be a weighted or failover target. Pin the
	// vector space to that model and provider revision, not just the route.
	fact := x.facts[len(x.facts)-1]
	identity, _ := json.Marshal([]string{fact.ProviderID, fact.ProviderRevisionID, fact.UpstreamModel})
	return vector, hexDigest(identity), nil
}
