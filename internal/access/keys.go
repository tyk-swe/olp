package access

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/secrets"
)

type KeyPolicy struct {
	Scopes            []string   `json:"scopes"`
	AllowedRoutes     []string   `json:"allowed_routes"`
	RequestsPerMinute *int64     `json:"requests_per_minute"`
	TokensPerMinute   *int64     `json:"tokens_per_minute"`
	MaxConcurrency    *int64     `json:"max_concurrency"`
	DailyCostLimit    *string    `json:"daily_cost_limit"`
	MonthlyCostLimit  *string    `json:"monthly_cost_limit"`
	ExpiresAt         *time.Time `json:"expires_at"`
}
type keyInput struct {
	Name string `json:"name"`
	KeyPolicy
}

var routeSlug = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
var decimal = regexp.MustCompile(`^[0-9]{1,12}(\.[0-9]{1,12})?$`)

func validateKey(input keyInput, expirationChanged bool) error {
	if err := validText("name", input.Name, 100); err != nil {
		return err
	}
	if len(input.Scopes) < 1 || len(input.Scopes) > 2 {
		return invalid("scopes", "Select at least one unique scope.")
	}
	seen := map[string]bool{}
	for _, scope := range input.Scopes {
		if (scope != "inference" && scope != "models_read") || seen[scope] {
			return invalid("scopes", "Use unique inference and models_read scopes.")
		}
		seen[scope] = true
	}
	if input.AllowedRoutes == nil {
		return invalid("allowed_routes", "This field cannot be null.")
	}
	if len(input.AllowedRoutes) > 100 {
		return invalid("allowed_routes", "Use at most 100 route slugs.")
	}
	seen = map[string]bool{}
	for _, route := range input.AllowedRoutes {
		if !routeSlug.MatchString(route) || seen[route] {
			return invalid("allowed_routes", "Use unique valid route slugs.")
		}
		seen[route] = true
	}
	for field, value := range map[string]*int64{"requests_per_minute": input.RequestsPerMinute, "max_concurrency": input.MaxConcurrency} {
		if value != nil && (*value < 1 || *value > 2147483647) {
			return invalid(field, "Use a positive integer of at most 2147483647.")
		}
	}
	if input.TokensPerMinute != nil && *input.TokensPerMinute < 1 {
		return invalid("tokens_per_minute", "Use a positive integer of at most 9223372036854775807.")
	}
	for field, value := range map[string]*string{"daily_cost_limit": input.DailyCostLimit, "monthly_cost_limit": input.MonthlyCostLimit} {
		if value != nil {
			amount := strings.TrimSpace(*value)
			if !decimal.MatchString(amount) || !strings.ContainsAny(amount, "123456789") {
				return invalid(field, "Use a positive decimal amount with at most 12 integer and 12 fractional digits.")
			}
			*value = amount
		}
	}
	if expirationChanged && input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return invalid("expires_at", "Choose a future expiry.")
	}
	return nil
}
func parseUUID(value string) (string, error) {
	id, err := uuid.Parse(value)
	if err != nil {
		return "", invalid("id", "Use a valid UUID.")
	}
	return id.String(), nil
}
func advanceAuthority(r *http.Request, tx pgx.Tx) (any, error) {
	var id string
	var sequence int64
	err := tx.QueryRow(r.Context(), "UPDATE olp_go.installation SET authority_id=$1,authority_sequence=authority_sequence+1 WHERE singleton RETURNING authority_id::text,authority_sequence", newID()).Scan(&id, &sequence)
	return map[string]any{"id": id, "sequence": sequence}, err
}

const keyJSON = `jsonb_build_object('id',k.id,'lookup_id',k.lookup_id,'name',k.name,'created_by',k.created_by,'created_by_email',u.email,'etag',k.etag,'created_at',k.created_at,'expires_at',k.expires_at,'revoked_at',k.revoked_at,'rotated_at',k.rotated_at,'scopes',k.policy->'scopes','allowed_routes',k.policy->'allowed_routes','requests_per_minute',k.policy->'requests_per_minute','tokens_per_minute',k.policy->'tokens_per_minute','max_concurrency',k.policy->'max_concurrency','budget',jsonb_build_object('enforcement_active',false,'daily',jsonb_build_object('limit',k.policy->'daily_cost_limit','accrued','0','window_ends_at',date_trunc('day',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'+interval '1 day'),'monthly',jsonb_build_object('limit',k.policy->'monthly_cost_limit','accrued','0','window_ends_at',date_trunc('month',now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'+interval '1 month'),'unpriced_attempts',0))`
const keyFrom = " FROM olp_go.api_keys k JOIN olp_go.users u ON u.id=k.created_by"

func (s *Server) apiKeys(r *http.Request) (reply, error) {
	if _, err := s.principal(r, s.Pool, "read"); err != nil {
		return reply{}, err
	}
	p, err := page(r)
	if err != nil {
		return reply{}, err
	}
	var issuer any
	if raw := r.URL.Query().Get("created_by"); raw != "" {
		issuer, err = parseUUID(raw)
		if err != nil {
			return reply{}, err
		}
	}
	rows, err := s.Pool.Query(r.Context(), "SELECT "+keyJSON+keyFrom+" WHERE k.id<$1 AND ($2::uuid IS NULL OR k.created_by=$2) ORDER BY k.id DESC LIMIT $3", p.Before, issuer, p.Limit+1)
	if err != nil {
		return reply{}, err
	}
	items, err := jsonRows(rows)
	return listReply(items, p), err
}
func (s *Server) apiKey(r *http.Request) (reply, error) {
	if _, err := s.principal(r, s.Pool, "read"); err != nil {
		return reply{}, err
	}
	id, err := idParam(r, "api_key_id")
	if err != nil {
		return reply{}, err
	}
	var data []byte
	var etag string
	err = s.Pool.QueryRow(r.Context(), "SELECT "+keyJSON+",k.etag::text"+keyFrom+" WHERE k.id=$1", id).Scan(&data, &etag)
	return detail(rawJSON(data), etag), err
}
func (s *Server) createAPIKey(r *http.Request) (reply, error) {
	input := keyInput{KeyPolicy: KeyPolicy{Scopes: []string{"inference"}, AllowedRoutes: []string{}}}
	if err := decode(r, &input); err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "keys")
	if err != nil {
		return reply{}, err
	}
	claim, replayed, err := s.replay(r, tx, p, input)
	if err != nil {
		return reply{}, err
	}
	if replayed != nil {
		return commit(r, tx, *replayed)
	}
	// A replay returns the original result even if its key has since expired.
	if err := validateKey(input, true); err != nil {
		return reply{}, err
	}
	id, lookup, etag := newID(), secrets.Token(), newID()
	secret := "olp_" + lookup + "_" + secrets.Token()
	policy, err := json.Marshal(input.KeyPolicy)
	if err != nil {
		return reply{}, err
	}
	if _, err = tx.Exec(r.Context(), "INSERT INTO olp_go.api_keys(id,lookup_id,digest,name,created_by,policy,etag,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, lookup, s.Auth.Digest("api_key", secret), strings.TrimSpace(input.Name), p.ID, policy, etag, input.ExpiresAt); err != nil {
		return reply{}, err
	}
	generation, err := advanceAuthority(r, tx)
	if err != nil {
		return reply{}, err
	}
	result := reply{Status: 201, ETag: etag, Location: "/api/v3/api-keys/" + id, Body: map[string]any{"id": id, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}}
	if err = audit(r.Context(), tx, r, p.ID, "api_key.create", "api_key", id, "success"); err != nil {
		return reply{}, err
	}
	if err = s.completeReplay(r, tx, claim, result); err != nil {
		return reply{}, err
	}
	return commit(r, tx, result)
}
func (s *Server) updateAPIKey(r *http.Request) (reply, error) {
	var patch map[string]json.RawMessage
	if err := decode(r, &patch); err != nil {
		return reply{}, err
	}
	if patch == nil {
		return reply{}, invalid("policy", "Send a policy object.")
	}
	allowed := []string{"name", "scopes", "allowed_routes", "requests_per_minute", "tokens_per_minute", "max_concurrency", "daily_cost_limit", "monthly_cost_limit", "expires_at"}
	for field, value := range patch {
		if !slices.Contains(allowed, field) {
			return reply{}, invalid(field, "Unknown policy field.")
		}
		if string(value) == "null" && (field == "name" || field == "scopes" || field == "allowed_routes") {
			return reply{}, invalid(field, "This field cannot be null.")
		}
	}
	id, err := idParam(r, "api_key_id")
	if err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "keys")
	if err != nil {
		return reply{}, err
	}
	var data []byte
	var etag string
	var revoked *time.Time
	if err = tx.QueryRow(r.Context(), "SELECT policy||jsonb_build_object('name',name),etag::text,revoked_at FROM olp_go.api_keys WHERE id=$1", id).Scan(&data, &etag, &revoked); err != nil {
		return reply{}, err
	}
	if err = match(r, etag); err != nil {
		return reply{}, err
	}
	if revoked != nil {
		return reply{}, fail(409, "api_key_revoked", "A revoked key cannot be edited.")
	}
	var merged map[string]json.RawMessage
	if err = json.Unmarshal(data, &merged); err != nil {
		return reply{}, err
	}
	for field, value := range patch {
		merged[field] = value
	}
	data, err = json.Marshal(merged)
	if err != nil {
		return reply{}, err
	}
	var input keyInput
	if err = json.Unmarshal(data, &input); err != nil {
		return reply{}, invalid("policy", "Invalid policy value.")
	}
	_, changed := patch["expires_at"]
	if err = validateKey(input, changed); err != nil {
		return reply{}, err
	}
	data, err = json.Marshal(input.KeyPolicy)
	if err != nil {
		return reply{}, err
	}
	etag = newID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET name=$1,policy=$2,expires_at=$3,etag=$4 WHERE id=$5", strings.TrimSpace(input.Name), data, input.ExpiresAt, etag, id); err != nil {
		return reply{}, err
	}
	generation, err := advanceAuthority(r, tx)
	if err != nil {
		return reply{}, err
	}
	if err = audit(r.Context(), tx, r, p.ID, "api_key.update", "api_key", id, "success"); err != nil {
		return reply{}, err
	}
	return commit(r, tx, detail(map[string]any{"etag": etag, "runtime_generation": generation}, etag))
}
func (s *Server) revokeAPIKey(r *http.Request) (reply, error) { return s.transitionKey(r, false) }
func (s *Server) rotateAPIKey(r *http.Request) (reply, error) { return s.transitionKey(r, true) }
func (s *Server) transitionKey(r *http.Request, rotate bool) (reply, error) {
	var input map[string]json.RawMessage
	if rotate && r.ContentLength != 0 {
		if err := decode(r, &input); err != nil {
			return reply{}, err
		}
		for field := range input {
			if field != "daily_cost_limit" && field != "monthly_cost_limit" {
				return reply{}, invalid(field, "Only cost budgets may accompany rotation.")
			}
		}
	}
	id, err := idParam(r, "api_key_id")
	if err != nil {
		return reply{}, err
	}
	tx, err := s.begin(r)
	if err != nil {
		return reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.principal(r, tx, "keys")
	if err != nil {
		return reply{}, err
	}
	claim, replayed, err := s.replay(r, tx, p, input)
	if err != nil {
		return reply{}, err
	}
	if replayed != nil {
		return commit(r, tx, *replayed)
	}
	var etag, name string
	var data []byte
	var revoked *time.Time
	if err = tx.QueryRow(r.Context(), "SELECT etag::text,name,policy,revoked_at FROM olp_go.api_keys WHERE id=$1", id).Scan(&etag, &name, &data, &revoked); err != nil {
		return reply{}, err
	}
	if err = match(r, etag); err != nil {
		return reply{}, err
	}
	if revoked != nil {
		return reply{}, fail(409, "api_key_revoked", "This key is already revoked.")
	}
	etag = newID()
	action := "api_key.revoke"
	var secret, lookup string
	if rotate {
		var policy map[string]json.RawMessage
		if err = json.Unmarshal(data, &policy); err != nil {
			return reply{}, err
		}
		for field, value := range input {
			policy[field] = value
		}
		data, err = json.Marshal(policy)
		if err != nil {
			return reply{}, err
		}
		var normalized KeyPolicy
		if err = json.Unmarshal(data, &normalized); err != nil {
			return reply{}, invalid("policy", "Invalid cost budget.")
		}
		if err = validateKey(keyInput{Name: name, KeyPolicy: normalized}, false); err != nil {
			return reply{}, err
		}
		data, err = json.Marshal(normalized)
		if err != nil {
			return reply{}, err
		}
		action = "api_key.rotate"
		lookup = secrets.Token()
		secret = "olp_" + lookup + "_" + secrets.Token()
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET lookup_id=$1,digest=$2,etag=$3,rotated_at=now(),policy=$4 WHERE id=$5", lookup, s.Auth.Digest("api_key", secret), etag, data, id)
	} else {
		_, err = tx.Exec(r.Context(), "UPDATE olp_go.api_keys SET revoked_at=now(),etag=$1 WHERE id=$2", etag, id)
	}
	if err != nil {
		return reply{}, err
	}
	generation, err := advanceAuthority(r, tx)
	if err != nil {
		return reply{}, err
	}
	result := detail(generation, etag)
	if rotate {
		result.Body = map[string]any{"id": id, "etag": etag, "lookup_id": lookup, "secret": secret, "runtime_generation": generation}
	}
	if err = audit(r.Context(), tx, r, p.ID, action, "api_key", id, "success"); err != nil {
		return reply{}, err
	}
	if err = s.completeReplay(r, tx, claim, result); err != nil {
		return reply{}, err
	}
	return commit(r, tx, result)
}

// Authority is durable input for M3's independent runtime refresh. Lookup never
// grants one positive scope through the other or applies unenforced limits.
type Authority struct {
	ID, Issuer           string
	Policy               KeyPolicy
	ExpiresAt, RevokedAt *time.Time
}

func (s *Server) LookupAuthority(ctx context.Context, secret string) (Authority, error) {
	var a Authority
	parts := strings.Split(secret, "_")
	if len(parts) != 3 || parts[0] != "olp" {
		return a, errors.New("invalid API key")
	}
	var digest, data []byte
	err := s.Pool.QueryRow(ctx, "SELECT id::text,created_by::text,digest,policy,expires_at,revoked_at FROM olp_go.api_keys WHERE lookup_id=$1", parts[1]).Scan(&a.ID, &a.Issuer, &digest, &data, &a.ExpiresAt, &a.RevokedAt)
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
