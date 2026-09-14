package routes

import (
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/management"
	"github.com/tyk-swe/olp/internal/runtime"
)

const policyETag = "00000000-0000-0000-0000-000000000000"

// defaultPolicy is the only routing policy this release applies: weighted
// rendezvous selection without constraints. Policy editing arrives in M5.
func defaultPolicy() map[string]any {
	constraints := map[string]any{"deny_data_collection": false, "ignore": []string{}, "max_price": nil, "only": nil, "quantizations": nil, "regions": nil, "require_parameters": false, "require_zero_data_retention": false}
	// The contract composes RoutingPreferences from the closed RoutingConstraints
	// schema, so defaults carry only constraint fields; the preference fields
	// (strategy, order, allow_fallbacks, latency and throughput targets) are
	// optional and stay unset until M5 introduces routing strategies.
	return map[string]any{"allowed_strategies": nil, "constraints": constraints, "defaults": maps.Clone(constraints)}
}

func (s *Server) policy(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	switch r.PathValue("scope") {
	case "installation", "route-draft", "api-key":
	default:
		return access.Reply{}, access.Fail(404, "not_found", "Unknown policy scope.")
	}
	return access.Detail(map[string]any{"policy": defaultPolicy(), "etag": policyETag}, policyETag), nil
}

type simulateDraftRequest struct {
	Operation   string       `json:"operation"`
	Surface     string       `json:"surface"`
	Mode        string       `json:"mode"`
	Seed        string       `json:"seed"`
	Preferences *Preferences `json:"preferences"`
}

func validTuple(operation, surface, mode string) error {
	if !slices.Contains(supportedOperations, operation) {
		return access.Fail(422, "operation_unavailable", "The "+operation+" operation is not available in this release.")
	}
	if surface != "openai" {
		return access.Fail(422, "surface_unavailable", "Only the openai surface is available in this release.")
	}
	if mode != "unary" && mode != "streaming" {
		return access.Fail(422, "mode_unavailable", "Use the unary or streaming mode.")
	}
	return nil
}

type candidate struct {
	target   runtime.PublishedTarget
	live     *resolved
	reason   string
	score    float64
	attempt  int
	eligible bool
}

// rank orders eligible targets the way the gateway will: priority ascending,
// rendezvous score descending, and assigns attempt numbers within the budget.
func rank(routingID, slug, keyID string, targets []runtime.PublishedTarget, live map[string]*resolved, operation, surface, mode string, seed []byte, budget int) []*candidate {
	routeID := uuid.MustParse(routingID)
	out := make([]*candidate, 0, len(targets))
	for _, t := range targets {
		c := &candidate{target: t, live: live[t.ProviderModelID]}
		switch {
		case c.live == nil:
			c.reason = "target_unknown"
		case c.live.ProviderState != "active":
			c.reason = "provider_not_active"
		case !c.live.Published:
			c.reason = "model_not_published"
		case !c.live.Certified[operation+"/"+surface+"/"+mode]:
			c.reason = "capability_not_certified"
		case !c.live.hasCredential(slug, keyID):
			c.reason = "no_eligible_credentials"
		default:
			c.eligible = true
			c.score = runtime.Score(routeID, uuid.MustParse(t.ProviderModelID), t.Weight, operation, surface, mode, seed)
		}
		out = append(out, c)
	}
	order := slices.Clone(out)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := order[i], order[j]
		if a.eligible != b.eligible {
			return a.eligible
		}
		if a.target.Priority != b.target.Priority {
			return a.target.Priority < b.target.Priority
		}
		return a.score > b.score
	})
	attempt := 0
	for _, c := range order {
		if !c.eligible {
			continue
		}
		attempt++
		if attempt <= budget {
			c.attempt = attempt
		} else {
			c.eligible, c.reason = false, "attempt_budget_exhausted"
		}
	}
	return order
}

func (r *resolved) hasCredential(slug, keyID string) bool {
	for _, slot := range r.Slots {
		if slot.Allows(r.ProviderModel, slug, keyID) && (r.AuthMode == "none" || slot.CredentialID != nil) {
			return true
		}
	}
	return false
}

func (c *candidate) decision() map[string]any {
	var attempt any
	if c.attempt > 0 {
		attempt = c.attempt
	}
	var reason any
	if c.reason != "" {
		reason = c.reason
	}
	providerID := c.target.ProviderID
	model := c.target.ProviderModel
	if c.live != nil {
		providerID, model = c.live.ProviderID, c.live.ProviderModel
	}
	return map[string]any{"target_id": c.target.ID, "provider_id": providerID, "upstream_model": model, "eligible": c.eligible, "priority": c.target.Priority, "strategy": "weighted", "attempt": attempt, "credential_slot_id": nil, "reason": reason, "price": nil, "performance": nil, "vendor_id": nil, "metadata_observed_at": nil}
}

func (s *Server) simulateDraft(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	id, err := access.IDParam(r, "draft_id")
	if err != nil {
		return access.Reply{}, err
	}
	var input simulateDraftRequest
	if err = access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if err = validTuple(input.Operation, input.Surface, input.Mode); err != nil {
		return access.Reply{}, err
	}
	if err = input.Preferences.Validate("preferences"); err != nil {
		return access.Reply{}, err
	}
	if len(input.Seed) > 256 {
		return access.Reply{}, access.Invalid("seed", "Use at most 256 characters.")
	}
	d, err := loadDraft(r.Context(), s.Access.Pool, id, false)
	if err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), s.Access.Pool, d.Targets)
	if err != nil {
		return access.Reply{}, err
	}
	routingID := d.ID
	if err = s.Access.Pool.QueryRow(r.Context(), "SELECT id::text FROM olp_go.routes WHERE slug=$1", d.Slug).Scan(&routingID); err != nil && !isNoRows(err) {
		return access.Reply{}, err
	}
	targets := []map[string]any{}
	for _, c := range rank(routingID, d.Slug, "", d.Targets, live, input.Operation, input.Surface, input.Mode, []byte(input.Seed), input.Preferences.Budget(d.MaxAttempts)) {
		decision := c.decision()
		var attempt any
		if c.attempt > 0 {
			attempt = c.attempt
		}
		targets = append(targets, map[string]any{"target_id": c.target.ID, "provider_id": decision["provider_id"], "provider_name": providerName(c), "provider_model": decision["upstream_model"], "priority": c.target.Priority, "eligible": c.eligible, "attempt": attempt, "reason": decision["reason"], "decision": decision})
	}
	return access.OK(map[string]any{"deterministic_seed": input.Seed, "operation": input.Operation, "surface": input.Surface, "mode": input.Mode, "targets": targets}), nil
}

func providerName(c *candidate) string {
	if c.live != nil {
		return c.live.ProviderName
	}
	return c.target.ProviderName
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

type simulationRequest struct {
	Operation   map[string]json.RawMessage `json:"operation"`
	Surface     string                     `json:"surface"`
	Mode        string                     `json:"mode"`
	Preferences *Preferences               `json:"preferences"`
	APIKeyID    *string                    `json:"api_key_id"`
	Seed        string                     `json:"seed"`
}

// simulateRouting answers the console's routing simulator against the routes
// as currently published.
func (s *Server) simulateRouting(r *http.Request) (access.Reply, error) {
	if _, err := s.Access.Principal(r, s.Access.Pool, "read"); err != nil {
		return access.Reply{}, err
	}
	var input simulationRequest
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	operation := "generation"
	if raw, ok := input.Operation["operation"]; ok {
		if err := json.Unmarshal(raw, &operation); err != nil {
			return access.Reply{}, access.Invalid("operation.operation", "Use an operation name.")
		}
	}
	if err := validTuple(operation, input.Surface, input.Mode); err != nil {
		return access.Reply{}, err
	}
	if err := input.Preferences.Validate("preferences"); err != nil {
		return access.Reply{}, err
	}
	if len(input.Seed) > 256 {
		return access.Reply{}, access.Invalid("seed", "Use at most 256 characters.")
	}
	var request struct {
		Route string `json:"route"`
		Model string `json:"model"`
	}
	if raw, ok := input.Operation["request"]; ok {
		if err := json.Unmarshal(raw, &request); err != nil {
			return access.Reply{}, access.Invalid("operation.request", "Use a request object.")
		}
	}
	slug := request.Route
	if slug == "" {
		slug = request.Model
	}
	if !access.RouteSlug.MatchString(slug) {
		return access.Reply{}, access.Invalid("operation.request.route", "Name the route to simulate.")
	}
	var keyReason string
	var keyID string
	if input.APIKeyID != nil {
		var err error
		keyID, err = access.ParseUUID(*input.APIKeyID)
		if err != nil {
			return access.Reply{}, access.Invalid("api_key_id", "Use an API key identifier.")
		}
		var authority access.Authority
		var raw []byte
		if err = s.Access.Pool.QueryRow(r.Context(), "SELECT policy,expires_at,revoked_at FROM olp_go.api_keys WHERE id=$1", keyID).Scan(&raw, &authority.ExpiresAt, &authority.RevokedAt); err != nil {
			return access.Reply{}, err
		}
		if err = json.Unmarshal(raw, &authority.Policy); err != nil {
			return access.Reply{}, err
		}
		if !authority.Allows("inference", slug, time.Now()) {
			keyReason = "api_key_not_authorized"
			if len(authority.Policy.AllowedRoutes) > 0 && !slices.Contains(authority.Policy.AllowedRoutes, slug) {
				keyReason = "route_not_allowed_for_key"
			}
		}
	}
	tx, err := s.Access.Pool.Begin(r.Context())
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	var routeID string
	var targets []byte
	var budget int
	if err = tx.QueryRow(r.Context(), "SELECT r.id::text,v.targets,v.max_attempts FROM olp_go.routes r JOIN olp_go.route_revisions v ON v.id=r.latest_revision_id WHERE r.slug=$1", slug).Scan(&routeID, &targets, &budget); err != nil {
		return access.Reply{}, err
	}
	var published []runtime.PublishedTarget
	if err = json.Unmarshal(targets, &published); err != nil {
		return access.Reply{}, err
	}
	live, err := resolve(r.Context(), tx, published)
	if err != nil {
		return access.Reply{}, err
	}
	decisions := []map[string]any{}
	for _, c := range rank(routeID, slug, keyID, published, live, operation, input.Surface, input.Mode, []byte(input.Seed), input.Preferences.Budget(budget)) {
		if keyReason != "" {
			c.eligible, c.attempt, c.reason = false, 0, keyReason
		}
		decisions = append(decisions, c.decision())
	}
	return access.OK(decisions), nil
}

// Register mounts the route surface.
func (s *Server) Register(mux *http.ServeMux) {
	h := s.Access.Handle
	mux.HandleFunc("GET /api/v3/route-drafts", h(s.drafts))
	mux.HandleFunc("POST /api/v3/route-drafts", h(s.createDraft))
	mux.HandleFunc("GET /api/v3/route-drafts/{draft_id}", h(s.draft))
	mux.HandleFunc("PUT /api/v3/route-drafts/{draft_id}", h(s.replaceDraft))
	mux.HandleFunc("DELETE /api/v3/route-drafts/{draft_id}", h(s.deleteDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/validate", h(s.validateDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/activate", h(s.activateDraft))
	mux.HandleFunc("POST /api/v3/route-drafts/{draft_id}/simulate", h(s.simulateDraft))
	mux.HandleFunc("GET /api/v3/routes", h(s.routes))
	mux.HandleFunc("GET /api/v3/routes/{route_id}", h(s.route))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions", h(s.revisions))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions/diff", h(s.revisionDiff))
	mux.HandleFunc("GET /api/v3/routes/{route_id}/revisions/{revision_id}", h(s.revision))
	mux.HandleFunc("POST /api/v3/routes/{route_id}/revisions/{revision_id}/restore-as-draft", h(s.restoreRevision))
	mux.HandleFunc("GET /api/v3/routing-policies/{scope}/{id}", h(s.policy))
	mux.HandleFunc("PUT /api/v3/routing-policies/{scope}/{id}", management.Unimplemented)
	mux.HandleFunc("POST /api/v3/routing/simulate", s.Access.HandleWith(1<<20, s.simulateRouting))
}
