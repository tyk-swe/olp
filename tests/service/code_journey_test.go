//go:build service

package service_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/promotion"
	codepromotion "github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/providers"
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
		decision.Evidence != nil || decision.EvidenceObservedAt != nil ||
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
	if pinned.Reason != codemode.DecisionPin || pinned.ChosenAccountID != healthy || pinned.Band != nil || pinned.Evidence != nil || pinned.EvidenceObservedAt != nil || len(pinned.Skipped) != 0 {
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
		connection[0].Band != nil || connection[0].Evidence != nil || connection[0].EvidenceObservedAt != nil ||
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

func bandAllowanceJSON(t *testing.T, remaining float64, resetsAt *time.Time, observedAt time.Time) []byte {
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

func TestCodeBandOrderingOrdersNewRootsAndNeverMovesPins(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	key := "11111111-1111-4111-8111-111111111111"
	healthy := "a1111111-1111-4111-8111-111111111111"
	low := "b2222222-2222-4222-8222-222222222222"
	unknown := "c3333333-3333-4333-8333-333333333333"
	stale := "d4444444-4444-4444-8444-444444444444"
	exhausted := "e5555555-5555-4555-8555-555555555555"
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
		{healthy, "band-healthy", bandAllowanceJSON(t, 75, &reset, observed)},
		{low, "band-low", bandAllowanceJSON(t, 5, &reset, observed)},
		{unknown, "band-unknown", nil},
		{stale, "band-stale", bandAllowanceJSON(t, 90, nil, base.Add(-2*time.Hour))},
		{exhausted, "band-exhausted", bandAllowanceJSON(t, 0, &reset, observed)},
		{shareA, "band-share-a", bandAllowanceJSON(t, 75, &reset, observed)},
		{shareB, "band-share-b", bandAllowanceJSON(t, 60, &reset, observed)},
	}
	poolMain, poolNoHealthy, poolShare := uuid.NewString(), uuid.NewString(), uuid.NewString()
	routes := []struct {
		id       string
		slug     string
		poolID   string
		ordering bool
	}{
		{"22222222-2222-4222-8222-222222222222", "band-off", poolMain, false},
		{"33333333-3333-4333-8333-333333333333", "band-on", poolMain, true},
		{"44444444-4444-4444-8444-444444444444", "band-no-healthy", poolNoHealthy, true},
		{"55555555-5555-4555-8555-555555555555", "band-share", poolShare, true},
	}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription"}}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'band@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'band-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'band-journey', 'plugin', 'active', '{}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'band-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
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
		{poolMain, "band-main", map[string]int64{healthy: 1, low: 1, unknown: 1}},
		{poolNoHealthy, "band-no-healthy", map[string]int64{unknown: 1, low: 1, stale: 1, exhausted: 1}},
		{poolShare, "band-share", map[string]int64{shareA: 3, shareB: 1}},
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
		route := codemode.Route{ID: seeded.id, ProjectID: project, Slug: seeded.slug, PoolID: seeded.poolID, Models: []string{"model"}, Enabled: true, AllowanceOrdering: seeded.ordering, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
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
	store := &resources.CodeStore{Pool: pool}
	admit := func(route codemode.Route, conversation string) resources.CodePermit {
		t.Helper()
		permit, err := store.Admit(t.Context(), resources.CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{provider}})
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

	off := admit(bySlug["band-off"], "band-conv-0")
	if off.Account.ID != low {
		t.Fatalf("ordering off chose %s, want the weighted score winner %s", off.Account.ID, low)
	}
	if decision := codeDecisionForAttempt(t, pool, off.Attempt.ID); decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil || decision.Reason != codemode.DecisionChosen {
		t.Fatalf("ordering off decision = %+v, want no band", decision)
	}

	on := admit(bySlug["band-on"], "band-conv-0")
	if on.Account.ID != low {
		t.Fatalf("an unmapped model chose %s, want the weighted score winner %s", on.Account.ID, low)
	}
	decision := codeDecisionForAttempt(t, pool, on.Attempt.ID)
	if decision.Band != nil ||
		decision.Evidence != nil || decision.EvidenceObservedAt != nil ||
		skippedClass(decision, healthy) != codemode.SkipWeight || skippedClass(decision, unknown) != codemode.SkipWeight {
		t.Fatalf("ordering on decision = %+v", decision)
	}

	noneHealthy := admit(bySlug["band-no-healthy"], "band-nohealthy-0")
	decision = codeDecisionForAttempt(t, pool, noneHealthy.Attempt.ID)
	if decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("no healthy decision = %+v", decision)
	}
	for _, skipped := range decision.Skipped {
		if skipped.Class == codemode.SkipBand {
			t.Fatalf("an unmapped family affected selection: %+v", decision)
		}
	}

	wins := map[string]int{}
	for index := range 40 {
		wins[admit(bySlug["band-share"], fmt.Sprintf("band-share-%d", index)).Account.ID]++
	}
	if wins[shareA]+wins[shareB] != 40 || wins[shareB] < 5 || wins[shareA] < 2*wins[shareB] {
		t.Fatalf("weights must share roots, got %v", wins)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, bandAllowanceJSON(t, 5, &reset, base)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, low, bandAllowanceJSON(t, 80, &reset, base)); err != nil {
		t.Fatal(err)
	}
	pinned := admit(bySlug["band-on"], "band-conv-0")
	if pinned.Account.ID != low {
		t.Fatalf("pinned turn moved to %s after bands flipped", pinned.Account.ID)
	}
	if decision = codeDecisionForAttempt(t, pool, pinned.Attempt.ID); decision.Reason != codemode.DecisionPin || decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("pinned decision = %+v", decision)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1)`, low); err != nil {
		t.Fatal(err)
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
			for _, conversation := range []string{uuid.NewString(), "band-conv-0"} {
				_, err = store.Admit(t.Context(), resources.CodeAdmission{Route: bySlug["band-on"], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{provider}})
				var refusal *codemode.Refusal
				if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || (tc.retryAt == nil && refusal.RetryAt != nil) || (tc.retryAt != nil && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(*tc.retryAt))) {
					t.Fatalf("admission for %s = %v (%+v), want refusal with retry at %v", conversation, err, refusal, tc.retryAt)
				}
			}
		})
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1)`, healthy); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET connections=jsonb_set(connections,ARRAY[$1,'profile_revision'],to_jsonb(repeat('0',64)),true)`, provider); err != nil {
		t.Fatal(err)
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
		off, on   bool
	}{
		{"old active mirror", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidenceActive, 0, nil, observed, &reset)}, RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, true, true},
		{"legacy top-level exhausted", codemode.Allowance{RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, false, false},
		{"positive count", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCount, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"positive credits", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCredits, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"positive cents", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCents, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"fresh exhausted", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, false, false},
		{"stale no reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), nil)}, ObservedAt: base.Add(-time.Hour)}, false, false},
		{"stale future reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), &reset)}, ObservedAt: base.Add(-time.Hour)}, false, false},
		{"stale token count", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: base.Add(-time.Hour), ResetsAt: &reset}, ObservedAt: observed}, false, false},
		{"fresh request count", codemode.Allowance{RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &reset}, ObservedAt: observed}, false, false},
		{"unrelated family", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("special_model", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, false, false},
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
			if tc.name == "stale future reset" {
				var available bool
				if err := pool.QueryRow(t.Context(), `SELECT olp.code_account_available(a,true,'codex') FROM olp.code_accounts a WHERE a.id=$1`, healthy).Scan(&available); err != nil || available {
					t.Fatalf("future-reset window availability = %t, %v", available, err)
				}
			}
			for _, check := range []struct {
				slug      string
				available bool
			}{{"band-off", tc.off}, {"band-on", tc.on}} {
				route := bySlug[check.slug]
				permit, err := store.Admit(t.Context(), resources.CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
				if check.available && (err != nil || permit.Account.ID != healthy) {
					t.Fatalf("%s admission = %+v, %v", check.slug, permit, err)
				}
				if !check.available && err == nil {
					t.Fatalf("%s admitted exhausted account %+v", check.slug, permit)
				}
				offers, err := resources.CodeOffers(t.Context(), pool, route.RevisionID)
				if err != nil || (len(offers) > 0) != check.available {
					t.Fatalf("%s offers = %+v, %v; available %t", check.slug, offers, err, check.available)
				}
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
	for _, tc := range []struct {
		slug  string
		reset time.Time
	}{{"band-on", reset}, {"band-off", reset}} {
		_, err := store.Admit(t.Context(), resources.CodeAdmission{Route: bySlug[tc.slug], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
		var refusal *codemode.Refusal
		if !errors.As(err, &refusal) || refusal.RetryAt == nil || !refusal.RetryAt.Equal(tc.reset) {
			t.Fatalf("%s refusal = %+v, want retry at %s", tc.slug, err, tc.reset)
		}
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
			for _, policy := range []struct {
				slug    string
				retryAt *time.Time
			}{{"band-on", nil}, {"band-off", nil}} {
				_, err := store.Admit(t.Context(), resources.CodeAdmission{Route: bySlug[policy.slug], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
				var refusal *codemode.Refusal
				if !errors.As(err, &refusal) || (policy.retryAt == nil && refusal.RetryAt != nil) || (policy.retryAt != nil && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(*policy.retryAt))) {
					t.Fatalf("%s refusal = %+v, want retry at %v", policy.slug, err, policy.retryAt)
				}
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
		for _, tc := range []struct {
			slug  string
			reset *time.Time
		}{{"band-on", nil}, {"band-off", nil}} {
			_, err := store.Admit(t.Context(), resources.CodeAdmission{Route: bySlug[tc.slug], APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
			var refusal *codemode.Refusal
			if !errors.As(err, &refusal) || (tc.reset == nil && refusal.RetryAt != nil) || (tc.reset != nil && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(*tc.reset))) {
				t.Fatalf("%s resetless refusal = %+v, want retry at %v", tc.slug, err, tc.reset)
			}
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
	store    *resources.CodeStore
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
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "cooldown-journey", PoolID: poolID, Models: []string{"model", "other"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	connections := `{"` + provider + `":{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription","profile_revision":"` + strings.Repeat("0", 64) + `"}}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'cooldown@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'cooldown-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'cooldown-journey', 'plugin', 'active', '{}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts,profile_id) VALUES($1, $2, 1, 'first-principal', repeat('0',64), '{}','codex-subscription')`, []any{firstCredential, provider}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts,profile_id) VALUES($1, $2, 2, 'second-principal', repeat('0',64), '{}','codex-subscription')`, []any{secondCredential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{firstCredential}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{secondCredential}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'first-principal', 'first', '["model","other"]', $5, $6)`, []any{first, project, provider, firstCredential, uuid.NewString(), user}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, 'second-principal', 'second', '["model","other"]', $5, $6)`, []any{second, project, provider, secondCredential, uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'cooldown-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'cooldown-journey', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, first}},
		{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, second}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'cooldown-journey', '{}', $3, $4)`, []any{routeID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, routeID, document, connections, user}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{routeID, revisionID}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return cooldownJourney{store: &resources.CodeStore{Pool: pool}, route: route, key: key, provider: provider, first: first, second: second}
}

func (j cooldownJourney) admission(model, conversation string) resources.CodeAdmission {
	return resources.CodeAdmission{Route: j.route, APIKeyID: j.key, Operation: codemode.Operation{Name: "responses", Model: model, Identity: codemode.Identity{Conversation: conversation}}, Providers: []string{j.provider}}
}

func TestCodeCooldownClassesScopeSelectionNeverShortenAndNameRefusalRecovery(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	journey := seedCooldownJourney(t, pool)
	base := time.Now().UTC().Truncate(time.Second)
	modelRetry := base.Add(30 * time.Minute)
	credentialRetry := base.Add(3 * time.Hour)
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: modelRetry, Envelope: "codex_model_at_capacity"}, base); err != nil {
		t.Fatal(err)
	}
	permit, err := journey.store.Admit(t.Context(), journey.admission("model", "cooldown-model-conversation"))
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != journey.second {
		t.Fatalf("model cooled admission chose %s, want %s", permit.Account.ID, journey.second)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if len(decision.Skipped) != 1 || decision.Skipped[0].AccountID != journey.first || decision.Skipped[0].Class != codemode.SkipCooldown {
		t.Fatalf("model cooldown decision = %+v", decision)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.second, "", codeadapter.Classification{Class: codeadapter.CooldownQuota, Scope: codeadapter.ScopeCredential, RetryAt: credentialRetry, Envelope: "codex_usage_limit_reached"}, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	other, err := journey.store.Admit(t.Context(), journey.admission("other", "cooldown-other-conversation"))
	if err != nil {
		t.Fatal(err)
	}
	if other.Account.ID != journey.first {
		t.Fatalf("other model admission chose %s, want the model cooled account %s", other.Account.ID, journey.first)
	}
	_, err = journey.store.Admit(t.Context(), journey.admission("model", "cooldown-refused-conversation"))
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(modelRetry) {
		t.Fatalf("refusal = %v (%+v), want recovery at %s", err, refusal, modelRetry)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: base.Add(10 * time.Minute)}, base.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.second, "", codeadapter.Classification{Class: codeadapter.CooldownQuota, Scope: codeadapter.ScopeCredential, RetryAt: base.Add(time.Hour)}, base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	var modelStored, credentialStored, unavailable time.Time
	if err := pool.QueryRow(t.Context(), `SELECT retry_at FROM olp.code_account_cooldowns WHERE account_id=$1 AND model='model'`, journey.first).Scan(&modelStored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT retry_at FROM olp.code_account_cooldowns WHERE account_id=$1 AND model IS NULL`, journey.second).Scan(&credentialStored); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT unavailable_until FROM olp.code_accounts WHERE id=$1`, journey.second).Scan(&unavailable); err != nil {
		t.Fatal(err)
	}
	if !modelStored.Equal(modelRetry) || !credentialStored.Equal(credentialRetry) || !unavailable.Equal(credentialRetry) {
		t.Fatalf("shortened cooldowns: model %s credential %s unavailable %s", modelStored, credentialStored, unavailable)
	}
	var modelHealth string
	var modelUnavailable *time.Time
	if err := pool.QueryRow(t.Context(), `SELECT health, unavailable_until FROM olp.code_accounts WHERE id=$1`, journey.first).Scan(&modelHealth, &modelUnavailable); err != nil {
		t.Fatal(err)
	}
	if modelUnavailable != nil {
		t.Fatalf("model scope cooldown mirrored to account health: %s until %s", modelHealth, modelUnavailable)
	}
}

func TestCodeModelLessConnectionChecksEveryModelCooldown(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	journey := seedCooldownJourney(t, pool)
	providers := []string{journey.provider}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	pinned, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "pinned-model-less"}, "", providers)
	if err != nil || pinned.Account.ID != journey.first {
		t.Fatalf("initial pinned connection = %+v, %v", pinned, err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=true WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	late := now.Add(20 * time.Minute)
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "other", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: late}, now); err != nil {
		t.Fatal(err)
	}
	reconnected, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "pinned-model-less"}, "", providers)
	if err != nil || reconnected.Account.ID != journey.first {
		t.Fatalf("model-less reconnect = %+v, %v, want account %s", reconnected, err, journey.first)
	}
	healthy, err := journey.store.Admit(t.Context(), resources.CodeAdmission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "pinned-model-less"}}, Providers: providers})
	if err != nil || healthy.Account.ID != journey.first {
		t.Fatalf("healthy generation = %+v, %v, want account %s", healthy, err, journey.first)
	}
	_, err = journey.store.Admit(t.Context(), resources.CodeAdmission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "other", Identity: codemode.Identity{Conversation: "pinned-model-less"}}, Providers: providers})
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(late) {
		t.Fatalf("cooled generation = %v (%+v), want retry at %s", err, refusal, late)
	}
	bound, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "new-model-less"}, "", providers)
	if err != nil || bound.Account.ID != journey.second {
		t.Fatalf("model-less connection = %+v, %v, want account %s", bound, err, journey.second)
	}
	decisions := codeConnectionDecisions(t, pool, bound.Binding.RootID)
	if len(decisions) != 1 || len(decisions[0].Skipped) != 1 || decisions[0].Skipped[0].AccountID != journey.first || decisions[0].Skipped[0].Class != codemode.SkipCooldown {
		t.Fatalf("model-less selection = %+v", decisions)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	hinted, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "hinted-model"}, "model", providers)
	if err != nil || hinted.Account.ID != journey.first {
		t.Fatalf("hinted connection = %+v, %v", hinted, err)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: now.Add(10 * time.Minute)}, now); err != nil {
		t.Fatal(err)
	}
	reconnectedAll, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "pinned-model-less"}, "", providers)
	if err != nil || reconnectedAll.Account.ID != journey.first {
		t.Fatalf("model-less reconnect with both models cooling = %+v, %v", reconnectedAll, err)
	}
	_, err = journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "pinned-model-less"}, "model", providers)
	if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("hinted reconnect = %v (%+v), want model cooldown", err, refusal)
	}
	_, err = journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "new-model-less-both"}, "", providers)
	if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(late) {
		t.Fatalf("new model-less connection = %v (%+v), want retry at %s", err, refusal, late)
	}
	credentialReset := now.Add(30 * time.Minute)
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "", codeadapter.Classification{Class: codeadapter.CooldownQuota, Scope: codeadapter.ScopeCredential, RetryAt: credentialReset}, now); err != nil {
		t.Fatal(err)
	}
	_, err = journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "pinned-model-less"}, "", providers)
	if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(credentialReset) {
		t.Fatalf("credential-cooled reconnect = %v (%+v), want retry at %s", err, refusal, credentialReset)
	}
}

func TestCodeModelLessConnectionRequiresUsefulModelHint(t *testing.T) {
	for _, tc := range []struct {
		name        string
		credential  bool
		splitModels bool
		disjoint    bool
		wantCode    string
	}{
		{name: "credential cooldown", credential: true, wantCode: "code_account_unavailable"},
		{name: "different model cooldowns", splitModels: true, wantCode: "code_connection_model_required"},
		{name: "disjoint capabilities", disjoint: true, wantCode: "code_connection_model_required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, _ := newDatabase(t)
			if err := database.Migrate(t.Context(), pool); err != nil {
				t.Fatal(err)
			}
			journey := seedCooldownJourney(t, pool)
			now := time.Now().UTC().Truncate(time.Second)
			if tc.credential {
				for i, account := range []string{journey.first, journey.second} {
					if err := journey.store.ObserveCooldownClass(t.Context(), account, "", codeadapter.Classification{Class: codeadapter.CooldownQuota, Scope: codeadapter.ScopeCredential, RetryAt: now.Add(time.Duration(i+1) * 10 * time.Minute)}, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.splitModels {
				for i, account := range []string{journey.first, journey.second} {
					if err := journey.store.ObserveCooldownClass(t.Context(), account, journey.route.Models[i], codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: now.Add(10 * time.Minute)}, now); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.disjoint {
				if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET models=CASE WHEN id=$1 THEN '["model"]'::jsonb ELSE '["other"]'::jsonb END WHERE id=ANY($2::uuid[])`, journey.first, []string{journey.first, journey.second}); err != nil {
					t.Fatal(err)
				}
			}
			providers := []string{journey.provider}
			_, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "model-less"}, "", providers)
			var refusal *codemode.Refusal
			if !errors.As(err, &refusal) || refusal.Code != tc.wantCode {
				t.Fatalf("model-less refusal = %v (%+v), want %s", err, refusal, tc.wantCode)
			}
			if tc.credential && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(now.Add(10*time.Minute))) {
				t.Fatalf("credential retry = %+v, want %s", refusal.RetryAt, now.Add(10*time.Minute))
			}
			for _, model := range journey.route.Models {
				_, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "hinted-" + model}, model, providers)
				if tc.credential && err == nil || !tc.credential && err != nil {
					t.Errorf("hinted connection for %s = %v", model, err)
				}
			}
		})
	}
}

func TestCodeModelCooldownFollowsAliasTarget(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	journey := seedCooldownJourney(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	observed := time.Now().UTC().Truncate(time.Second)
	reset := observed.Add(time.Hour)
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: reset}, observed); err != nil {
		t.Fatal(err)
	}
	route := journey.route
	route.ModelRewriting = true
	route.Aliases = map[string]string{"first-alias": "model", "second-alias": "model"}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET document=$2 WHERE id=$1`, route.RevisionID, document); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"first-alias", "second-alias"} {
		_, err := journey.store.Admit(t.Context(), resources.CodeAdmission{Route: route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: alias, Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{journey.provider}})
		var refusal *codemode.Refusal
		if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(reset) {
			t.Fatalf("cooled target via %s = %v (%+v), want refusal until %s", alias, err, refusal, reset)
		}
	}
	checkCatalog := func(want []string) {
		t.Helper()
		offers, err := resources.CodeOffers(t.Context(), pool, route.RevisionID, journey.key)
		if err != nil {
			t.Fatal(err)
		}
		models := slices.Sorted(func(yield func(string) bool) {
			for _, model := range codeclient.Catalog(route, offers) {
				if !yield(model.ID) {
					return
				}
			}
		})
		if !slices.Equal(models, want) {
			t.Fatalf("offered models = %v, want %v", models, want)
		}
	}
	checkCatalog([]string{"other"})
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=true WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	checkCatalog([]string{"first-alias", "model", "other", "second-alias"})
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	republished := route
	republished.RevisionID = uuid.NewString()
	republished.Revision = 2
	republished.Aliases = map[string]string{"first-alias": "other", "second-alias": "model"}
	republishedDocument, err := json.Marshal(republished)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.code_route_revisions(id,route_id,revision,document,connections,created_by) SELECT $1,route_id,2,$2,connections,created_by FROM olp.code_route_revisions WHERE id=$3`, republished.RevisionID, republishedDocument, route.RevisionID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, route.ID, republished.RevisionID); err != nil {
		t.Fatal(err)
	}
	permit, err := journey.store.Admit(t.Context(), resources.CodeAdmission{Route: republished, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "first-alias", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{journey.provider}})
	if err != nil || permit.Account.ID != journey.first || permit.Attempt.Model != "other" {
		t.Fatalf("same alias with a different target = %+v, %v", permit, err)
	}
}

func TestCodeCatalogReachesPaidModelPoolFallback(t *testing.T) {
	route := codemode.Route{
		Enabled: true, RevisionID: "revision", Slug: "route", Models: []string{"native-a", "native-b"},
		ModelRewriting: true, Aliases: map[string]string{"pool": "native-a"},
		ModelPools:        map[string][]string{"pool": {"native-a", "native-b"}},
		PaidParticipation: true, InitialRecoveryAttempts: 1,
		PaidFallbackTargets: map[string]codemode.PaidTarget{"native-b": {}},
	}
	offers := codeclient.Offers{{PaidProtocol: codemode.ProtocolResponses, PaidStreaming: true, Models: []string{"native-b"}}}
	fallback, err := route.PaidFallbackModel(codemode.Operation{Model: "pool"}, nil)
	if err != nil || fallback == nil || fallback.Model != "native-b" {
		t.Fatalf("paid fallback = %+v, error = %v", fallback, err)
	}
	pool := func(route codemode.Route, offers codeclient.Offers) *codemode.Model {
		models := codeclient.Catalog(route, offers)
		index := slices.IndexFunc(models, func(model codemode.Model) bool { return model.ID == "pool" })
		if index < 0 {
			return nil
		}
		return &models[index]
	}
	model := pool(route, offers)
	if model == nil || !model.Paid || !slices.Contains(model.Clients, codeadapter.ClientCodex) || len(model.Capabilities) != 1 || model.Capabilities[0].Protocol != codemode.ProtocolResponses {
		t.Fatalf("pool catalog entry = %+v", model)
	}
	configuration, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"})
	if err != nil || configuration.Model != "pool" || !slices.Contains(configuration.PaidModels, "pool") || !strings.Contains(configuration.Configuration, "supports_websockets = false") {
		t.Fatalf("client configuration = %+v, error = %v", configuration, err)
	}
	missing := pool(route, nil)
	if missing != nil {
		t.Fatalf("unavailable paid target advertised: %+v", missing)
	}
	withoutRecovery := route
	withoutRecovery.InitialRecoveryAttempts = 0
	if model := pool(withoutRecovery, offers); model != nil {
		t.Fatalf("disabled fallback advertised: %+v", model)
	}
	withoutPool := route
	withoutPool.ModelPools = nil
	if model := pool(withoutPool, offers); model != nil {
		t.Fatalf("unreachable alternate advertised: %+v", model)
	}
	earlierFallback := route
	earlierFallback.PaidFallbackTargets = map[string]codemode.PaidTarget{"native-a": {}, "native-b": {}}
	if model := pool(earlierFallback, offers); model != nil {
		t.Fatalf("unselected alternate advertised: %+v", model)
	}
	messageOffers := codeclient.Offers{{PaidProtocol: codemode.ProtocolMessages, PaidStreaming: true, Models: []string{"native-b"}}}
	messageModel := pool(route, messageOffers)
	if messageModel == nil || slices.Contains(messageModel.Clients, codeadapter.ClientCodex) || !slices.Contains(messageModel.Clients, codeadapter.ClientClaudeCode) {
		t.Fatalf("messages paid fallback = %+v", messageModel)
	}
	if _, err := codeclient.Generate(route, messageOffers, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"}); err == nil {
		t.Fatal("unsupported client accepted for paid fallback")
	}
	withoutStreaming := codeclient.Offers{{PaidProtocol: codemode.ProtocolResponses, Models: []string{"native-b"}}}
	if model := pool(route, withoutStreaming); model == nil || len(model.Clients) != 0 {
		t.Fatalf("non-streaming paid fallback clients = %+v", model)
	}
	withSubscription := slices.Concat(offers, codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-a"}}})
	configuration, err = codeclient.Generate(route, withSubscription, codeclient.Request{GatewayURL: "http://localhost:4000", Client: codeadapter.ClientCodex, Model: "pool"})
	if err != nil || !strings.Contains(configuration.Configuration, "supports_websockets = true") {
		t.Fatalf("subscription client configuration = %+v, error = %v", configuration, err)
	}
}

func TestCodeRecoveryRecordsActualDestinationSelection(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	journey := seedCooldownJourney(t, pool)
	third, credential := uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,grant_facts,profile_id) VALUES($1,$2,3,'third-principal',repeat('0',64),'{}','codex-subscription')`, []any{credential, journey.provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
		{`INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,models,etag,created_by,enabled) SELECT $1,project_id,provider_id,$2,'third-principal','third',models,$3,created_by,false FROM olp.code_accounts WHERE id=$4`, []any{third, credential, uuid.NewString(), journey.first}},
		{`INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, []any{journey.route.PoolID, third}},
		{`UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, []any{journey.second}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	journey.route.InitialRecoveryAttempts = 1
	journey.route.ModelRewriting = true
	journey.route.ModelPools = map[string][]string{"model": {"model", "other"}}
	document, err := json.Marshal(journey.route)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET document=$2 WHERE id=$1`, journey.route.RevisionID, document); err != nil {
		t.Fatal(err)
	}
	admission := journey.admission("model", "recovery-decision")
	admission.InitialRecovery = true
	first, err := journey.store.Admit(t.Context(), admission)
	if err != nil || first.Account.ID != journey.first {
		t.Fatalf("initial admission = %+v, %v", first, err)
	}
	now := time.Now().UTC()
	status := 429
	if err := journey.store.MarkDispatched(t.Context(), first.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ObserveOutcome(t.Context(), first.Attempt.ID, codemode.Outcome{Origin: "upstream", Kind: "rejected", UpstreamStatus: &status, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ConfirmNonexecution(t.Context(), first.Attempt.ID, status); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	if err := journey.store.Settle(t.Context(), first.Attempt.ID, codemode.Usage{Total: &zero}); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "", codeadapter.Classification{Class: codeadapter.CooldownQuota, Scope: codeadapter.ScopeCredential, RetryAt: now.Add(time.Hour)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=true WHERE id=ANY($1::uuid[])`, []string{journey.second, third}); err != nil {
		t.Fatal(err)
	}
	for _, account := range []string{journey.second, third} {
		if err := journey.store.ObserveCooldownClass(t.Context(), account, "model", codeadapter.Classification{Class: codeadapter.CooldownCapacity, Scope: codeadapter.ScopeModel, RetryAt: now.Add(time.Hour)}, now); err != nil {
			t.Fatal(err)
		}
	}
	admission.Recovery = &resources.CodeRecovery{From: first.Attempt.ID, Envelope: "codex_usage_limit_reached", Number: 1}
	recovered, err := journey.store.Admit(t.Context(), admission)
	if err != nil || recovered.Attempt.Model != "other" {
		t.Fatalf("model-pool recovery = %+v, %v", recovered, err)
	}
	decision := codeDecisionForAttempt(t, pool, recovered.Attempt.ID)
	if decision.Reason != codemode.DecisionChosen || decision.ChosenAccountID != recovered.Account.ID || decision.Band != nil || decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 || len(decision.Skipped) != 1 || decision.Skipped[0].Class != codemode.SkipWeight || decision.Skipped[0].AccountID == recovered.Account.ID {
		t.Fatalf("recovery decision = %+v", decision)
	}
}

func TestCodeTreeRefusalUsesPreferredAccountRecovery(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	journey := seedCooldownJourney(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, journey.second); err != nil {
		t.Fatal(err)
	}
	root, err := journey.store.Admit(t.Context(), journey.admission("model", "tree-recovery"))
	if err != nil || root.Account.ID != journey.first {
		t.Fatalf("root admission = %+v, %v", root, err)
	}
	provider, credential, alternative := uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.providers(id,project_id,name,kind,state,configuration,etag,slots_etag,created_by) SELECT $1,project_id,'tree-alternative','plugin','active','{}',$2,$3,created_by FROM olp.code_accounts WHERE id=$4`, []any{provider, uuid.NewString(), uuid.NewString(), journey.first}},
		{`INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,grant_facts) VALUES($1,$2,1,'alternative',repeat('0',64),'{}')`, []any{credential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
		{`INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,models,etag,created_by) SELECT $1,project_id,$2,$3,'alternative','alternative','["other"]',$4,created_by FROM olp.code_accounts WHERE id=$5`, []any{alternative, provider, credential, uuid.NewString(), journey.first}},
		{`INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, []any{journey.route.PoolID, alternative}},
		{`UPDATE olp.code_route_revisions SET connections=jsonb_set(connections,ARRAY[$2::text],$3::jsonb,true) WHERE id=$1`, []any{journey.route.RevisionID, provider, `{"kind":"plugin","auth_mode":"grant","profile_id":"zai-coding-plan"}`}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Truncate(time.Second)
	preferredRetry := base.Add(30 * time.Minute)
	for _, observation := range []struct {
		account string
		retryAt time.Time
	}{{journey.first, preferredRetry}, {alternative, base.Add(5 * time.Minute)}} {
		if err := journey.store.ObserveCooldownClass(t.Context(), observation.account, "other", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeModel, RetryAt: observation.retryAt}, base); err != nil {
			t.Fatal(err)
		}
	}
	_, err = journey.store.Admit(t.Context(), resources.CodeAdmission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "other", Identity: codemode.Identity{Conversation: "tree-recovery"}}, Providers: []string{journey.provider, provider}})
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.RetryAt == nil || !refusal.RetryAt.Equal(preferredRetry) {
		t.Fatalf("tree refusal = %v (%+v), want preferred account recovery at %s", err, refusal, preferredRetry)
	}
}

func TestCodeActiveAllowanceDoesNotAffectBands(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	active, unknown := uuid.NewString(), uuid.NewString()
	key, poolID, routeID, revisionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "active-band", PoolID: poolID, Models: []string{"model"}, Enabled: true, AllowanceOrdering: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	configuration := `{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription","profile_revision":"` + strings.Repeat("0", 64) + `"}`
	connections := `{"` + provider + `":` + configuration + `}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'active@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'active-band', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'active-band', 'plugin', 'active', $3, $4, $5, $6)`, []any{provider, project, configuration, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'active-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'active-band', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'active-band', '{}', $3, $4)`, []any{routeID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, routeID, document, connections, user}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{routeID, revisionID}},
	}
	for index, account := range []struct {
		id        string
		principal string
	}{{active, "active-principal"}, {unknown, "unknown-principal"}} {
		credential := uuid.NewString()
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, $3, $4, repeat('0',64), '{}')`, []any{credential, provider, index + 1, account.principal}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, $5, $6, '["model"]', $7, $8)`, []any{account.id, project, provider, credential, account.principal, account.principal, uuid.NewString(), user}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, account.id}},
		)
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance_read_requested_at=$2 WHERE id=$1`, active, base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	store := &resources.CodeStore{Pool: pool}
	due, err := store.DueAllowanceReads(t.Context(), base, 50, []string{"codex-subscription"})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].Account.ID != active || due[0].Configuration.ProfileID != "codex-subscription" {
		t.Fatalf("due reads = %+v, want the requested Codex account first with its live connection", due)
	}
	reset := base.Add(2 * time.Hour)
	body := fmt.Sprintf(`{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":20,"limit_window_seconds":18000,"reset_after_seconds":3600,"reset_at":%d},"secondary_window":null},"credits":{"has_credits":true,"unlimited":false,"balance":"3.25"}}`, reset.Unix())
	allowance, ok := codeallowance.ParseUsage([]byte(body), base)
	if !ok {
		t.Fatal("pinned usage payload must parse")
	}
	if err := store.ObserveAllowance(t.Context(), active, allowance); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAllowanceRead(t.Context(), active, base); err != nil {
		t.Fatal(err)
	}
	var requestedAt, readAt *time.Time
	if err := pool.QueryRow(t.Context(), `SELECT allowance_read_requested_at, allowance_read_at FROM olp.code_accounts WHERE id=$1`, active).Scan(&requestedAt, &readAt); err != nil {
		t.Fatal(err)
	}
	if requestedAt != nil || readAt == nil || !readAt.Equal(base) {
		t.Fatalf("completed read state requested %v read %v", requestedAt, readAt)
	}
	due, err = store.DueAllowanceReads(t.Context(), base, 50, []string{"codex-subscription"})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Account.ID != unknown {
		t.Fatalf("fresh active evidence must not be due again, got %+v", due)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, unknown); err != nil {
		t.Fatal(err)
	}
	permit, err := store.Admit(t.Context(), resources.CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "active-conv-0"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != active {
		t.Fatalf("chosen account = %s", permit.Account.ID)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("active band decision = %+v", decision)
	}
	passive := codemode.Allowance{Windows: []codemode.AllowanceWindow{{LimitID: "codex", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 40, RemainingPercent: 60, ResetsAt: &reset, ObservedAt: base}}, ObservedAt: base}
	if err := store.ObserveAllowance(t.Context(), active, passive); err != nil {
		t.Fatal(err)
	}
	if err := store.InvalidateActiveAllowance(t.Context(), active); err != nil {
		t.Fatal(err)
	}
	var stored codemode.Allowance
	if err := pool.QueryRow(t.Context(), `SELECT allowance FROM olp.code_accounts WHERE id=$1`, active).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Windows) != 1 || stored.Windows[0].Evidence != codemode.AllowanceEvidencePassive || stored.Windows[0].RemainingPercent != 60 || stored.RemainingPercent == nil || *stored.RemainingPercent != 60 || stored.Credits == nil {
		t.Fatalf("allowance after failed read = %+v, want passive window and credits preserved", stored)
	}

}

func TestCodeMiniMaxActiveRemainsDoesNotAffectBands(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	active, unknown := uuid.NewString(), uuid.NewString()
	key, poolID, routeID, revisionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "minimax-active-band", PoolID: poolID, Models: []string{"MiniMax-M3"}, Enabled: true, AllowanceOrdering: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
	document, err := json.Marshal(route)
	if err != nil {
		t.Fatal(err)
	}
	configuration := `{"kind":"plugin","auth_mode":"grant","profile_id":"minimax-plan","profile_revision":"` + strings.Repeat("0", 64) + `"}`
	connections := `{"` + provider + `":` + configuration + `}`
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'minimax-active@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'minimax-active-band', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'minimax-active-band', 'plugin', 'active', $3, $4, $5, $6)`, []any{provider, project, configuration, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.api_keys(id, lookup_id, digest, name, created_by, policy, etag, project_id) VALUES($1, 'minimax-active-lookup', '\x00', 'journey', $2, '{"scopes":["inference"]}', $3, $4)`, []any{key, user, uuid.NewString(), project}},
		{`INSERT INTO olp.code_pools(id, project_id, name, kind, etag, created_by) VALUES($1, $2, 'minimax-active-band', 'shared', $3, $4)`, []any{poolID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_keys(pool_id, api_key_id) VALUES($1, $2)`, []any{poolID, key}},
		{`INSERT INTO olp.code_routes(id, project_id, slug, draft, etag, created_by) VALUES($1, $2, 'minimax-active-band', '{}', $3, $4)`, []any{routeID, project, uuid.NewString(), user}},
		{`INSERT INTO olp.code_route_revisions(id, route_id, revision, document, connections, created_by) VALUES($1, $2, 1, $3, $4, $5)`, []any{revisionID, routeID, document, connections, user}},
		{`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, []any{routeID, revisionID}},
	}
	for index, account := range []struct {
		id        string
		principal string
	}{{active, "minimax-active-principal"}, {unknown, "minimax-unknown-principal"}} {
		credential := uuid.NewString()
		statements = append(statements,
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, $3, $4, repeat('0',64), '{}')`, []any{credential, provider, index + 1, account.principal}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, $5, $6, '["MiniMax-M3"]', $7, $8)`, []any{account.id, project, provider, credential, account.principal, account.principal, uuid.NewString(), user}},
			struct {
				sql  string
				args []any
			}{`INSERT INTO olp.code_pool_accounts(pool_id, account_id, weight) VALUES($1, $2, 1)`, []any{poolID, account.id}},
		)
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Truncate(time.Second)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance_read_requested_at=$2 WHERE id=$1`, active, base.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	store := &resources.CodeStore{Pool: pool}
	due, err := store.DueAllowanceReads(t.Context(), base, 50, []string{"minimax-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0].Account.ID != active || due[0].Configuration.ProfileID != "minimax-plan" || len(due[0].Account.Models) != 1 || due[0].Account.Models[0] != "MiniMax-M3" {
		t.Fatalf("due reads = %+v, want the requested MiniMax account first with its live connection and models", due)
	}
	reset := base.Add(2 * time.Hour)
	start := reset.Add(-5 * time.Hour)
	body := fmt.Sprintf(`{"model_remains":[{"model_name":"MiniMax-M3","start_time":%d,"end_time":%d,"remains_time":7200000,"current_interval_total_count":100,"current_interval_usage_count":20,"current_interval_remaining_percent":80,"current_weekly_total_count":500,"current_weekly_usage_count":200,"current_weekly_remaining_percent":60,"current_interval_status":1,"current_weekly_status":1,"weekly_start_time":%d,"weekly_end_time":%d,"weekly_remains_time":604800000}],"base_resp":{"status_code":0,"status_msg":"success"}}`,
		start.UnixMilli(), reset.UnixMilli(), start.UnixMilli(), start.Add(7*24*time.Hour).UnixMilli())
	allowance, ok := codeallowance.ParseRemains([]byte(body), due[0].Account.Models, base)
	if !ok {
		t.Fatal("pinned remains payload must parse")
	}
	if err := store.ObserveAllowance(t.Context(), active, allowance); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteAllowanceRead(t.Context(), active, base); err != nil {
		t.Fatal(err)
	}
	due, err = store.DueAllowanceReads(t.Context(), base, 50, []string{"minimax-plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].Account.ID != unknown {
		t.Fatalf("fresh MiniMax active evidence must not be due again, got %+v", due)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, unknown); err != nil {
		t.Fatal(err)
	}
	permit, err := store.Admit(t.Context(), resources.CodeAdmission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "messages", Model: "MiniMax-M3", Identity: codemode.Identity{Conversation: "minimax-active-conv-0"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != active {
		t.Fatalf("chosen account = %s", permit.Account.ID)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("MiniMax active band decision = %+v", decision)
	}
}

func TestCodeAllowanceReadRequestEndpoint(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	var user string
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.users WHERE email='owner@example.com'`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "allowance-read"}, idempotent(), http.StatusCreated)["id"].(string)
	seed := func(profile, principal string) string {
		t.Helper()
		provider, credential, account := uuid.NewString(), uuid.NewString(), uuid.NewString()
		configuration := `{"kind":"plugin","auth_mode":"grant","profile_id":"` + profile + `","profile_revision":"` + strings.Repeat("0", 64) + `"}`
		for _, statement := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, $7, 'plugin', 'active', $3, $4, $5, $6)`, []any{provider, project, configuration, uuid.NewString(), uuid.NewString(), user, principal}},
			{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, profile_id, grant_facts) VALUES($1, $2, 1, $3, repeat('0',64), $4, '{}')`, []any{credential, provider, principal, profile}},
			{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
			{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, etag, created_by) VALUES($1, $2, $3, $4, $5, $6, '["model"]', $7, $8)`, []any{account, project, provider, credential, principal, principal, uuid.NewString(), user}},
		} {
			if _, err := s.pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
				t.Fatal(err)
			}
		}
		return account
	}
	codex := seed("codex-subscription", "codex-principal")
	minimax := seed("minimax-plan", "minimax-principal")
	claude := seed("claude-subscription", "claude-principal")

	if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+codex+"/allowance-reads", nil, nil); status != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key: status %d: %s", status, raw)
	}
	for _, account := range []string{codex, minimax} {
		if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+account+"/allowance-reads", nil, idempotent()); status != http.StatusUnprocessableEntity {
			t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
		}
	}
	if status, raw := s.do(owner, "POST", "/api/v1/code/accounts/"+claude+"/allowance-reads", nil, idempotent()); status != http.StatusUnprocessableEntity {
		t.Fatalf("unqualified allowance read: status %d: %s", status, raw)
	}
	s.eventually("code allowance worker checkpoint", 30*time.Second, func() (bool, string) {
		var checks int
		if err := s.pool.QueryRow(t.Context(), `SELECT successes_total+failures_total FROM olp.worker_task_health WHERE task='code_allowance_read'`).Scan(&checks); err != nil {
			return false, err.Error()
		}
		return checks > 0, "no checkpoint yet"
	})
}
func TestCodeAccountExtraUsageRoundTripsAndRefusesNonKimiAccounts(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project := uuid.NewString(), uuid.NewString()
	kimiProvider, codexProvider := uuid.NewString(), uuid.NewString()
	kimiCredential, codexCredential := uuid.NewString(), uuid.NewString()
	kimiAccount, codexAccount := uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'extra-usage@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'extra-usage-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'extra-usage-kimi', 'plugin', 'active', '{"kind":"plugin","auth_mode":"grant","profile_id":"kimi-code"}', $3, $4, $5)`, []any{kimiProvider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'extra-usage-codex', 'plugin', 'active', '{"kind":"plugin","auth_mode":"grant","profile_id":"codex-subscription"}', $3, $4, $5)`, []any{codexProvider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 1, 'kimi-principal', repeat('0',64), '{}')`, []any{kimiCredential, kimiProvider}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 1, 'codex-principal', repeat('0',64), '{}')`, []any{codexCredential, codexProvider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{kimiCredential}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{codexCredential}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	operator := access.Principal{User: access.User{ID: user}, AllProjects: true}
	enabled, disabled := true, false
	save := func(id string, input providers.CodeAccountInput, existing bool) (map[string]any, error) {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		body, err := providers.SaveCodeAccount(t.Context(), tx, operator, id, access.NewID(), input, existing)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		var account map[string]any
		if err := json.Unmarshal(body, &account); err != nil {
			t.Fatal(err)
		}
		return account, nil
	}
	kimiInput := providers.CodeAccountInput{ProjectID: project, ProviderID: kimiProvider, CredentialID: kimiCredential, Name: "kimi", Enabled: true, Models: []string{"model"}, ExtraUsageEnabled: &enabled}
	created, err := save(kimiAccount, kimiInput, false)
	if err != nil {
		t.Fatal(err)
	}
	if created["adapter"] != "kimi_code" || created["extra_usage_enabled"] != true {
		t.Fatalf("created kimi account = %v, want extra_usage_enabled true in the management projection", created)
	}
	kimiInput.ExtraUsageEnabled = &disabled
	updated, err := save(kimiAccount, kimiInput, true)
	if err != nil {
		t.Fatal(err)
	}
	if updated["extra_usage_enabled"] != false {
		t.Fatalf("updated kimi account = %v, want extra_usage_enabled false", updated)
	}
	kimiInput.ExtraUsageEnabled = nil
	cleared, err := save(kimiAccount, kimiInput, true)
	if err != nil {
		t.Fatal(err)
	}
	if value, present := cleared["extra_usage_enabled"]; !present || value != nil {
		t.Fatalf("cleared kimi account = %v, want extra_usage_enabled present and null (not recorded)", cleared)
	}
	codexInput := providers.CodeAccountInput{ProjectID: project, ProviderID: codexProvider, CredentialID: codexCredential, Name: "codex", Enabled: true, Models: []string{"model"}, ExtraUsageEnabled: &enabled}
	if _, err := save(codexAccount, codexInput, false); err == nil {
		t.Fatal("extra usage on a Codex account saved, want a refusal")
	}
	var storedAccounts int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_accounts WHERE id=$1`, codexAccount).Scan(&storedAccounts); err != nil {
		t.Fatal(err)
	}
	if storedAccounts != 0 {
		t.Fatal("refused non-Kimi account was stored")
	}
	kimiInput.ExtraUsageEnabled = &enabled
	if _, err := save(kimiAccount, kimiInput, true); err != nil {
		t.Fatal(err)
	}
	exported, err := codepromotion.ExportCode(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(exported.Accounts, func(entry promotion.CodeAccountEntry) bool { return entry.ID == kimiAccount })
	if index < 0 || exported.Accounts[index].ExtraUsageEnabled == nil || !*exported.Accounts[index].ExtraUsageEnabled {
		t.Fatalf("exported accounts = %+v, want the kimi account carrying extra usage on", exported.Accounts)
	}
}
