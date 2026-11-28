package release

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

func RegisterGenerations(a *access.Server, mux *http.ServeMux) {
	a.Route(mux, "GET /api/v1/runtime-generations", func(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
		page, err := httpkit.Page(r)
		if err != nil {
			return httpkit.Reply{}, err
		}
		items, err := httpkit.QueryJSON(r.Context(), a.Pool, "SELECT jsonb_build_object('id',g.id,'sequence',g.sequence,'sha256',g.sha256,'created_by',g.created_by,'created_by_email',u.email,'created_at',g.created_at) FROM olp.runtime_releases g JOIN olp.users u ON u.id=g.created_by WHERE g.id<$1 ORDER BY g.id DESC LIMIT $2", page.Before, page.Limit+1)
		if err != nil {
			return httpkit.Reply{}, err
		}
		return httpkit.ListReply(items, page), nil
	})
}
