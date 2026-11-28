package auditlog

import (
	"net/http"
	"strings"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

type API struct{ server *access.Server }

func NewAPI(server *access.Server) *API { return &API{server: server} }

func (a *API) Register(mux *http.ServeMux) {
	a.server.Route(mux, "GET /api/v1/audit", a.list)
}

func (a *API) list(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var err error
	page, err := httpkit.Page(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	q := r.URL.Query()
	outcome := q.Get("outcome")
	if outcome != "" && outcome != "success" && outcome != "failure" {
		return httpkit.Reply{}, httpkit.Invalid("outcome", "Use success or failure.")
	}
	var actor, after, before any
	if raw := q.Get("actor_user_id"); raw != "" {
		actor, err = httpkit.ParseUUID(raw)
		if err != nil {
			return httpkit.Reply{}, err
		}
	}
	for key, target := range map[string]*any{"occurred_after": &after, "occurred_before": &before} {
		if raw := q.Get(key); raw != "" {
			v, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return httpkit.Reply{}, httpkit.Invalid(key, "Use an RFC3339 date and time.")
			}
			*target = v
		}
	}
	if after != nil && before != nil && after.(time.Time).After(before.(time.Time)) {
		return httpkit.Reply{}, httpkit.Invalid("occurred_before", "The end must follow the start.")
	}
	for _, key := range []string{"action", "resource_type", "resource_id"} {
		if len(q.Get(key)) > 200 || strings.ContainsAny(q.Get(key), "\r\n") {
			return httpkit.Reply{}, httpkit.Invalid(key, "Invalid audit filter.")
		}
	}
	items, err := httpkit.QueryJSON(r.Context(), a.server.Pool, `SELECT jsonb_build_object('id',a.id,'actor_user_id',a.actor_user_id,'actor_management_token_id',a.actor_management_token_id,'actor_type',CASE WHEN a.actor_user_id IS NOT NULL THEN 'user' WHEN a.actor_management_token_id IS NOT NULL THEN 'management_token' ELSE 'system' END,'actor_label',COALESCE(u.email,t.name),'actor_email',u.email,'action',a.action,'resource_type',a.resource_type,'resource_id',a.resource_id,'outcome',a.outcome,'source_ip',a.source_ip,'user_agent_family',a.user_agent_family,'occurred_at',a.occurred_at)
        FROM olp.audit a LEFT JOIN olp.users u ON u.id=a.actor_user_id LEFT JOIN olp.management_tokens t ON t.id=a.actor_management_token_id
        WHERE a.id<$1 AND ($2='' OR a.action=$2) AND ($3='' OR a.resource_type=$3) AND ($4='' OR a.resource_id=$4) AND ($5::uuid IS NULL OR a.actor_user_id=$5) AND ($6='' OR a.outcome=$6) AND ($7::timestamptz IS NULL OR a.occurred_at>=$7) AND ($8::timestamptz IS NULL OR a.occurred_at<=$8)
        ORDER BY a.id DESC LIMIT $9`, page.Before, q.Get("action"), q.Get("resource_type"), q.Get("resource_id"), actor, outcome, after, before, page.Limit+1)
	return httpkit.ListReply(items, page), err
}
