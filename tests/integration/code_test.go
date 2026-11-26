//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestCodeDecisionJourneys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	t.Run("chosen and pinned", func(t *testing.T) { codeAdmissionRecordsChosenAndPinnedDecisions(t, pool) })
	t.Run("weighted selection and availability", func(t *testing.T) { codeSelectionWeighsRootsAndHonorsAvailability(t, pool) })
}

func codeAdmissionRecordsChosenAndPinnedDecisions(t *testing.T, pool *pgxpool.Pool) {
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
	store := codesessions.New(pool)
	admission := codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "journey-conversation"}}, Providers: []string{provider}}
	permit, err := store.Admit(t.Context(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != healthy {
		t.Fatalf("first admission chose %s, want the available account %s", permit.Account.ID, healthy)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Reason != codemode.DecisionChosen || decision.ChosenAccountID != healthy ||
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
	generation, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "connection-conversation"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	pinnedReconnect, err := store.BindConnection(t.Context(), route, key, codemode.Identity{Conversation: "connection-conversation"}, "model", []string{provider})
	if err != nil {
		t.Fatal(err)
	}
	if pinnedReconnect.Binding.RootID != bound.Binding.RootID || pinnedReconnect.Account.ID != generation.Pin.AccountID {
		t.Fatalf("pinned reconnect permit = %+v, want pinned account %s", pinnedReconnect, generation.Pin.AccountID)
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
	if len(recordedConnections) != 3 || emptySkipped != 2 {
		t.Fatalf("connection decisions = %+v", recordedConnections)
	}
	// Vendor object shape per https://opencode.ai/v2/docs/console/api/inference/ (fetched 2026-11-05).
	reported := json.RawMessage(`{"billing_source":"go","list_cost":0.00412,"allowance":0.0021,"tokens":{"input":1200,"output":300},"cost":0.00412}`)
	if err := store.ObserveAllowanceCharge(t.Context(), second.Attempt.ID, reported); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveAllowanceCharge(t.Context(), second.Attempt.ID, json.RawMessage(`"go"`)); err == nil {
		t.Fatal("non-object allowance charge stored, want an error")
	}
	stored := attemptAllowanceCharge(t, pool, second.Attempt.ID)
	if stored == nil || stored["billing_source"] != "go" || stored["allowance"] != 0.0021 || stored["cost"] != 0.00412 {
		t.Fatalf("stored allowance charge = %v, want the reported vendor object", stored)
	}
	if missing := attemptAllowanceCharge(t, pool, permit.Attempt.ID); missing != nil {
		t.Fatalf("attempt without a reported charge = %v, want null (unknown)", missing)
	}
	third, err := store.Admit(t.Context(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if third.Account.ID != healthy {
		t.Fatalf("admission with a charge recorded chose %s, want the pinned account %s", third.Account.ID, healthy)
	}
	if missing := attemptAllowanceCharge(t, pool, third.Attempt.ID); missing != nil {
		t.Fatalf("fresh attempt charge = %v, want null (unknown)", missing)
	}
}

func percentAllowanceJSON(t *testing.T, remaining float64, resetsAt *time.Time, observedAt time.Time) []byte {
	t.Helper()
	document, err := json.Marshal(codemode.Allowance{
		Windows:          []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: resetsAt, ObservedAt: observedAt}},
		RemainingPercent: &remaining,
		ResetsAt:         resetsAt,
		ObservedAt:       observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func codeSelectionWeighsRootsAndHonorsAvailability(t *testing.T, pool *pgxpool.Pool) {
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	key := "11111111-1111-4111-8111-111111111111"
	healthy := "a1111111-1111-4111-8111-111111111111"
	low := "b2222222-2222-4222-8222-222222222222"
	unknown := "c3333333-3333-4333-8333-333333333333"
	shareA := "f6666666-6666-4666-8666-666666666666"
	shareB := "a7777777-7777-4777-8777-777777777777"
	base := time.Now().UTC().Truncate(time.Second)
	reset := base.Add(2 * time.Hour)
	observed := base.Add(-time.Minute)
	accounts := []struct {
		id        string
		principal string
		allowance []byte
	}{
		{healthy, "select-healthy", percentAllowanceJSON(t, 75, &reset, observed)},
		{low, "select-low", percentAllowanceJSON(t, 5, &reset, observed)},
		{unknown, "select-unknown", nil},
		{shareA, "select-share-a", percentAllowanceJSON(t, 75, &reset, observed)},
		{shareB, "select-share-b", percentAllowanceJSON(t, 60, &reset, observed)},
	}
	poolMain, poolShare := uuid.NewString(), uuid.NewString()
	routes := []struct {
		id     string
		slug   string
		poolID string
	}{
		{"22222222-2222-4222-8222-222222222222", "select-weighted", poolMain},
		{"55555555-5555-4555-8555-555555555555", "select-share", poolShare},
	}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription"}}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'select@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'select-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'select-journey', 'plugin', 'active', '{}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'select-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
	}
	for index, account := range accounts {
		credential := uuid.NewString()
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts, profile_id) VALUES($1, $2, $3, $4, repeat('0',64), '{}', 'codex-subscription')`, []any{credential, provider, index + 1, account.principal}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, allowance, etag, created_by) VALUES($1, $2, $3, $4, $5, $6, '["model"]', $7, $8, $9)`, []any{account.id, project, provider, credential, account.principal, account.principal, account.allowance, uuid.NewString(), user}},
		)
	}
	for _, membership := range []struct {
		poolID  string
		name    string
		members map[string]int64
	}{
		{poolMain, "select-main", map[string]int64{healthy: 1, low: 1, unknown: 1}},
		{poolShare, "select-share", map[string]int64{shareA: 3, shareB: 1}},
	} {
		statements = append(statements, struct {
			sql  string
			args []any
		}{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, $3, 'shared', $4, $5)`, []any{membership.poolID, project, membership.name, uuid.NewString(), user}})
		for accountID, weight := range membership.members {
			statements = append(statements, struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, $3)`, []any{membership.poolID, accountID, weight}})
		}
		statements = append(statements, struct {
			sql  string
			args []any
		}{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{membership.poolID, key}})
	}
	bySlug := map[string]codemode.Route{}
	for _, seeded := range routes {
		revisionID := uuid.NewString()
		route := codemode.Route{ID: seeded.id, ProjectID: project, Slug: seeded.slug, PoolID: seeded.poolID, Models: []string{"model"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
		bySlug[seeded.slug] = route
		document, err := json.Marshal(route)
		if err != nil {
			t.Fatal(err)
		}
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, $3, '{}', $4, $5)`, []any{seeded.id, project, seeded.slug, uuid.NewString(), user}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, seeded.id, document, connections, user}},
			struct {
				sql  string
				args []any
			}{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{seeded.id, revisionID}},
		)
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	store := codesessions.New(pool)
	admit := func(route codemode.Route, conversation string) codesessions.Permit {
		t.Helper()
		permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{provider}})
		if err != nil {
			t.Fatal(err)
		}
		return permit
	}
	skippedClass := func(decision codemode.Decision, accountID string) codemode.SkipClass {
		t.Helper()
		for _, skipped := range decision.Skipped {
			if skipped.AccountID == accountID {
				return skipped.Class
			}
		}
		t.Fatalf("decision %+v does not skip %s", decision, accountID)
		return ""
	}

	chosen := admit(bySlug["select-weighted"], "select-conv-0")
	if chosen.Account.ID != low {
		t.Fatalf("selection chose %s, want the weighted score winner %s", chosen.Account.ID, low)
	}
	decision := codeDecisionForAttempt(t, pool, chosen.Attempt.ID)
	if decision.Reason != codemode.DecisionChosen || skippedClass(decision, healthy) != codemode.SkipWeight || skippedClass(decision, unknown) != codemode.SkipWeight {
		t.Fatalf("chosen decision = %+v", decision)
	}

	wins := map[string]int{}
	for index := range 100 {
		wins[admit(bySlug["select-share"], fmt.Sprintf("select-share-%d", index)).Account.ID]++
	}
	if wins[shareA]+wins[shareB] != 100 || wins[shareB] < 10 || wins[shareA] < 2*wins[shareB] {
		t.Fatalf("weights must share roots, got %v", wins)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, percentAllowanceJSON(t, 5, &reset, base)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, low, percentAllowanceJSON(t, 80, &reset, base)); err != nil {
		t.Fatal(err)
	}
	pinned := admit(bySlug["select-weighted"], "select-conv-0")
	if pinned.Account.ID != low {
		t.Fatalf("pinned turn moved to %s after allowances flipped", pinned.Account.ID)
	}
	if decision = codeDecisionForAttempt(t, pool, pinned.Attempt.ID); decision.Reason != codemode.DecisionPin {
		t.Fatalf("pinned decision = %+v", decision)
	}
	other := seedCooldownJourney(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1) WHERE project_id=$2`, low, project); err != nil {
		t.Fatal(err)
	}
	if _, err := other.store.Admit(t.Context(), other.admission("model", "other-project-first")); err != nil {
		t.Fatalf("other project admission after pinned selection: %v", err)
	}
	for _, tc := range []struct {
		name      string
		allowance codemode.Allowance
		retryAt   *time.Time
	}{
		{"exhausted window", codemode.Allowance{Windows: []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100, RemainingPercent: 0, ResetsAt: &reset, ObservedAt: base}}, ObservedAt: base}, &reset},
		{"stale resetless count", codemode.Allowance{RemainingTokens: new(int64(0)), ObservedAt: base.Add(-time.Hour)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, err := json.Marshal(tc.allowance)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, low, document); err != nil {
				t.Fatal(err)
			}
			for _, conversation := range []string{uuid.NewString(), "select-conv-0"} {
				_, err = store.Admit(t.Context(), codesessions.Admission{Route: bySlug["select-weighted"], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{provider}})
				var refusal *codemode.Refusal
				if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || (tc.retryAt == nil && refusal.RetryAt != nil) || (tc.retryAt != nil && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(*tc.retryAt))) {
					t.Fatalf("admission for %s = %v (%+v), want refusal with retry at %v", conversation, err, refusal, tc.retryAt)
				}
			}
		})
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1) WHERE project_id=$2`, healthy, project); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET connections=jsonb_set(connections,ARRAY[$1,'profile_revision'],to_jsonb(repeat('0',64)),true) WHERE route_id IN (SELECT id FROM olp.code_routes WHERE project_id=$2)`, provider, project); err != nil {
		t.Fatal(err)
	}
	if _, err := other.store.Admit(t.Context(), other.admission("model", "other-project-second")); err != nil {
		t.Fatalf("other project admission after root selection: %v", err)
	}
	window := func(family string, unit codemode.AllowanceUnit, evidence codemode.AllowanceEvidence, remainingPercent float64, remainingValue *float64, observation time.Time, resetAt *time.Time) codemode.AllowanceWindow {
		usedPercent := func() float64 {
			if unit == codemode.AllowanceUnitPercent {
				return 100 - remainingPercent
			}
			return 0
		}()
		return codemode.AllowanceWindow{LimitID: family, Window: "primary", Unit: unit, Evidence: evidence, UsedPercent: usedPercent, RemainingPercent: remainingPercent, RemainingValue: remainingValue, ObservedAt: observation, ResetsAt: resetAt}
	}
	cases := []struct {
		name      string
		allowance codemode.Allowance
		available bool
	}{
		{"old active mirror", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidenceActive, 0, nil, observed, &reset)}, RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, true},
		{"legacy top-level exhausted", codemode.Allowance{RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, false},
		{"positive count", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCount, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true},
		{"positive credits", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCredits, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true},
		{"positive cents", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCents, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true},
		{"fresh exhausted", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, false},
		{"stale no reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), nil)}, ObservedAt: base.Add(-time.Hour)}, false},
		{"stale future reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), &reset)}, ObservedAt: base.Add(-time.Hour)}, false},
		{"stale token count", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: base.Add(-time.Hour), ResetsAt: &reset}, ObservedAt: observed}, false},
		{"fresh request count", codemode.Allowance{RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &reset}, ObservedAt: observed}, false},
		{"unrelated family", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("special_model", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			document, err := json.Marshal(tc.allowance)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, document); err != nil {
				t.Fatal(err)
			}
			route := bySlug["select-weighted"]
			permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
			if tc.available && (err != nil || permit.Account.ID != healthy) {
				t.Fatalf("admission = %+v, %v", permit, err)
			}
			if !tc.available && err == nil {
				t.Fatalf("admitted exhausted account %+v", permit)
			}
			offers, err := codesessions.Offers(t.Context(), pool, route.RevisionID)
			if err != nil || (len(offers) > 0) != tc.available {
				t.Fatalf("offers = %+v, %v; available %t", offers, err, tc.available)
			}
		})
	}
	countReset := base.Add(30 * time.Minute)
	activeReset := base.Add(4 * time.Hour)
	blocked := codemode.Allowance{
		RemainingTokens:  new(int64(0)),
		TokenObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &countReset},
		ObservedAt:       observed,
		Windows: []codemode.AllowanceWindow{
			window("special_model", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset),
			window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidenceActive, 0, nil, observed, &activeReset),
		},
	}
	blockedJSON, err := json.Marshal(blocked)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, blockedJSON); err != nil {
		t.Fatal(err)
	}
	_, err = store.Admit(t.Context(), codesessions.Admission{Route: bySlug["select-weighted"], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
	if refusal := (*codemode.Refusal)(nil); !errors.As(err, &refusal) || refusal.RetryAt == nil || !refusal.RetryAt.Equal(reset) {
		t.Fatalf("refusal = %+v, want retry at %s", err, reset)
	}
	for _, tc := range []struct {
		name      string
		allowance codemode.Allowance
	}{
		{"token freshness", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: observed}, ObservedAt: observed}},
		{"request freshness", codemode.Allowance{RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed}, ObservedAt: observed}},
		{"legacy freshness", codemode.Allowance{RemainingTokens: new(int64(0)), ObservedAt: observed}},
		{"latest freshness", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: observed.Add(-time.Minute)}, RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed}, ObservedAt: observed}},
		{"later vendor reset", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: observed}, RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &countReset}, ObservedAt: observed}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			document, err := json.Marshal(tc.allowance)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, document); err != nil {
				t.Fatal(err)
			}
			_, err = store.Admit(t.Context(), codesessions.Admission{Route: bySlug["select-weighted"], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
			if refusal := (*codemode.Refusal)(nil); !errors.As(err, &refusal) || refusal.RetryAt != nil {
				t.Fatalf("refusal = %+v, want no retry time", err)
			}
		})
	}
	if err := store.ObserveCooldownClass(t.Context(), healthy, "model", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeModel, RetryAt: countReset, Envelope: "fixture_rate"}, observed); err != nil {
		t.Fatal(err)
	}
	for _, allowance := range []codemode.Allowance{
		{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, nil)}, ObservedAt: observed},
		{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: base.Add(-time.Hour)}, ObservedAt: base.Add(-time.Hour)},
	} {
		withoutReset, err := json.Marshal(allowance)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, withoutReset); err != nil {
			t.Fatal(err)
		}
		_, err = store.Admit(t.Context(), codesessions.Admission{Route: bySlug["select-weighted"], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
		if refusal := (*codemode.Refusal)(nil); !errors.As(err, &refusal) || refusal.RetryAt != nil {
			t.Fatalf("resetless refusal = %+v, want no retry time", err)
		}
	}

}

func attemptAllowanceCharge(t *testing.T, pool *pgxpool.Pool, attemptID string) map[string]any {
	t.Helper()
	var document []byte
	if err := pool.QueryRow(t.Context(), `SELECT allowance_charge FROM olp.code_attempts WHERE id=$1`, attemptID).Scan(&document); err != nil {
		t.Fatal(err)
	}
	if document == nil {
		return nil
	}
	var charge map[string]any
	if err := json.Unmarshal(document, &charge); err != nil {
		t.Fatal(err)
	}
	return charge
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

type cooldownJourney struct {
	store    *codesessions.Store
	route    codemode.Route
	key      string
	provider string
	first    string
	second   string
}

func seedCooldownJourney(t *testing.T, pool *pgxpool.Pool) cooldownJourney {
	t.Helper()
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	firstCredential, secondCredential := uuid.NewString(), uuid.NewString()
	first, second := uuid.NewString(), uuid.NewString()
	key, poolID, routeID, revisionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "cooldown-" + routeID, PoolID: poolID, Models: []string{"model", "other"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription","profile_revision":"` + strings.Repeat("0", 64) + `"}}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, $2, 'Journey', 'owner', $3)`, []any{user, user + "@example.invalid", uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, $2, $3, $4)`, []any{project, "cooldown-" + project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, $3, 'plugin', 'active', '{}', $4, $5, $6)`, []any{provider, project, "cooldown-" + provider, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts,profile_id) VALUES($1, $2, 1, 'first-principal', repeat('0',64), '{}','codex-subscription')`, []any{firstCredential, provider}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts,profile_id) VALUES($1, $2, 2, 'second-principal', repeat('0',64), '{}','codex-subscription')`, []any{secondCredential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{firstCredential}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{secondCredential}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'first-principal', 'first', '["model","other"]', $5, $6)`, []any{first, project, provider, firstCredential, uuid.NewString(), user}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'second-principal', 'second', '["model","other"]', $5, $6)`, []any{second, project, provider, secondCredential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, $2, '\x00', 'journey', $3, '{"scopes":["inference"]}', $4, $5)`, []any{key, "cooldown-" + key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'cooldown-journey', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, first}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, second}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, $3, '{}', $4, $5)`, []any{routeID, project, route.Slug, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, routeID, document, connections, user}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{routeID, revisionID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return cooldownJourney{store: codesessions.New(pool), route: route, key: key, provider: provider, first: first, second: second}
}

func (j cooldownJourney) admission(model, conversation string) codesessions.Admission {
	return codesessions.Admission{Route: j.route, APIKeyID: j.key, Operation: codemode.Operation{Name: "responses", Model: model, Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{j.provider}}
}

func secondCodeReplica(t *testing.T, pool *pgxpool.Pool, address string, j cooldownJourney) cooldownJourney {
	t.Helper()
	replica, err := pgxpool.New(t.Context(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(replica.Close)
	route := j.route
	route.ID, route.RevisionID, route.ETag = uuid.NewString(), uuid.NewString(), uuid.NewString()
	route.Slug = "replica-" + route.ID
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	key := uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id)
		SELECT $1,$2,digest,name,created_by,policy,$3,project_id FROM olp.api_keys WHERE id=$4`, []any{key, "replica-" + key, uuid.NewString(), j.key}},
		{`INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, []any{route.PoolID, key}},
		{`INSERT INTO olp.code_routes(id,project_id,slug,draft,etag,created_by)
		SELECT $1,project_id,$2,draft,$3,created_by FROM olp.code_routes WHERE id=$4`, []any{route.ID, route.Slug, route.ETag, j.route.ID}},
		{`INSERT INTO olp.code_route_revisions(id,route_id,revision,document,connections,created_by)
		SELECT $1,$2,1,$3,connections,created_by FROM olp.code_route_revisions WHERE id=$4`, []any{route.RevisionID, route.ID, document, j.route.RevisionID}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{route.ID, route.RevisionID}},
	} {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return cooldownJourney{store: codesessions.New(replica), route: route, key: key, provider: j.provider, first: j.first, second: j.second}
}

func TestCodeDecisionsKeepRecordsContentFree(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	user, project, provider, credential, account, other, key, route, revision, binding, attempt := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'decision@example.invalid', 'Decision', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'decision-migration', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, 'decision-migration', 'plugin', 'active', '{}', $2, $3, $4)`, []any{provider, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version) VALUES($1, $2, 1)`, []any{credential, provider}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'principal', 'account', '["model"]', $5, $6)`, []any{account, project, provider, credential, uuid.NewString(), user}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'other-principal', 'other', '["model"]', $5, $6)`, []any{other, project, provider, credential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'decision-lookup', '\x00', 'decision', $2, '{}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'decision-route', '{}', $3, $4)`, []any{route, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, '{}', '{}', $3)`, []any{revision, route, user}},
		{`INSERT INTO olp.code_bindings(id, project_id, route_id, api_key_id, conversation, root_id, account_id, principal) VALUES($1, $2, $3, $4, 'conversation', $1, $5, 'principal')`, []any{binding, project, route, key, account}},
		{`INSERT INTO olp.code_attempts(id, project_id, route_id, route_revision_id, api_key_id, binding_id, account_id, operation, model, state, reserved_tokens) VALUES($1, $2, $3, $4, $5, $6, $7, 'responses', 'model', 'prepared', 0)`, []any{attempt, project, route, revision, key, binding, account}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	insert := `INSERT INTO olp.code_decisions(id,attempt_id,project_id,route_id,route_revision_id,api_key_id,binding_id,root_id,chosen_account_id,reason,skipped,recovery_envelope,recovery_ordinal)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`
	skipped := `[{"account_id":"` + other + `","class":"cooldown"}]`
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "chosen", skipped, "codex_usage_limit_reached", 1); err != nil {
		t.Fatal(err)
	}
	var document []byte
	if err := pool.QueryRow(t.Context(), `SELECT to_jsonb(x) FROM olp.code_decisions x WHERE x.attempt_id=$1 AND x.chosen_account_id=$2`, attempt, account).Scan(&document); err != nil {
		t.Fatal(err)
	}
	var decision codemode.Decision
	if err := json.Unmarshal(document, &decision); err != nil {
		t.Fatal(err)
	}
	if decision.Reason != codemode.DecisionChosen ||
		len(decision.Skipped) != 1 || decision.Skipped[0].AccountID != other || decision.Skipped[0].Class != codemode.SkipCooldown ||
		decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 {
		t.Fatalf("decision projection = %s", document)
	}
	if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), attempt, project, route, revision, key, binding, binding, account, "pin", "[]", nil, nil); err == nil {
		t.Fatal("second decision for one attempt accepted")
	}
	for range 2 {
		if _, err := pool.Exec(t.Context(), insert, uuid.NewString(), nil, project, route, revision, key, binding, binding, account, "connection", "[]", nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	rejected := []struct {
		name string
		tail []any
	}{
		{"unknown reason", []any{"guessed", "[]", nil, nil}},
		{"free-form recovery envelope", []any{"chosen", "[]", "raw error body", nil}},
		{"skipped not an array", []any{"chosen", "{}", nil, nil}},
	}
	for _, rejectedInsert := range rejected {
		fresh := uuid.NewString()
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_attempts(id, project_id, route_id, route_revision_id, api_key_id, binding_id, account_id, operation, model, state, reserved_tokens) VALUES($1, $2, $3, $4, $5, $6, $7, 'responses', 'model', 'prepared', 0)`, fresh, project, route, revision, key, binding, account); err != nil {
			t.Fatal(err)
		}
		args := append([]any{uuid.NewString(), fresh, project, route, revision, key, binding, binding, account}, rejectedInsert.tail...)
		if _, err := pool.Exec(t.Context(), insert, args...); err == nil {
			t.Fatalf("%s accepted", rejectedInsert.name)
		}
	}
}

func TestCodeThreadStatusFromStoredEvidence(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	credential, account := uuid.NewString(), uuid.NewString()
	key, otherKey, poolID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	routeID, revisionID := uuid.NewString(), uuid.NewString()
	otherRouteID, otherRevisionID := uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "thread-status", PoolID: poolID, Models: []string{"model"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	otherRoute := codemode.Route{ID: otherRouteID, ProjectID: project, Slug: "thread-status-other", PoolID: poolID, Models: []string{"model"}, Enabled: true, RevisionID: otherRevisionID, Revision: 1, ETag: uuid.NewString()}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription"}}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'thread-status@example.invalid', 'Status', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'thread-status', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'thread-status', 'plugin', 'active', '{}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 1, 'status-principal', repeat('0',64), '{}')`, []any{credential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'status-principal', 'status', '["model"]', $5, $6)`, []any{account, project, provider, credential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'thread-status-lookup', '\x00', 'status', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'thread-status-other-lookup', '\x00', 'status-other', $2, '{"scopes":["inference"]}', $3, $4)`, []any{otherKey, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'thread-status', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, account}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, otherKey}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'thread-status', '{}', $3, $4)`, []any{routeID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'thread-status-other', '{}', $3, $4)`, []any{otherRouteID, project, uuid.NewString(), user}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, published := range []struct {
		routeID, revisionID string
		document            codemode.Route
	}{{routeID, revisionID, route}, {otherRouteID, otherRevisionID, otherRoute}} {
		document, err := json.Marshal(published.document)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, published.revisionID, published.routeID, document, connections, user); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(t.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, published.routeID, published.revisionID); err != nil {
			t.Fatal(err)
		}
	}
	store := codesessions.New(pool)
	identity := codemode.Identity{Conversation: "status-thread"}
	admit := func(id codemode.Identity) codesessions.Permit {
		t.Helper()
		permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: id}, Providers: []string{provider}})
		if err != nil {
			t.Fatal(err)
		}
		return permit
	}
	permit := admit(identity)
	child := admit(codemode.Identity{Conversation: "status-child", Parent: "status-thread"})
	attemptCount := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_attempts WHERE account_id=$1`, account).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	baseline := attemptCount()
	base := time.Now().UTC().Truncate(time.Second)
	retry := base.Add(5 * time.Minute)
	status := func(at time.Time, onRoute codemode.Route, withKey string, id codemode.Identity) (codemode.ThreadStatus, error) {
		t.Helper()
		return store.ThreadStatus(t.Context(), onRoute, withKey, id, at)
	}
	fresh, err := status(base, route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Status != codemode.ThreadUnknown || fresh.RetryAt != nil || fresh.LastObservedAt != nil {
		t.Fatalf("fresh thread status = %+v, want unknown without evidence", fresh)
	}
	if err := store.ObserveCooldownClass(t.Context(), account, "model", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeCredential, RetryAt: retry}, base); err != nil {
		t.Fatal(err)
	}
	cooling, err := status(base.Add(2*time.Minute), route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if cooling.Status != codemode.ThreadUnavailable || cooling.RetryAt == nil || !cooling.RetryAt.Equal(retry) ||
		cooling.LastObservedAt == nil || !cooling.LastObservedAt.Equal(base) || !cooling.CheckedAt.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("cooling status = %+v, want unavailable with retry %v observed %v", cooling, retry, base)
	}
	childStatus, err := status(base.Add(2*time.Minute), route, key, codemode.Identity{Conversation: child.Binding.Conversation, Parent: "status-thread"})
	if err != nil || childStatus.Status != codemode.ThreadUnavailable {
		t.Fatalf("child status = %+v, %v, want unavailable on the tree account", childStatus, err)
	}
	expired, err := status(base.Add(6*time.Minute), route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if expired.Status != codemode.ThreadUnknown || expired.RetryAt != nil ||
		expired.LastObservedAt == nil || !expired.LastObservedAt.Equal(base) || !expired.CheckedAt.Equal(base.Add(6*time.Minute)) {
		t.Fatalf("expired status = %+v, want unknown without a retry time", expired)
	}
	notFound := func(err error) *codemode.Refusal {
		t.Helper()
		refusal, ok := errors.AsType[*codemode.Refusal](err)
		if !ok || refusal.Status != 404 || refusal.Code != "code_thread_not_found" {
			t.Fatalf("status error = %v, want the missing-thread refusal", err)
		}
		return refusal
	}
	_, wrongKeyErr := status(base, route, otherKey, identity)
	_, wrongRouteErr := status(base, otherRoute, key, identity)
	_, wrongParentErr := status(base, route, key, codemode.Identity{Conversation: "status-child", Parent: "someone-else"})
	_, missingErr := status(base, route, key, codemode.Identity{Conversation: "never-bound"})
	if *notFound(wrongKeyErr) != *notFound(missingErr) || *notFound(wrongRouteErr) != *notFound(missingErr) || *notFound(wrongParentErr) != *notFound(missingErr) {
		t.Fatal("wrong key, route or lineage must look the same as a missing thread")
	}
	var bound int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_bindings WHERE route_id=$1 AND conversation='never-bound'`, route.ID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if polled := attemptCount(); bound != 0 || polled != baseline {
		t.Fatalf("polling bound %d conversations and left %d attempts, want no writes over %d", bound, polled, baseline)
	}
	recovered := base.Add(7 * time.Minute)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET health='healthy', health_observed_at=$2, unavailable_until=NULL WHERE id=$1`, account, recovered); err != nil {
		t.Fatal(err)
	}
	available, err := status(base.Add(8*time.Minute), route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if available.Status != codemode.ThreadAvailable || available.RetryAt != nil {
		t.Fatalf("recovered status = %+v, want available", available)
	}
	revised := route
	revised.Models = []string{"other-model"}
	revised.RevisionID, revised.Revision = uuid.NewString(), 2
	revisedDocument, err := json.Marshal(revised)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by)
		VALUES($1,$2,2,$3,$4,$5)`, revised.RevisionID, routeID, revisedDocument, connections, user); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, routeID, revised.RevisionID); err != nil {
		t.Fatal(err)
	}
	for _, installed := range []codemode.Route{route, revised} {
		removed, err := status(base.Add(8*time.Minute), installed, key, identity)
		if err != nil || removed.Status != codemode.ThreadUnavailable || removed.RetryAt != nil ||
			removed.LastObservedAt == nil || !removed.LastObservedAt.Equal(recovered) {
			t.Fatalf("revision %d without its published pinned model returned %+v, %v, want unavailable with last observation %v", installed.Revision, removed, err, recovered)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, routeID, revisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET connections='{}' WHERE id=$1`, revisionID); err != nil {
		t.Fatal(err)
	}
	unpublished, err := status(base.Add(8*time.Minute), route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if unpublished.Status != codemode.ThreadUnavailable || unpublished.RetryAt != nil {
		t.Fatalf("status without the pinned provider's connection = %+v, want unavailable", unpublished)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET connections=$2 WHERE id=$1`, revisionID, connections); err != nil {
		t.Fatal(err)
	}
	slot, paidBinding := uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_slots(id, provider_id, position, name, credential_id) VALUES($1, $2, 0, 'default', $3)`, slot, provider, credential); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_paid_bindings(id, project_id, route_id, api_key_id, conversation, root_id, model, selected_model, provider_id, slot_id, credential_id, subscription_root_id)
		VALUES($1, $2, $3, $4, 'status-paid', $1, 'model', 'model', $5, $6, $7, $8)`, paidBinding, project, routeID, key, provider, slot, credential, permit.Binding.RootID); err != nil {
		t.Fatal(err)
	}
	paid := codemode.Identity{Conversation: "status-paid"}
	paidLive, err := status(base.Add(8*time.Minute), route, key, paid)
	if err != nil || paidLive.Status != codemode.ThreadUnknown {
		t.Fatalf("paid status = %+v, %v, want unknown while its subscription tree is live", paidLive, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_bindings SET retired_at=now() WHERE id=$1`, permit.Binding.RootID); err != nil {
		t.Fatal(err)
	}
	paidRetired, err := status(base.Add(9*time.Minute), route, key, paid)
	if err != nil || paidRetired.Status != codemode.ThreadUnavailable {
		t.Fatalf("paid status = %+v, %v, want unavailable once its subscription tree retires", paidRetired, err)
	}
	retired, err := status(base.Add(9*time.Minute), route, key, identity)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != codemode.ThreadUnavailable || retired.RetryAt != nil {
		t.Fatalf("retired status = %+v, want unavailable without a retry time", retired)
	}
	if checked := attemptCount(); checked != baseline {
		t.Fatalf("status checks left %d attempts, want %d", checked, baseline)
	}
}
