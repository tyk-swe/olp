package apikeys

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

var keyPatchFields = []string{"run_limits", "allowed_toolsets", "allowed_agents", "regional_limits", "route_limits", "limit_template", "name", "scopes", "rotation_interval_days", "allowed_routes", "allowed_route_groups", "allowed_cidrs", "allowed_attribution_keys", "required_attribution_keys", "attribution_defaults", "requests_per_minute", "tokens_per_minute", "max_concurrency", "daily_cost_limit", "monthly_cost_limit", "weekly_cost_limit", "cost_basis", "expires_at", "budget_group_id", "allow_provider_state", "response_metadata", "forbid_response_cache", "end_user_source", "end_user_policy", "priority", "max_priority"}

var keyRotationFields = []string{"daily_cost_limit", "monthly_cost_limit", "weekly_cost_limit", "budget_group_id", "overlap_seconds"}

type storedKey struct {
	id, etag, name   string
	policy, document []byte
	revoked          *time.Time
	paused           *time.Time
	expires          *time.Time
	project, group   *string
	managed          bool
}

// lockKey seeds document's expiry from the authoritative column, so an omitted
// patch field preserves it even when the policy JSON has a stale value.
func lockKey(r *http.Request, tx pgx.Tx, p authz.Principal, id string) (storedKey, error) {
	key := storedKey{id: id}
	err := tx.QueryRow(r.Context(), `SELECT etag::text,name,policy,policy||jsonb_build_object('name',name,'expires_at',expires_at),revoked_at,project_id::text,budget_group_id::text,workload_issuer_id IS NOT NULL,paused_at,expires_at
 FROM olp.api_keys WHERE id=$1 FOR UPDATE`, id).Scan(&key.etag, &key.name, &key.policy, &key.document, &key.revoked, &key.project, &key.group, &key.managed, &key.paused, &key.expires)
	if err != nil {
		return storedKey{}, err
	}
	return key, p.Project(key.project, authz.Change)
}

func (k storedKey) live(r *http.Request, revoked string) error {
	if err := httpkit.Match(r, k.etag); err != nil {
		return err
	}
	if k.revoked != nil {
		return problem.Fail(409, "api_key_revoked", revoked)
	}
	return nil
}

func (k storedKey) regrouped(ctx context.Context, q database.Queryer, patch map[string]json.RawMessage) (*string, error) {
	raw, ok := patch["budget_group_id"]
	if !ok {
		return k.group, nil
	}
	var group *string
	if err := json.Unmarshal(raw, &group); err != nil {
		return nil, problem.Invalid("budget_group_id", "Use a budget group UUID or null.")
	}
	return budgetGroup(ctx, q, group, k.project)
}

func budgetGroup(ctx context.Context, q database.Queryer, group, project *string) (*string, error) {
	if group == nil {
		return nil, checkBudgetGroup(ctx, q, nil, project)
	}
	parsed, err := ids.Parse(*group)
	if err != nil {
		return nil, err
	}
	return &parsed, checkBudgetGroup(ctx, q, &parsed, project)
}

func checkKeyReferences(r *http.Request, q database.Queryer, policy, agentResources keyauthority.KeyPolicy, project *string) error {
	if err := access.CheckLimitTemplates(r.Context(), q, project, policy.LimitTemplate, endUserTemplate(policy.EndUserPolicy)); err != nil {
		return err
	}
	if err := checkKeyRouteGroups(r, q, policy.AllowedRouteGroups, project); err != nil {
		return err
	}
	if err := checkKeyAgentResources(r.Context(), q, agentResources, project); err != nil {
		return err
	}
	return access.ValidateRouteReferences(r, q, policy.AllowedRoutes, project)
}

func keyEvent(action, id string) access.Event {
	return access.Event{Action: action, Resource: "api_key", ID: id}
}

func SetETag(ctx context.Context, tx pgx.Tx, id, etag string) error {
	_, err := tx.Exec(ctx, "UPDATE olp.api_keys SET etag=$2 WHERE id=$1", id, etag)
	return err
}

func (a *API) createAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input := keyInput{KeyPolicy: keyauthority.KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{}, AllowedAttributionKeys: []string{}}}
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	project, err := ids.ParseProject(input.ProjectID)
	if err != nil {
		return httpkit.Reply{}, err
	}
	input.ProjectID = project
	return access.Mutate(a.server, r, input, func(tx pgx.Tx, p authz.Principal) (struct{}, error) {
		return struct{}{}, a.server.RequireProject(r.Context(), tx, p, project)
	}, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		return a.insertKey(r, tx, p, input)
	})
}

func (a *API) insertKey(r *http.Request, tx pgx.Tx, p authz.Principal, input keyInput) (httpkit.Reply, access.Event, error) {
	if err := validateAndNormalizeKey(input, true); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	group, err := budgetGroup(r.Context(), tx, input.BudgetGroupID, input.ProjectID)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := checkKeyReferences(r, tx, input.KeyPolicy, input.KeyPolicy, input.ProjectID); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	id, lookup, etag := ids.New(), secrets.Token(), ids.New()
	secret := "olp_" + lookup + "_" + secrets.Token()
	policy, err := json.Marshal(input.KeyPolicy)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if _, err := tx.Exec(r.Context(), "INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,project_id,budget_group_id,policy,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, lookup, a.server.Auth.Digest(secrets.APIKeyDigest, secret), strings.TrimSpace(input.Name), p.UserID(), input.ProjectID, group, policy, etag, input.ExpiresAt); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := ensureRouteBudgetAccounts(r.Context(), tx, id, input.RouteLimits); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	generation, err := access.AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	body := map[string]any{"id": id, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}
	return httpkit.Reply{Status: 201, ETag: etag, Location: "/api/v1/api-keys/" + id, Body: body}, keyEvent("api_key.create", id), nil
}

func decodeKeyPatch(r *http.Request) (map[string]json.RawMessage, error) {
	var patch map[string]json.RawMessage
	if err := httpkit.Decode(r, &patch); err != nil {
		return nil, err
	}
	if patch == nil {
		return nil, problem.Invalid("policy", "Send a policy object.")
	}
	for field, value := range patch {
		if !slices.Contains(keyPatchFields, field) {
			return nil, problem.Invalid(field, "Unknown policy field.")
		}
		if string(value) == "null" && (field == "name" || field == "scopes" || field == "allowed_routes") {
			return nil, problem.Invalid(field, "This field cannot be null.")
		}
	}
	return patch, nil
}

func (a *API) updateAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	patch, err := decodeKeyPatch(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Transact(a.server, r, func(tx pgx.Tx, p authz.Principal) (storedKey, error) {
		key, err := lockKey(r, tx, p, id)
		if err != nil {
			return storedKey{}, err
		}
		if key.managed {
			return storedKey{}, problem.Fail(409, "workload_managed", "Edit this principal's workload issuer mapping instead.")
		}
		return key, key.live(r, "A revoked key cannot be edited.")
	}, func(tx pgx.Tx, _ authz.Principal, key storedKey) (httpkit.Reply, access.Event, error) {
		return patchKey(r, tx, key, patch)
	})
}

func (k storedKey) patched(patch map[string]json.RawMessage) (keyInput, keyauthority.KeyPolicy, error) {
	var merged map[string]json.RawMessage
	if err := json.Unmarshal(k.document, &merged); err != nil {
		return keyInput{}, keyauthority.KeyPolicy{}, err
	}
	var previous keyauthority.KeyPolicy
	if err := json.Unmarshal(k.document, &previous); err != nil {
		return keyInput{}, keyauthority.KeyPolicy{}, err
	}
	maps.Copy(merged, patch)
	document, err := json.Marshal(merged)
	if err != nil {
		return keyInput{}, keyauthority.KeyPolicy{}, err
	}
	var input keyInput
	if err := json.Unmarshal(document, &input); err != nil {
		return keyInput{}, keyauthority.KeyPolicy{}, problem.Invalid("policy", "Invalid policy value.")
	}
	_, expirationChanged := patch["expires_at"]
	return input, previous, validateAndNormalizeKey(input, expirationChanged)
}

func patchKey(r *http.Request, tx pgx.Tx, key storedKey, patch map[string]json.RawMessage) (httpkit.Reply, access.Event, error) {
	input, previous, err := key.patched(patch)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	group, err := key.regrouped(r.Context(), tx, patch)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := checkKeyReferences(r, tx, input.KeyPolicy, addedKeyAgentResources(input.KeyPolicy, previous), key.project); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	policy, err := json.Marshal(input.KeyPolicy)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	etag := ids.New()
	if _, err := tx.Exec(r.Context(), "UPDATE olp.api_keys SET name=$1,policy=$2,expires_at=$3,budget_group_id=$4,etag=$5 WHERE id=$6", strings.TrimSpace(input.Name), policy, input.ExpiresAt, group, etag, key.id); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	if err := ensureRouteBudgetAccounts(r.Context(), tx, key.id, input.RouteLimits); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	generation, err := access.AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	return httpkit.Detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag), keyEvent("api_key.update", key.id), nil
}

func (a *API) revokeAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, nil, func(tx pgx.Tx, p authz.Principal) (storedKey, error) {
		return lockKey(r, tx, p, id)
	}, func(tx pgx.Tx, _ authz.Principal, key storedKey) (httpkit.Reply, access.Event, error) {
		if err := key.live(r, "This key is already revoked."); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		etag := ids.New()
		if _, err := tx.Exec(r.Context(), "UPDATE olp.api_keys SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		generation, err := access.AdvanceAuthority(r.Context(), tx)
		return httpkit.Detail(generation, etag), keyEvent("api_key.revoke", id), err
	})
}

func decodeRotation(r *http.Request) (map[string]json.RawMessage, int, error) {
	if r.ContentLength == 0 {
		return nil, 0, nil
	}
	var input map[string]json.RawMessage
	if err := httpkit.Decode(r, &input); err != nil {
		return nil, 0, err
	}
	for field := range input {
		if !slices.Contains(keyRotationFields, field) {
			return nil, 0, problem.Invalid(field, "Only cost budgets and overlap_seconds may accompany rotation.")
		}
	}
	raw, ok := input["overlap_seconds"]
	if !ok {
		return input, 0, nil
	}
	var overlap int
	if string(raw) == "null" || json.Unmarshal(raw, &overlap) != nil || overlap < 0 || overlap > 86400 {
		return nil, 0, problem.Invalid("overlap_seconds", "Use 0–86400 seconds of overlap.")
	}
	return input, overlap, nil
}

func (a *API) rotateAPIKey(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	input, overlap, err := decodeRotation(r)
	if err != nil {
		return httpkit.Reply{}, err
	}
	id, err := httpkit.IDParam(r, "api_key_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	return access.Mutate(a.server, r, input, func(tx pgx.Tx, p authz.Principal) (storedKey, error) {
		key, err := lockKey(r, tx, p, id)
		if err == nil && key.managed {
			return storedKey{}, problem.Fail(409, "workload_managed", "Workload principals do not have API secrets to rotate.")
		}
		return key, err
	}, func(tx pgx.Tx, _ authz.Principal, key storedKey) (httpkit.Reply, access.Event, error) {
		if err := key.live(r, "This key is already revoked."); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		return a.rotateKey(r, tx, key, input, overlap)
	})
}

func (a *API) rotateKey(r *http.Request, tx pgx.Tx, key storedKey, input map[string]json.RawMessage, overlap int) (httpkit.Reply, access.Event, error) {
	group, err := key.regrouped(r.Context(), tx, input)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	overlapUntil, err := overlapPreviousSecret(r.Context(), tx, key.id, overlap)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	policy, err := key.rebudgeted(input)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	etag, lookup := ids.New(), secrets.Token()
	secret := "olp_" + lookup + "_" + secrets.Token()
	if _, err := tx.Exec(r.Context(), "UPDATE olp.api_keys SET limit_lookup_id=COALESCE(limit_lookup_id,lookup_id),lookup_id=$1,digest=$2,etag=$3,rotated_at=now(),policy=$4,budget_group_id=$5 WHERE id=$6", lookup, a.server.Auth.Digest(secrets.APIKeyDigest, secret), etag, policy, group, key.id); err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	generation, err := access.AdvanceAuthority(r.Context(), tx)
	if err != nil {
		return httpkit.Reply{}, access.Event{}, err
	}
	body := map[string]any{"id": key.id, "etag": etag, "lookup_id": lookup, "secret": secret, "runtime_generation": generation, "overlap_expires_at": overlapUntil}
	return httpkit.Detail(body, etag), keyEvent("api_key.rotate", key.id), nil
}

func overlapPreviousSecret(ctx context.Context, tx pgx.Tx, id string, seconds int) (*time.Time, error) {
	if _, err := tx.Exec(ctx, "DELETE FROM olp.api_key_overlaps WHERE api_key_id=$1 AND (expires_at<=now() OR $2=0)", id, seconds); err != nil {
		return nil, err
	}
	if seconds == 0 {
		return nil, nil
	}
	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM olp.api_key_overlaps WHERE api_key_id=$1", id).Scan(&count); err != nil {
		return nil, err
	}
	if count >= 8 {
		return nil, problem.Fail(409, "key_overlap_limit", "Eight previous secrets are still active. Wait for an overlap to end, or rotate with zero overlap.")
	}
	var until *time.Time
	err := tx.QueryRow(ctx, `INSERT INTO olp.api_key_overlaps(lookup_id,api_key_id,digest,expires_at)
     SELECT lookup_id,id,digest,LEAST(now()+make_interval(secs=>$2),expires_at) FROM olp.api_keys WHERE id=$1 RETURNING expires_at`, id, seconds).Scan(&until)
	return until, err
}

func (k storedKey) rebudgeted(input map[string]json.RawMessage) ([]byte, error) {
	var policy map[string]json.RawMessage
	if err := json.Unmarshal(k.policy, &policy); err != nil {
		return nil, err
	}
	for field, value := range input {
		if field != "budget_group_id" && field != "overlap_seconds" {
			policy[field] = value
		}
	}
	merged, err := json.Marshal(policy)
	if err != nil {
		return nil, err
	}
	var normalized keyauthority.KeyPolicy
	if err := json.Unmarshal(merged, &normalized); err != nil {
		return nil, problem.Invalid("policy", "Invalid cost budget.")
	}
	if err := validateAndNormalizeKey(keyInput{Name: k.name, KeyPolicy: normalized}, false); err != nil {
		return nil, err
	}
	return json.Marshal(normalized)
}

func endUserTemplate(p *keyauthority.EndUserPolicy) *string {
	if p == nil {
		return nil
	}
	return p.LimitTemplate
}
