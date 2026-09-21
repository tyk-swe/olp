package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/secrets"
)

type KeyPolicy struct {
	Scopes                 []string   `json:"scopes"`
	AllowedRoutes          []string   `json:"allowed_routes"`
	AllowedAttributionKeys []string   `json:"allowed_attribution_keys"`
	RequestsPerMinute      *int64     `json:"requests_per_minute"`
	TokensPerMinute        *int64     `json:"tokens_per_minute"`
	MaxConcurrency         *int64     `json:"max_concurrency"`
	DailyCostLimit         *string    `json:"daily_cost_limit"`
	MonthlyCostLimit       *string    `json:"monthly_cost_limit"`
	ExpiresAt              *time.Time `json:"expires_at"`
	AllowProviderState     bool       `json:"allow_provider_state"`
}
type keyInput struct {
	Name          string  `json:"name"`
	ProjectID     *string `json:"project_id"`
	BudgetGroupID *string `json:"budget_group_id"`
	KeyPolicy
}

var RouteSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
var decimal = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`)
var attributionKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,31}$`)

const maxAttributionKeys = 8

func validateKey(input keyInput, expirationChanged bool) error {
	if err := ValidText("name", input.Name, 100); err != nil {
		return err
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > 2 {
		return Invalid("scopes", "Select at least one unique scope.")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if (scope != "inference" && scope != "models_read") || seen[scope] {
			return Invalid("scopes", "Use unique inference and models_read scopes.")
		}
		seen[scope] = true
	}
	if input.AllowedRoutes == nil {
		return Invalid("allowed_routes", "This field cannot be null.")
	}
	if len(input.AllowedRoutes) > 100 {
		return Invalid("allowed_routes", "Use at most 100 route slugs.")
	}
	seen = map[string]bool{}
	for _, route := range input.AllowedRoutes {
		if !RouteSlug.MatchString(route) || seen[route] {
			return Invalid("allowed_routes", "Use unique valid route slugs.")
		}
		seen[route] = true
	}
	if input.AllowedAttributionKeys == nil {
		input.AllowedAttributionKeys = []string{}
	}
	if len(input.AllowedAttributionKeys) > maxAttributionKeys {
		return Invalid("allowed_attribution_keys", "Use at most 8 attribution keys.")
	}
	seen = map[string]bool{}
	for _, key := range input.AllowedAttributionKeys {
		if !attributionKey.MatchString(key) || seen[key] {
			return Invalid("allowed_attribution_keys", "Use unique valid attribution keys.")
		}
		seen[key] = true
	}
	for field, value := range map[string]*int64{"requests_per_minute": input.RequestsPerMinute, "max_concurrency": input.MaxConcurrency} {
		if value != nil && (*value < 1 || *value > 2147483647) {
			return Invalid(field, "Use a positive integer of at most 2147483647.")
		}
	}
	if input.TokensPerMinute != nil && (*input.TokensPerMinute < 1 || *input.TokensPerMinute > limits.MaxCounter) {
		return Invalid("tokens_per_minute", "Use a positive integer of at most 9007199254740991.")
	}
	for field, value := range map[string]*string{"daily_cost_limit": input.DailyCostLimit, "monthly_cost_limit": input.MonthlyCostLimit} {
		if value != nil {
			amount := strings.TrimSpace(*value)
			if !decimal.MatchString(amount) || !strings.ContainsAny(amount, "123456789") {
				return Invalid(field, "Use a positive decimal amount with at most 12 integer and 12 fractional digits.")
			}
			*value = amount
		}
	}
	if expirationChanged && input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return Invalid("expires_at", "Choose a future expiry.")
	}
	return nil
}
func ParseUUID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", Invalid("id", "Use a valid UUID.")
	}
	return id.String(), nil
}
func AdvanceAuthority(r *http.Request, tx pgx.Tx) (any, error) {
	var id string
	var sequence int64
	err := tx.QueryRow(r.Context(), "UPDATE olp_go.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton RETURNING authority_id::text,authority_sequence", NewID()).Scan(&id, &sequence)
	return map[string]any{"id": id, "sequence": sequence}, err
}

const keyFields = `'id',k.id,'lookup_id',k.lookup_id,'name',k.name,'project_id',k.project_id,'project_name',pr.name,'budget_group_id',k.budget_group_id,'created_by',k.created_by,'created_by_email',u.email,'etag',k.etag,'created_at',k.created_at,'expires_at',k.expires_at,'revoked_at',k.revoked_at,'rotated_at',k.rotated_at,'scopes',k.policy->'scopes','allowed_routes',k.policy->'allowed_routes','requests_per_minute',k.policy->'requests_per_minute','tokens_per_minute',k.policy->'tokens_per_minute','max_concurrency',k.policy->'max_concurrency','allowed_attribution_keys',COALESCE(k.policy->'allowed_attribution_keys','[]'::jsonb),'allow_provider_state',COALESCE(k.policy->'allow_provider_state','false'::jsonb)`
const keyFrom = " FROM olp_go.api_keys k JOIN olp_go.users u ON u.id=k.created_by LEFT JOIN olp_go.projects pr ON pr.id=k.project_id"

// keyJSON renders one API key row, whose alias must be k, as the management
// contract's key detail. The budget is live accounting: accrued spend and
// unpriced attempts are summed from the recorded facts and their retained
// rollups, the amounts they are measured against come from the stored policy,
// and enforcement_active tells the console whether this installation admits
// requests against them at all.
func (s *Server) keyJSON() string {
	enforcement := "false"
	if s.LimitsEnforced {
		enforcement = "true"
	}
	return `jsonb_build_object(` + keyFields + `,'budget',jsonb_set(jsonb_set(` + limits.BudgetSQL +
		`||jsonb_build_object('enforcement_active',` + enforcement +
		`),'{daily,limit}',COALESCE(k.policy->'daily_cost_limit','null'::jsonb)),'{monthly,limit}',COALESCE(k.policy->'monthly_cost_limit','null'::jsonb)))`
}

func (s *Server) apiKeys(r *http.Request) (Reply, error) {
	principal, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	p, err := Page(r)
	if err != nil {
		return Reply{}, err
	}
	var issuer any
	if raw := r.URL.Query().Get("created_by"); raw != "" {
		issuer, err = ParseUUID(raw)
		if err != nil {
			return Reply{}, err
		}
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+s.keyJSON()+keyFrom+" WHERE k.id<$1 AND ($2::uuid IS NULL OR k.created_by=$2) AND ($3 OR k.project_id=ANY($4::uuid[])) ORDER BY k.id DESC LIMIT $5", p.Before, issuer, principal.AllProjects, principal.ProjectIDs(), p.Limit+1)
	if err != nil {
		return Reply{}, err
	}
	items, err := JSONRows(rows)
	return ListReply(items, p), err
}
func (s *Server) apiKey(r *http.Request) (Reply, error) {
	p, err := s.Principal(r, s.Pool, "read")
	if err != nil {
		return Reply{}, err
	}
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var projectID *string
	if err = s.Pool.QueryRow(r.Context(), "SELECT "+s.keyJSON()+",k.etag::text,k.project_id::text"+keyFrom+" WHERE k.id=$1", id).Scan(&data, &etag, &projectID); err != nil {
		return Reply{}, err
	}
	if !p.CanProject(projectID, false) {
		return Reply{}, pgx.ErrNoRows
	}
	return Detail(json.RawMessage(data), etag), nil
}
func (s *Server) createAPIKey(r *http.Request) (Reply, error) {
	input := keyInput{KeyPolicy: KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{}, AllowedAttributionKeys: []string{}}}
	if err := Decode(r, &input); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "keys")
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	// A replay returns the original result even if its key has since expired.
	if err := validateKey(input, true); err != nil {
		return Reply{}, err
	}
	if err = s.RequireProject(r.Context(), tx, p, input.ProjectID, true); err != nil {
		return Reply{}, err
	}
	if input.BudgetGroupID != nil {
		var parsed string
		if parsed, err = ParseUUID(*input.BudgetGroupID); err != nil {
			return Reply{}, err
		}
		input.BudgetGroupID = &parsed
	}
	if err = checkBudgetGroup(r.Context(), tx, input.BudgetGroupID, input.ProjectID); err != nil {
		return Reply{}, err
	}
	if err = checkKeyRoutes(r, tx, input.AllowedRoutes, input.ProjectID); err != nil {
		return Reply{}, err
	}
	id, lookup, etag := NewID(), secrets.Token(), NewID()
	secret := "olp_" + lookup + "_" + secrets.Token()
	policy, err := json.Marshal(input.KeyPolicy)
	if err != nil {
		return Reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.api_keys(id,lookup_id,digest,name,created_by,project_id,budget_group_id,policy,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)", id, lookup, s.Auth.Digest("api_key", secret), strings.TrimSpace(input.Name), p.UserID(), input.ProjectID, input.BudgetGroupID, policy, etag, input.ExpiresAt); err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r, tx)
	if err != nil {
		return Reply{}, err
	}
	result := Reply{Status: 201, ETag: etag, Location: "/api/v3/api-keys/" + id, Body: map[string]any{"id": id, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}}
	if err = Audit(r.Context(), tx, r, p.ID, "api_key.create", "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}
func (s *Server) updateAPIKey(r *http.Request) (Reply, error) {
	var patch map[string]json.RawMessage
	if err := Decode(r, &patch); err != nil {
		return Reply{}, err
	}
	if patch == nil {
		return Reply{}, Invalid("policy", "Send a policy object.")
	}
	allowed := []string{"name", "scopes", "allowed_routes", "allowed_attribution_keys", "requests_per_minute", "tokens_per_minute", "max_concurrency", "daily_cost_limit", "monthly_cost_limit", "expires_at", "budget_group_id", "allow_provider_state"}
	for field, value := range patch {
		if !slices.Contains(allowed, field) {
			return Reply{}, Invalid(field, "Unknown policy field.")
		}
		if string(value) == "null" && (field == "name" || field == "scopes" || field == "allowed_routes") {
			return Reply{}, Invalid(field, "This field cannot be null.")
		}
	}
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "keys")
	if err != nil {
		return Reply{}, err
	}
	var data []byte
	var etag string
	var revoked *time.Time
	var projectID *string
	var groupID *string
	if err = tx.QueryRow(r.Context(), "SELECT policy||jsonb_build_object('name',name),etag::text,revoked_at,project_id::text,budget_group_id::text FROM olp_go.api_keys WHERE id=$1", id).Scan(&data, &etag, &revoked, &projectID, &groupID); err != nil {
		return Reply{}, err
	}
	if err := ProjectAccess(p, projectID, true); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if revoked != nil {
		return Reply{}, Fail(409, "api_key_revoked", "A revoked key cannot be edited.")
	}
	var merged map[string]json.RawMessage
	if err = json.Unmarshal(data, &merged); err != nil {
		return Reply{}, err
	}
	maps.Copy(merged, patch)
	data, err = json.Marshal(merged)
	if err != nil {
		return Reply{}, err
	}
	var input keyInput
	if err = json.Unmarshal(data, &input); err != nil {
		return Reply{}, Invalid("policy", "Invalid policy value.")
	}
	_, changed := patch["expires_at"]
	if err = validateKey(input, changed); err != nil {
		return Reply{}, err
	}
	if raw, ok := patch["budget_group_id"]; ok {
		if err = json.Unmarshal(raw, &groupID); err != nil {
			return Reply{}, Invalid("budget_group_id", "Use a budget group UUID or null.")
		}
		if groupID != nil {
			var parsed string
			if parsed, err = ParseUUID(*groupID); err != nil {
				return Reply{}, err
			}
			groupID = &parsed
		}
		if err = checkBudgetGroup(r.Context(), tx, groupID, projectID); err != nil {
			return Reply{}, err
		}
	}
	if err = checkKeyRoutes(r, tx, input.AllowedRoutes, projectID); err != nil {
		return Reply{}, err
	}
	data, err = json.Marshal(input.KeyPolicy)
	if err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET name=$1,policy=$2,expires_at=$3,budget_group_id=$4,etag=$5 WHERE id=$6", strings.TrimSpace(input.Name), data, input.ExpiresAt, groupID, etag, id); err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.ID, "api_key.update", "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag))
}
func (s *Server) revokeAPIKey(r *http.Request) (Reply, error) { return s.transitionKey(r, false) }
func (s *Server) rotateAPIKey(r *http.Request) (Reply, error) { return s.transitionKey(r, true) }
func (s *Server) transitionKey(r *http.Request, rotate bool) (Reply, error) {
	var input map[string]json.RawMessage
	if rotate && r.ContentLength != 0 {
		if err := Decode(r, &input); err != nil {
			return Reply{}, err
		}
		for field := range input {
			if field != "daily_cost_limit" && field != "monthly_cost_limit" && field != "budget_group_id" {
				return Reply{}, Invalid(field, "Only cost budgets may accompany rotation.")
			}
		}
	}
	id, err := IDParam(r, "api_key_id")
	if err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Principal(r, tx, "keys")
	if err != nil {
		return Reply{}, err
	}
	claim, replayed, err := s.Replay(r, tx, p, input)
	if err != nil {
		return Reply{}, err
	}
	if replayed != nil {
		return Commit(r, tx, *replayed)
	}
	var etag, name string
	var data []byte
	var revoked *time.Time
	var projectID *string
	var groupID *string
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,name,policy,revoked_at,project_id::text,budget_group_id::text FROM olp_go.api_keys WHERE id=$1", id).Scan(&etag, &name, &data, &revoked, &projectID, &groupID); err != nil {
		return Reply{}, err
	}
	if err := ProjectAccess(p, projectID, true); err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	if revoked != nil {
		return Reply{}, Fail(409, "api_key_revoked", "This key is already revoked.")
	}
	etag = NewID()
	action := "api_key.revoke"
	var secret, lookup string
	if rotate {
		if raw, ok := input["budget_group_id"]; ok {
			delete(input, "budget_group_id")
			if err = json.Unmarshal(raw, &groupID); err != nil {
				return Reply{}, Invalid("budget_group_id", "Use a budget group UUID or null.")
			}
			if groupID != nil {
				var parsed string
				if parsed, err = ParseUUID(*groupID); err != nil {
					return Reply{}, err
				}
				groupID = &parsed
			}
			if err = checkBudgetGroup(r.Context(), tx, groupID, projectID); err != nil {
				return Reply{}, err
			}
		}
		var policy map[string]json.RawMessage
		if err = json.Unmarshal(data, &policy); err != nil {
			return Reply{}, err
		}
		maps.Copy(policy, input)
		data, err = json.Marshal(policy)
		if err != nil {
			return Reply{}, err
		}
		var normalized KeyPolicy
		if err = json.Unmarshal(data, &normalized); err != nil {
			return Reply{}, Invalid("policy", "Invalid cost budget.")
		}
		if err = validateKey(keyInput{Name: name, KeyPolicy: normalized}, false); err != nil {
			return Reply{}, err
		}
		data, err = json.Marshal(normalized)
		if err != nil {
			return Reply{}, err
		}
		action = "api_key.rotate"
		lookup = secrets.Token()
		secret = "olp_" + lookup + "_" + secrets.Token()
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET lookup_id=$1,digest=$2,etag=$3,rotated_at=now(),policy=$4,budget_group_id=$5 WHERE id=$6", lookup, s.Auth.Digest("api_key", secret), etag, data, groupID, id)
	} else {
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id)
	}
	if err != nil {
		return Reply{}, err
	}
	generation, err := AdvanceAuthority(r, tx)
	if err != nil {
		return Reply{}, err
	}
	result := Detail(generation, etag)
	if rotate {
		result.Body = map[string]any{"id": id, "etag": etag, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}
	}
	if err = Audit(r.Context(), tx, r, p.ID, action, "api_key", id, "success"); err != nil {
		return Reply{}, err
	}
	if err = s.CompleteReplay(r, tx, claim, result); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, result)
}

func checkKeyRoutes(r *http.Request, q Queryer, routes []string, projectID *string) error {
	if len(routes) == 0 {
		return nil
	}
	var mismatched bool
	if err := q.QueryRow(r.Context(), "SELECT EXISTS(SELECT 1 FROM olp_go.routes WHERE slug=ANY($1::text[]) AND project_id IS DISTINCT FROM $2::uuid)", routes, projectID).Scan(&mismatched); err != nil {
		return err
	}
	if mismatched {
		return Invalid("allowed_routes", "Allowed routes must belong to the key's project.")
	}
	return nil
}

// Authority is durable input for the independent runtime authority refresh. Lookup never
// grants one positive scope through the other or applies unenforced limits.
// LookupID is the public lookup segment of the secret, which identifies the key
// in shared state that must never carry the key's internal identifier.
type Authority struct {
	ID, Issuer, LookupID        string
	ProjectID                   *string
	BudgetGroupID               *string
	BudgetGroupDailyCostLimit   *string
	BudgetGroupMonthlyCostLimit *string
	Policy                      KeyPolicy
	ExpiresAt, RevokedAt        *time.Time
}

func (s *Server) LookupAuthority(ctx context.Context, secret string) (Authority, error) {
	var a Authority
	parts := strings.Split(secret, "_")
	if len(parts) != 3 || parts[0] != "olp" {
		return a, errors.New("invalid API key")
	}
	var digest, data []byte
	err := s.Pool.QueryRow(ctx, "SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.digest,k.policy,k.expires_at,k.revoked_at,k.budget_group_id::text,g.daily_cost_limit::text,g.monthly_cost_limit::text FROM olp_go.api_keys k LEFT JOIN olp_go.budget_groups g ON g.id=k.budget_group_id WHERE k.lookup_id=$1", parts[1]).Scan(&a.ID, &a.LookupID, &a.Issuer, &a.ProjectID, &digest, &data, &a.ExpiresAt, &a.RevokedAt, &a.BudgetGroupID, &a.BudgetGroupDailyCostLimit, &a.BudgetGroupMonthlyCostLimit)
	if err != nil || !hmac.Equal(digest, s.Auth.Digest("api_key", secret)) {
		return a, errors.New("invalid API key")
	}
	if err = json.Unmarshal(data, &a.Policy); err != nil {
		return a, err
	}
	return a, nil
}
func (a Authority) Allows(scope, route string, now time.Time) bool {
	if scope != "inference" && scope != "models_read" {
		return false
	}
	if a.RevokedAt != nil || (a.ExpiresAt != nil && !a.ExpiresAt.After(now)) {
		return false
	}
	return slices.Contains(a.Policy.Scopes, scope) &&
		(len(a.Policy.AllowedRoutes) == 0 || slices.Contains(a.Policy.AllowedRoutes, route))
}
