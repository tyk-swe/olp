//go:build service

package service_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestCodeAdmissionRecordsChosenAndPinnedDecisions(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	healthyCredential, coolingCredential := uuid.NewString(), uuid.NewString()
	healthy, cooling := uuid.NewString(), uuid.NewString()
	key, poolID, routeID, revisionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "decision-journey", PoolID: poolID, Models: []string{"model"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription"}}`
	observed := time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)
	allowance := `{"windows":[{"limit_id":"codex","window":"primary","unit":"percent","evidence":"passive","used_percent":25,"remaining_percent":75,"window_minutes":300,"resets_at":null,"observed_at":"2026-11-05T10:00:00Z"}],"remaining_tokens":null,"remaining_requests":null,"remaining_percent":75,"resets_at":null,"observed_at":"2026-11-05T10:00:00Z"}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'journey@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'decision-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'decision-journey', 'plugin', 'active', '{}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 1, 'healthy-principal', repeat('0',64), '{}')`, []any{healthyCredential, provider}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 2, 'cooling-principal', repeat('0',64), '{}')`, []any{coolingCredential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{healthyCredential}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{coolingCredential}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, allowance, etag, created_by) VALUES($1, $2, $3, $4, 'healthy-principal', 'healthy', '["model"]', $5, $6, $7)`, []any{healthy, project, provider, healthyCredential, allowance, uuid.NewString(), user}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, health, unavailable_until, etag, created_by) VALUES($1, $2, $3, $4, 'cooling-principal', 'cooling', '["model"]', 'unavailable', now()+interval '1 hour', $5, $6)`, []any{cooling, project, provider, coolingCredential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'journey-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'decision-journey', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, healthy}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, cooling}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'decision-journey', '{}', $3, $4)`, []any{routeID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, routeID, document, connections, user}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{routeID, revisionID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := &resources.CodeStore{Pool: pool}
	admission := resources.CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "journey-conversation"}}, Providers: []string{provider}}
	permit, err := store.Admit(t.Context(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != healthy {
		t.Fatalf("first admission chose %s, want the available account %s", permit.Account.ID, healthy)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Reason != codemode.DecisionChosen || decision.ChosenAccountID != healthy || decision.Band != nil ||
		decision.Evidence == nil || *decision.Evidence != codemode.AllowanceEvidencePassive ||
		decision.EvidenceObservedAt == nil || !decision.EvidenceObservedAt.Equal(observed) ||
		len(decision.Skipped) != 1 || decision.Skipped[0].AccountID != cooling || decision.Skipped[0].Class != codemode.SkipCooldown ||
		decision.RecoveryEnvelope != nil || decision.RecoveryOrdinal != nil {
		t.Fatalf("first decision = %+v", decision)
	}
	second, err := store.Admit(t.Context(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if second.Account.ID != healthy {
		t.Fatalf("pinned turn moved to %s", second.Account.ID)
	}
	pinned := codeDecisionForAttempt(t, pool, second.Attempt.ID)
	if pinned.Reason != codemode.DecisionPin || pinned.ChosenAccountID != healthy || len(pinned.Skipped) != 0 {
		t.Fatalf("pinned decision = %+v", pinned)
	}
	bound, err := store.BindConnection(t.Context(), route, key, codemode.Identity{Conversation: "connection-conversation"}, "model", []string{provider})
	if err != nil {
		t.Fatal(err)
	}
	if bound.Account.ID != healthy {
		t.Fatalf("connection bound to %s, want the available account %s", bound.Account.ID, healthy)
	}
	connection := codeConnectionDecisions(t, pool, bound.Binding.RootID)
	if len(connection) != 1 || connection[0].AttemptID != nil || connection[0].Reason != codemode.DecisionConnection ||
		connection[0].ChosenAccountID != healthy || connection[0].BindingID != bound.Binding.ID || connection[0].RootID != bound.Binding.RootID ||
		connection[0].Evidence == nil || *connection[0].Evidence != codemode.AllowanceEvidencePassive ||
		len(connection[0].Skipped) != 1 || connection[0].Skipped[0].AccountID != cooling || connection[0].Skipped[0].Class != codemode.SkipCooldown {
		t.Fatalf("connection decision = %+v", connection)
	}
	rebound, err := store.BindConnection(t.Context(), route, key, codemode.Identity{Conversation: "connection-conversation"}, "model", []string{provider})
	if err != nil {
		t.Fatal(err)
	}
	if rebound.Binding.RootID != bound.Binding.RootID || rebound.Account.ID != healthy {
		t.Fatalf("reconnected permit = %+v", rebound)
	}
	recordedConnections := codeConnectionDecisions(t, pool, bound.Binding.RootID)
	emptySkipped := 0
	for _, recorded := range recordedConnections {
		if recorded.AttemptID != nil || recorded.Reason != codemode.DecisionConnection || recorded.ChosenAccountID != healthy {
			t.Fatalf("connection decisions = %+v", recordedConnections)
		}
		if len(recorded.Skipped) == 0 {
			emptySkipped++
		}
	}
	if len(recordedConnections) != 2 || emptySkipped != 1 {
		t.Fatalf("connection decisions = %+v", recordedConnections)
	}
}

func codeConnectionDecisions(t *testing.T, pool *pgxpool.Pool, rootID string) []codemode.Decision {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT to_jsonb(x) FROM olp.code_decisions x WHERE x.root_id=$1 AND x.reason='connection' AND x.attempt_id IS NULL ORDER BY x.created_at,x.id`, rootID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	decisions := []codemode.Decision{}
	for rows.Next() {
		var document []byte
		if err := rows.Scan(&document); err != nil {
			t.Fatal(err)
		}
		var decision codemode.Decision
		if err := json.Unmarshal(document, &decision); err != nil {
			t.Fatal(err)
		}
		decisions = append(decisions, decision)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return decisions
}

func codeDecisionForAttempt(t *testing.T, pool *pgxpool.Pool, attemptID string) codemode.Decision {
	t.Helper()
	var document []byte
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(x) FROM olp.code_decisions x WHERE x.attempt_id=$1`, attemptID).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var decision codemode.Decision
	if err := json.Unmarshal(document, &decision); err != nil {
		t.Fatal(err)
	}
	return decision
}
