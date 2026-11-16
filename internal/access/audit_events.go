package access

import (
	"net/http"
	"strings"
	"time"
)

func (s *Server) auditEvents(r *http.Request, p Principal) (Reply, error) {
	var err error
	page, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	q := r.URL.Query()
	outcome := q.Get("outcome")
	if outcome != "" && outcome != "success" && outcome != "failure" {
		return Reply{}, Invalid("outcome", "Use success or failure.")
	}
	var actor, after, before any
	if raw := q.Get("actor_user_id"); raw != "" {
		actor, err = ParseUUID(raw)
		if err != nil {
			return Reply{}, err
		}
	}
	for key, target := range map[string]*any{"occurred_after": &after, "occurred_before": &before} {
		if raw := q.Get(key); raw != "" {
			v, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return Reply{}, Invalid(key, "Use an RFC3339 date and time.")
			}
			*target = v
		}
	}
	if after != nil && before != nil && after.(time.Time).After(before.(time.Time)) {
		return Reply{}, Invalid("occurred_before", "The end must follow the start.")
	}
	for _, key := range []string{"action", "resource_type", "resource_id"} {
		if len(q.Get(key)) > 200 || strings.ContainsAny(q.Get(key), "\r\n") {
			return Reply{}, Invalid(key, "Invalid audit filter.")
		}
	}
	rows, err := s.Pool.Query(r.Context(), `SELECT jsonb_build_object('id',a.id,'actor_user_id',a.actor_user_id,'actor_management_token_id',a.actor_management_token_id,'actor_type',CASE WHEN a.actor_user_id IS NOT NULL THEN 'user' WHEN a.actor_management_token_id IS NOT NULL THEN 'management_token' ELSE 'system' END,'actor_label',COALESCE(u.email,t.name),'actor_email',u.email,'action',a.action,'resource_type',a.resource_type,'resource_id',a.resource_id,'outcome',a.outcome,'source_ip',a.source_ip,'user_agent_family',a.user_agent_family,'occurred_at',a.occurred_at)
        FROM olp.audit a LEFT JOIN olp.users u ON u.id=a.actor_user_id LEFT JOIN olp.management_tokens t ON t.id=a.actor_management_token_id
        WHERE a.id<$1 AND ($2='' OR a.action=$2) AND ($3='' OR a.resource_type=$3) AND ($4='' OR a.resource_id=$4) AND ($5::uuid IS NULL OR a.actor_user_id=$5) AND ($6='' OR a.outcome=$6) AND ($7::timestamptz IS NULL OR a.occurred_at>=$7) AND ($8::timestamptz IS NULL OR a.occurred_at<=$8)
        ORDER BY a.id DESC LIMIT $9`, page.Before, q.Get("action"), q.Get("resource_type"), q.Get("resource_id"), actor, outcome, after, before, page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}
