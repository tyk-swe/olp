package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

const policyETag = "00000000-0000-0000-0000-000000000000"

func policyJSON(p *runtime.Policy) map[string]any {
	if p == nil {
		p = &runtime.Policy{}
	}
	body, _ := json.Marshal(p)
	var result map[string]any
	_ = json.Unmarshal(body, &result)
	return result
}
func defaultPolicy() map[string]any { return policyJSON(nil) }
func samePolicy(a, b *runtime.Policy) bool {
	left, _ := json.Marshal(policyJSON(a))
	right, _ := json.Marshal(policyJSON(b))
	return bytes.Equal(left, right)
}
func policyScope(r *http.Request) (string, string, string, error) {
	scope := r.PathValue("scope")
	id, err := access.IDParam(r, "id")
	if err != nil {
		return "", "", "", err
	}
	permission := "configure"
	switch scope {
	case "installation":
		if id != uuid.Nil.String() {
			return "", "", "", access.Invalid("id", "Installation policy uses the nil UUID")
		}
		permission = "settings"
	case "route-draft":
	case "api-key":
		permission = "keys"
	default:
		return "", "", "", access.Fail(404, "not_found", "Unknown policy scope")
	}
	return scope, id, permission, nil
}
func loadPolicy(ctx context.Context, q access.Queryer, scope, id string, lock bool) (*runtime.Policy, string, error) {
	etag := policyETag
	if scope != "installation" {
		table := "route_drafts"
		if scope == "api-key" {
			table = "api_keys"
		}
		query := "SELECT etag::text FROM olp_go." + table + " WHERE id=$1"
		if lock {
			query += " FOR UPDATE"
		}
		if e := q.QueryRow(ctx, query, id).Scan(&etag); e != nil {
			return nil, "", e
		}
	}
	var data []byte
	var storedETag string
	query := "SELECT policy,etag::text FROM olp_go.routing_policies WHERE scope=$1 AND scope_id=$2"
	if lock {
		query += " FOR UPDATE"
	}
	err := q.QueryRow(ctx, query, scope, id).Scan(&data, &storedETag)
	if errors.Is(err, pgx.ErrNoRows) {
		return &runtime.Policy{}, etag, nil
	}
	if err != nil {
		return nil, "", err
	}
	if scope == "installation" {
		etag = storedETag
	}
	var p runtime.Policy
	err = json.Unmarshal(data, &p)
	return &p, etag, err
}
func (s *Server) policy(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	scope, id, _, err := policyScope(r)
	if err != nil {
		return access.Reply{}, err
	}
	policy, etag, err := loadPolicy(r.Context(), s.Access.Pool, scope, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"policy": policyJSON(policy), "etag": etag}, etag), nil
}
func (s *Server) putPolicy(r *http.Request) (access.Reply, error) {
	scope, id, permission, err := policyScope(r)
	if err != nil {
		return access.Reply{}, err
	}
	var policy runtime.Policy
	if err = access.Decode(r, &policy); err != nil {
		return access.Reply{}, err
	}
	if err = policy.Validate(); err != nil {
		return access.Reply{}, err
	}
	a := s.Access
	tx, err := a.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	principal, err := a.Principal(r, tx, permission)
	if err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := a.Replay(r, tx, principal, nil)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	_, etag, err := loadPolicy(r.Context(), tx, scope, id, true)
	if err != nil {
		return access.Reply{}, err
	}
	if err = access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	etag = access.NewID()
	data, _ := json.Marshal(policy)
	if _, err = tx.Exec(r.Context(), `INSERT INTO olp_go.routing_policies(scope,scope_id,policy,etag,updated_by) VALUES($1,$2,$3,$4,$5) ON CONFLICT(scope,scope_id) DO UPDATE SET policy=excluded.policy,etag=excluded.etag,updated_by=excluded.updated_by,updated_at=now()`, scope, id, data, etag, principal.ID); err != nil {
		return access.Reply{}, err
	}
	switch scope {
	case "route-draft":
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.route_drafts SET etag=$2,state='draft',updated_at=now() WHERE id=$1", id, etag)
	case "api-key":
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET etag=$2 WHERE id=$1", id, etag)
	}
	if err != nil {
		return access.Reply{}, err
	}
	if scope != "route-draft" {
		if _, err = runtime.Publish(r.Context(), tx, principal.ID); err != nil {
			return access.Reply{}, err
		}
	}
	if err = access.Audit(r.Context(), tx, r, principal.ID, "routing_policy.update", scope, id, "success"); err != nil {
		return access.Reply{}, err
	}
	reply := access.Detail(map[string]any{"policy": policyJSON(&policy), "etag": etag}, etag)
	if err = a.CompleteReplay(r, tx, claim, reply); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, reply)
}
