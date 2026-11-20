package routes

import (
	"encoding/json"
	"net/http"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/codesessions"

	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

// CodeClientConfiguration returns configuration for the latest publication.
// The management composition registers it with the route's read authorization.
func (s *Server) CodeClientConfiguration(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	var project, revision string
	var document []byte
	err = s.Access.Pool.QueryRow(r.Context(), `SELECT r.project_id::text,coalesce(v.id::text,''),v.document
		FROM olp.code_routes r LEFT JOIN olp.code_route_revisions v ON v.id=r.latest_revision_id WHERE r.id=$1`, id).Scan(&project, &revision, &document)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err = p.Project(&project, authz.View); err != nil {
		return httpkit.Reply{}, err
	}
	if len(document) == 0 {
		return httpkit.Reply{}, httpkit.Fail(409, "code_route_unpublished", "Publish an enabled code-mode route first.")
	}
	var route codemode.Route
	if err = json.Unmarshal(document, &route); err != nil {
		return httpkit.Reply{}, err
	}
	offers, err := codesessions.Offers(r.Context(), s.Access.Pool, revision)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if len(offers) == 0 {
		return httpkit.Reply{}, httpkit.Fail(409, "code_route_unserved", "No eligible target serves this published code route.")
	}
	query := r.URL.Query()
	config, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: query.Get("gateway_url"), Client: query.Get("client"), Platform: query.Get("platform"), Model: query.Get("model"), PlanModel: query.Get("plan_model"), SmallModel: query.Get("small_model")})
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.Detail(config, route.ETag), nil
}
