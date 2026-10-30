package routes

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/resources"
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
	offers, err := resources.CodeOffers(r.Context(), s.Access.Pool, revision)
	if err != nil {
		return access.Reply{}, err
	}
	if len(offers) == 0 {
		return access.Reply{}, access.Fail(409, "code_route_unserved", "No eligible pool account serves this route's published subscriptions.")
	}
	query := r.URL.Query()
	config, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: query.Get("gateway_url"), Client: query.Get("client"), Platform: query.Get("platform"), Model: query.Get("model"), PlanModel: query.Get("plan_model"), SmallModel: query.Get("small_model")})
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(config, route.ETag), nil
}
