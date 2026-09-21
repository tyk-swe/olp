package access

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) settings(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if !p.AllProjects {
		return Reply{}, Forbidden()
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT to_jsonb(s) FROM olp_go.settings s ORDER BY key")
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return OK(map[string]any{"items": items}), err
}
func (s *Server) setting(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if !p.AllProjects {
		return Reply{}, Forbidden()
	}
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT to_jsonb(s),etag::text FROM olp_go.settings s WHERE key=$1", r.PathValue("key")).Scan(&data, &etag)
	return Detail(json.RawMessage(data), etag), err
}
func (s *Server) updateSetting(r *http.Request) (Reply, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	key := r.PathValue("key")
	switch key {
	case "retention.requests_days", "retention.usage_days", "retention.audit_days":
		n, err := strconv.Atoi(input.Value)
		if err != nil || n < 1 || n > 3650 {
			return Reply{}, Invalid("value", "Use an integer from 1 to 3650.")
		}
	case "limits.valkey_unavailable":
		if input.Value != "fail_open" && input.Value != "fail_closed" {
			return Reply{}, Invalid("value", "Use fail_open or fail_closed.")
		}
	case "auth.local_login_enabled":
		if input.Value != "true" && input.Value != "false" {
			return Reply{}, Invalid("value", "Use true or false.")
		}
	default:
		return Reply{}, Fail(404, "not_found", "This setting does not exist.")
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "settings")
	if err != nil {
		return Reply{}, err
	}
	if key == "auth.local_login_enabled" && p.Role != "owner" {
		return Reply{}, Forbidden()
	}
	var etag string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text FROM olp_go.settings WHERE key=$1", key).Scan(&etag); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.settings SET value=$1,etag=$2,updated_by=$3,updated_at=now() WHERE key=$4", input.Value, etag, p.UserID(), key); err != nil {
		return Reply{}, err
	}
	if key == "auth.local_login_enabled" {
		if err = s.usableOwner(r, tx); err != nil {
			return Reply{}, err
		}
	}
	if err = Audit(r.Context(), tx, r, p.ID, "setting.update", "setting", key, "success"); err != nil {
		return Reply{}, err
	}
	var data []byte
	if err = tx.QueryRow(r.Context(), "SELECT to_jsonb(s) FROM olp_go.settings s WHERE key=$1", key).Scan(&data); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(json.RawMessage(data), etag))
}
func (s *Server) auditEvents(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	if !p.AllProjects {
		return Reply{}, Forbidden()
	}
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
        FROM olp_go.audit a LEFT JOIN olp_go.users u ON u.id=a.actor_user_id LEFT JOIN olp_go.management_tokens t ON t.id=a.actor_management_token_id
        WHERE a.id<$1 AND ($2='' OR a.action=$2) AND ($3='' OR a.resource_type=$3) AND ($4='' OR a.resource_id=$4) AND ($5::uuid IS NULL OR a.actor_user_id=$5) AND ($6='' OR a.outcome=$6) AND ($7::timestamptz IS NULL OR a.occurred_at>=$7) AND ($8::timestamptz IS NULL OR a.occurred_at<=$8)
        ORDER BY a.id DESC LIMIT $9`, page.Before, q.Get("action"), q.Get("resource_type"), q.Get("resource_id"), actor, outcome, after, before, page.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, page), err
}
