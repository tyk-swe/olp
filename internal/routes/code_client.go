package routes

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

// CodeClientConfiguration returns configuration for the latest publication.
// The management composition registers it with the route's read authorization.
func (s *Server) CodeClientConfiguration(r *http.Request, p access.Principal) (access.Reply, error) {
	id, err := access.IDParam(r, "id")
	if err != nil {
		return access.Reply{}, err
	}
	var project, revision string
	var document []byte
	err = s.Access.Pool.QueryRow(r.Context(), `SELECT r.project_id::text,coalesce(v.id::text,''),v.document
		FROM olp.code_routes r LEFT JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, id).Scan(&project, &revision, &document)
	if err != nil {
		return access.Reply{}, err
	}
	if err = p.Project(&project, access.View); err != nil {
		return access.Reply{}, err
	}
	if len(document) == 0 {
		return access.Reply{}, access.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	var route codemode.Route
	if err = json.Unmarshal(document, &route); err != nil {
		return access.Reply{}, err
	}
	offers, err := s.codeOffers(r.Context(), revision)
	if err != nil {
		return access.Reply{}, err
	}
	if len(offers) == 0 {
		// The Codex fallback fits a revision whose frozen connections name
		// no adapter, as before adapters existed. When they do, the pool
		// has stopped serving the published subscriptions: no honest
		// configuration exists until an account returns or the route is
		// republished.
		var frozen []codemode.Adapter
		if err = s.Access.Pool.QueryRow(r.Context(), `SELECT `+codeadapter.SQLAdapters("connections")+` FROM olp.code_route_revisions WHERE id=$1`, revision).Scan(&frozen); err != nil {
			return access.Reply{}, err
		}
		if len(frozen) != 0 {
			return access.Reply{}, access.Fail(409, "code_route_unserved", "No pool account serves this route's published subscriptions; restore one or republish.")
		}
		offers = codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: route.Models}}
	}
	query := r.URL.Query()
	config, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: query.Get("gateway_url"), Client: query.Get("client"), Model: query.Get("model"), PlanModel: query.Get("plan_model"), SmallModel: query.Get("small_model")})
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(config, route.ETag), nil
}

// codeOffers reads the offers of a revision's pool accounts whose providers
// it froze, the accounts its requests can reach. An account offers its models
// only while it is eligible to serve them — the same permanent predicates
// codeAccount applies; transient availability is a dispatch concern.
func (s *Server) codeOffers(ctx context.Context, revision string) (codeclient.Offers, error) {
	rows, err := s.Access.Pool.Query(ctx, `SELECT DISTINCT a.models,`+codeadapter.SQL("(v.connections->(a.provider_id::text))")+`
		FROM olp.code_route_revisions v JOIN olp.code_pool_accounts pa ON pa.pool_id=(v.document->>'pool_id')::uuid
		JOIN olp.code_accounts a ON a.id=pa.account_id
		JOIN olp.provider_credentials c ON c.id=a.credential_id JOIN olp.provider_grants g ON g.credential_id=c.id JOIN olp.providers p ON p.id=a.provider_id
		WHERE v.id=$1 AND v.connections ? a.provider_id::text
		AND a.project_id=(v.document->>'project_id')::uuid AND a.enabled AND p.state<>'disabled' AND p.project_id=a.project_id
		AND c.provider_id=a.provider_id AND c.principal=a.principal AND c.revoked_at IS NULL AND g.lapsed_at IS NULL AND (g.expires_at IS NULL OR g.expires_at>now())`, revision)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var offers codeclient.Offers
	for rows.Next() {
		var models []string
		var adapter *string
		if err = rows.Scan(&models, &adapter); err != nil {
			return nil, err
		}
		if adapter != nil {
			offers = append(offers, codeclient.Offer{Adapter: codemode.Adapter(*adapter), Models: models})
		}
	}
	return offers, rows.Err()
}
