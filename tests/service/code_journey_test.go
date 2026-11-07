//go:build service

package service_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/antigravityauth"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeallowance"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"

	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/promotion"
	codepromotion "github.com/tyk-swe/olp/internal/promotion/code"
	"github.com/tyk-swe/olp/internal/providers"
)

func TestCodeDecisionJourneys(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	t.Run("chosen and pinned", func(t *testing.T) { codeAdmissionRecordsChosenAndPinnedDecisions(t, pool) })
	t.Run("allowance ordering", func(t *testing.T) { codeBandOrderingOrdersNewRootsAndNeverMovesPins(t, pool) })
	t.Run("mixed adapter bands", func(t *testing.T) { codeBandsAssessEachAdapter(t, pool) })
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

func bandAllowanceJSON(t *testing.T, remaining float64, resetsAt *time.Time, observedAt time.Time) []byte {
	t.Helper()
	document, err := json.Marshal(codemode.Allowance{
		Windows:          []codemode.AllowanceWindow{{LimitID: "codex", MeterID: "codex", LimitName: "model", Window: "primary", Unit: codemode.AllowanceUnitPercent, Evidence: codemode.AllowanceEvidencePassive, UsedPercent: 100 - remaining, RemainingPercent: remaining, ResetsAt: resetsAt, ObservedAt: observedAt}},
		RemainingPercent: &remaining,
		ResetsAt:         resetsAt,
		ObservedAt:       observedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func codeBandOrderingOrdersNewRootsAndNeverMovesPins(t *testing.T, pool *pgxpool.Pool) {
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

	off := admit(bySlug["band-off"], "band-conv-0")
	if off.Account.ID != low {
		t.Fatalf("ordering off chose %s, want the weighted score winner %s", off.Account.ID, low)
	}
	if decision := codeDecisionForAttempt(t, pool, off.Attempt.ID); decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil || decision.Reason != codemode.DecisionChosen {
		t.Fatalf("ordering off decision = %+v, want no band", decision)
	}

	on := admit(bySlug["band-on"], "band-conv-0")
	if on.Account.ID != healthy {
		t.Fatalf("exact model ordering chose %s, want healthy %s", on.Account.ID, healthy)
	}
	decision := codeDecisionForAttempt(t, pool, on.Attempt.ID)
	if decision.Band == nil || *decision.Band != codemode.BandHealthy ||
		decision.Evidence == nil || decision.EvidenceObservedAt == nil ||
		skippedClass(decision, low) != codemode.SkipBand || skippedClass(decision, unknown) != codemode.SkipBand {
		t.Fatalf("ordering on decision = %+v", decision)
	}

	noneHealthy := admit(bySlug["band-no-healthy"], "band-nohealthy-0")
	decision = codeDecisionForAttempt(t, pool, noneHealthy.Attempt.ID)
	if decision.Band == nil || *decision.Band != codemode.BandUnknown || decision.Evidence != nil || decision.EvidenceObservedAt != nil || skippedClass(decision, low) != codemode.SkipBand || skippedClass(decision, exhausted) != codemode.SkipBand {
		t.Fatalf("unknown must precede low and exhausted: %+v", decision)
	}

	wins := map[string]int{}
	for index := range 40 {
		wins[admit(bySlug["band-share"], fmt.Sprintf("band-share-%d", index)).Account.ID]++
	}
	if wins[shareA]+wins[shareB] != 40 || wins[shareB] < 5 || wins[shareA] < 2*wins[shareB] {
		t.Fatalf("weights must share roots, got %v", wins)
	}

	for _, phase := range []struct {
		remaining float64
		band      codemode.Band
	}{{5, codemode.BandLow}, {0, codemode.BandExhausted}} {
		if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id IN ($1,$3)`, shareA, bandAllowanceJSON(t, phase.remaining, &reset, base), shareB); err != nil {
			t.Fatal(err)
		}
		selected := admit(bySlug["band-share"], uuid.NewString())
		if got := codeDecisionForAttempt(t, pool, selected.Attempt.ID); got.Band == nil || *got.Band != phase.band {
			t.Fatalf("last advisory band must remain usable: %+v", got)
		}
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, healthy, bandAllowanceJSON(t, 5, &reset, base)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, low, bandAllowanceJSON(t, 80, &reset, base)); err != nil {
		t.Fatal(err)
	}
	pinned := admit(bySlug["band-on"], "band-conv-0")
	if pinned.Account.ID != healthy {
		t.Fatalf("pinned turn moved to %s after bands flipped", pinned.Account.ID)
	}
	if decision = codeDecisionForAttempt(t, pool, pinned.Attempt.ID); decision.Reason != codemode.DecisionPin || decision.Band != nil || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("pinned decision = %+v", decision)
	}
	other := seedCooldownJourney(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1) WHERE project_id=$2`, low, project); err != nil {
		t.Fatal(err)
	}
	if _, err := other.store.Admit(t.Context(), other.admission("model", "other-project-first")); err != nil {
		t.Fatalf("other project admission after pinned selection: %v", err)
	}

	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET connections=jsonb_set(connections,ARRAY[$1,'profile_revision'],to_jsonb(repeat('0',64)),true) WHERE route_id IN (SELECT id FROM olp.code_routes WHERE project_id=$2)`, provider, project); err != nil {
		t.Fatal(err)
	}
	if _, err := other.store.Admit(t.Context(), other.admission("model", "other-project-second")); err != nil {
		t.Fatalf("other project admission after root selection: %v", err)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=(id=$1) WHERE project_id=$2`, healthy, project); err != nil {
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
		{"missing window", codemode.Allowance{ObservedAt: observed}, true, true},
		{"ambiguous windows", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset), window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 60, nil, observed, &reset)}, ObservedAt: observed}, true, true},
		{"old active mirror", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidenceActive, 0, nil, observed, &reset)}, RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, true, true},
		{"legacy top-level exhausted", codemode.Allowance{RemainingPercent: new(float64(0)), ResetsAt: &reset, ObservedAt: observed}, true, true},
		{"positive count", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCount, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"positive credits", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCredits, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"positive cents", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitCents, codemode.AllowanceEvidencePassive, 0, new(float64(9)), observed, &reset)}, ObservedAt: observed}, true, true},
		{"fresh exhausted", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, true, true},
		{"stale no reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), nil)}, ObservedAt: base.Add(-time.Hour)}, true, true},
		{"stale future reset", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("codex", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, base.Add(-time.Hour), &reset)}, ObservedAt: base.Add(-time.Hour)}, true, true},
		{"stale token count", codemode.Allowance{RemainingTokens: new(int64(0)), TokenObservation: &codemode.CountObservation{ObservedAt: base.Add(-time.Hour), ResetsAt: &reset}, ObservedAt: observed}, true, true},
		{"fresh request count", codemode.Allowance{RemainingRequests: new(int64(0)), RequestObservation: &codemode.CountObservation{ObservedAt: observed, ResetsAt: &reset}, ObservedAt: observed}, true, true},
		{"unrelated family", codemode.Allowance{Windows: []codemode.AllowanceWindow{window("special_model", codemode.AllowanceUnitPercent, codemode.AllowanceEvidencePassive, 0, nil, observed, &reset)}, ObservedAt: observed}, true, true},
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
			for _, check := range []struct {
				slug      string
				available bool
			}{{"band-off", tc.off}, {"band-on", tc.on}} {
				route := bySlug[check.slug]
				permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{provider}})
				if check.available && (err != nil || permit.Account.ID != healthy) {
					t.Fatalf("%s selected account = %s, error = %v", check.slug, permit.Account.ID, err)
				}
				if !check.available && err == nil {
					t.Fatalf("%s admitted exhausted account %+v", check.slug, permit)
				}
				offers, err := codesessions.Offers(t.Context(), pool, route.RevisionID)
				if err != nil || (len(offers) > 0) != check.available {
					t.Fatalf("%s offers = %+v, %v; available %t", check.slug, offers, err, check.available)
				}
			}
		})
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

func TestCodeCooldownJourneys(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	t.Run("classes and recovery", func(t *testing.T) { codeCooldownClassesScopeSelectionNeverShortenAndNameRefusalRecovery(t, pool) })
	t.Run("model-less cooldowns", func(t *testing.T) { codeModelLessConnectionChecksEveryModelCooldown(t, pool) })
	t.Run("model hints", func(t *testing.T) { codeModelLessConnectionRequiresUsefulModelHint(t, pool) })
	t.Run("alias targets", func(t *testing.T) { codeModelCooldownFollowsAliasTarget(t, pool) })
	t.Run("recovery selection", func(t *testing.T) { codeRecoveryRecordsActualDestinationSelection(t, pool) })
	t.Run("tree recovery", func(t *testing.T) { codeTreeRefusalUsesPreferredAccountRecovery(t, pool) })
}

func codeCooldownClassesScopeSelectionNeverShortenAndNameRefusalRecovery(t *testing.T, pool *pgxpool.Pool) {
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

func codeModelLessConnectionChecksEveryModelCooldown(t *testing.T, pool *pgxpool.Pool) {
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
	healthy, err := journey.store.Admit(t.Context(), codesessions.Admission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "pinned-model-less"}}, Providers: providers})
	if err != nil || healthy.Account.ID != journey.first {
		t.Fatalf("healthy generation = %+v, %v, want account %s", healthy, err, journey.first)
	}
	_, err = journey.store.Admit(t.Context(), codesessions.Admission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "other", Identity: codemode.Identity{Conversation: "pinned-model-less"}}, Providers: providers})
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

func codeModelLessConnectionRequiresUsefulModelHint(t *testing.T, pool *pgxpool.Pool) {
	for _, tc := range []struct {
		name        string
		credential  bool
		splitModels bool
		disjoint    bool
		disabled    bool
		wantCode    string
	}{
		{name: "credential cooldown", credential: true, wantCode: "code_account_unavailable"},
		{name: "different model cooldowns", splitModels: true, wantCode: "code_connection_model_required"},
		{name: "disjoint capabilities", disjoint: true, wantCode: "code_connection_model_required"},
		{name: "disjoint capabilities cooling", credential: true, disjoint: true, wantCode: "code_connection_model_required"},
		{name: "no usable accounts", disabled: true, wantCode: "code_account_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journey := seedCooldownJourney(t, pool)
			accounts := []string{journey.first, journey.second}
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
				if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET models=CASE WHEN id=$1 THEN '["model"]'::jsonb ELSE '["other"]'::jsonb END WHERE id=ANY($2::uuid[])`, journey.first, accounts); err != nil {
					t.Fatal(err)
				}
			}
			if tc.disabled {
				if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=ANY($1::uuid[])`, accounts); err != nil {
					t.Fatal(err)
				}
			}
			providers := []string{journey.provider}
			conversation := uuid.NewString()
			_, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "model-less-" + conversation}, "", providers)
			var refusal *codemode.Refusal
			if !errors.As(err, &refusal) || refusal.Code != tc.wantCode {
				t.Fatalf("model-less refusal = %v (%+v), want %s", err, refusal, tc.wantCode)
			}
			if tc.credential && (refusal.RetryAt == nil || !refusal.RetryAt.Equal(now.Add(10*time.Minute))) {
				t.Fatalf("credential retry = %+v, want %s", refusal.RetryAt, now.Add(10*time.Minute))
			}
			for _, model := range journey.route.Models {
				_, err := journey.store.BindConnection(t.Context(), journey.route, journey.key, codemode.Identity{Conversation: "hinted-" + model + "-" + conversation}, model, providers)
				if (tc.credential || tc.disabled) && err == nil || !tc.credential && !tc.disabled && err != nil {
					t.Errorf("hinted connection for %s = %v", model, err)
				}
			}
		})
	}
}

func codeModelCooldownFollowsAliasTarget(t *testing.T, pool *pgxpool.Pool) {
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
		_, err := journey.store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: alias, Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{journey.provider}})
		var refusal *codemode.Refusal
		if !errors.As(err, &refusal) || refusal.Code != "code_account_unavailable" || refusal.RetryAt == nil || !refusal.RetryAt.Equal(reset) {
			t.Fatalf("cooled target via %s = %v (%+v), want refusal until %s", alias, err, refusal, reset)
		}
	}
	checkCatalog := func(want []string) {
		t.Helper()
		offers, err := codesessions.Offers(t.Context(), pool, route.RevisionID, journey.key)
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
	permit, err := journey.store.Admit(t.Context(), codesessions.Admission{Route: republished, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "first-alias", Identity: codemode.Identity{Conversation: uuid.NewString()}}, Providers: []string{journey.provider}})
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

func codeRecoveryRecordsActualDestinationSelection(t *testing.T, pool *pgxpool.Pool) {
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
	admission.Recovery = &codesessions.Recovery{From: first.Attempt.ID, Envelope: "codex_usage_limit_reached", Number: 1}
	recovered, err := journey.store.Admit(t.Context(), admission)
	if err != nil || recovered.Attempt.Model != "other" {
		t.Fatalf("model-pool recovery = %+v, %v", recovered, err)
	}
	decision := codeDecisionForAttempt(t, pool, recovered.Attempt.ID)
	if decision.Reason != codemode.DecisionChosen || decision.ChosenAccountID != recovered.Account.ID || decision.Band != nil || decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 || len(decision.Skipped) != 1 || decision.Skipped[0].Class != codemode.SkipWeight || decision.Skipped[0].AccountID == recovered.Account.ID {
		t.Fatalf("recovery decision = %+v", decision)
	}
}

func codeTreeRefusalUsesPreferredAccountRecovery(t *testing.T, pool *pgxpool.Pool) {
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
	_, err = journey.store.Admit(t.Context(), codesessions.Admission{Route: journey.route, APIKeyID: journey.key, Operation: codemode.Operation{Name: "responses", Model: "other", Identity: codemode.Identity{Conversation: "tree-recovery"}}, Providers: []string{journey.provider, provider}})
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.RetryAt == nil || !refusal.RetryAt.Equal(preferredRetry) {
		t.Fatalf("tree refusal = %v (%+v), want preferred account recovery at %s", err, refusal, preferredRetry)
	}
}

func TestCodeActiveAllowanceJourneys(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	t.Run("Codex", func(t *testing.T) { codeActiveAllowanceDoesNotAffectBands(t, pool) })
	t.Run("MiniMax", func(t *testing.T) { codeMiniMaxActiveRemainsDoesNotAffectBands(t, pool) })
}

func codeActiveAllowanceDoesNotAffectBands(t *testing.T, pool *pgxpool.Pool) {
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
	store := codesessions.New(pool)
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
	permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "responses", Model: "model", Identity: codemode.Identity{Conversation: "active-conv-0"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != active {
		t.Fatalf("chosen account = %s", permit.Account.ID)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Band == nil || *decision.Band != codemode.BandUnknown || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
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

func codeMiniMaxActiveRemainsDoesNotAffectBands(t *testing.T, pool *pgxpool.Pool) {
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
	store := codesessions.New(pool)
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
	permit, err := store.Admit(t.Context(), codesessions.Admission{Route: route, APIKeyID: key, Operation: codemode.Operation{Name: "messages", Model: "MiniMax-M3", Identity: codemode.Identity{Conversation: "minimax-active-conv-0"}}, Providers: []string{provider}})
	if err != nil {
		t.Fatal(err)
	}
	if permit.Account.ID != active {
		t.Fatalf("chosen account = %s", permit.Account.ID)
	}
	decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
	if decision.Band == nil || *decision.Band != codemode.BandUnknown || decision.Evidence != nil || decision.EvidenceObservedAt != nil {
		t.Fatalf("MiniMax active band decision = %+v", decision)
	}
}

func checkCodeAllowanceReadRequestEndpoint(t *testing.T, s *service, owner *caller) {
	t.Helper()
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
	journey := seedCooldownJourney(t, s.pool)
	reset := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id IN ($1,$3)`, journey.first, bandAllowanceJSON(t, 0, &reset, time.Now().UTC()), journey.second); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "model", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeModel, RetryAt: reset, Envelope: "fixture_rate"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := journey.store.ObserveCooldownClass(t.Context(), journey.second, "", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeCredential, RetryAt: reset, Envelope: "fixture_rate"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	listing := s.want(owner, "GET", "/api/v1/code/accounts?project_id="+journey.route.ProjectID, nil, nil, http.StatusOK)
	if len(listing["items"].([]any)) != 2 {
		t.Fatalf("management accounts = %v", listing)
	}
	for _, item := range listing["items"].([]any) {
		account := item.(map[string]any)
		if account["id"] == journey.first && account["eligible"] != true {
			t.Fatalf("account with another usable model is ineligible: %v", account)
		}
		if account["id"] == journey.second && account["eligible"] != false {
			t.Fatalf("account-wide cooldown remains eligible: %v", account)
		}
	}
	permit, err := journey.store.Admit(t.Context(), journey.admission("other", "allowance-model-scope"))
	if err != nil || permit.Account.ID != journey.first {
		t.Fatalf("unrelated model admission = %+v, %v", permit, err)
	}
	_, err = journey.store.Admit(t.Context(), journey.admission("model", "allowance-model-blocked"))
	var refusal *codemode.Refusal
	if !errors.As(err, &refusal) || refusal.RetryAt == nil || !refusal.RetryAt.Equal(reset) {
		t.Fatalf("blocked model refusal = %v", err)
	}
	offers, err := codesessions.Offers(t.Context(), s.pool, journey.route.RevisionID)
	if err != nil || len(offers) != 1 || !slices.Equal(offers[0].Models, []string{"other"}) {
		t.Fatalf("model-scoped offers = %+v, %v", offers, err)
	}

	if err := journey.store.ObserveCooldownClass(t.Context(), journey.first, "other", codeadapter.Classification{Class: codeadapter.CooldownRate, Scope: codeadapter.ScopeModel, RetryAt: reset, Envelope: "fixture_rate"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	blockedListing := s.want(owner, "GET", "/api/v1/code/accounts?project_id="+journey.route.ProjectID, nil, nil, http.StatusOK)
	if len(blockedListing["items"].([]any)) != 2 {
		t.Fatalf("blocked management accounts = %v", blockedListing)
	}
	for _, item := range blockedListing["items"].([]any) {
		if item.(map[string]any)["eligible"] != false {
			t.Fatalf("account without any usable models remains eligible: %v", item)
		}
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

func TestCodeAccountPromotionCycleAnchor(t *testing.T) {
	pool, _ := newDatabase(t)
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	user, project, provider, credential, account := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	anchor, changedAnchor, etag := "2026-10-04", "2026-10-12", uuid.NewString()
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.users(id, email, display_name, role, etag) VALUES($1, 'cycle-anchor@example.invalid', 'Journey', 'owner', $2)`, []any{user, uuid.NewString()}},
		{`INSERT INTO olp.projects(id, name, etag, created_by) VALUES($1, 'cycle-anchor-journey', $2, $3)`, []any{project, uuid.NewString(), user}},
		{`INSERT INTO olp.providers(id, project_id, name, kind, state, configuration, etag, slots_etag, created_by) VALUES($1, $2, 'cycle-anchor-provider', 'plugin', 'active', '{"kind":"plugin","auth_mode":"grant","profile_id":"qwen-token-plan"}', $3, $4, $5)`, []any{provider, project, uuid.NewString(), uuid.NewString(), user}},
		{`INSERT INTO olp.provider_credentials(id, provider_id, version, principal, plugin_digest, grant_facts) VALUES($1, $2, 1, 'cycle-anchor-principal', repeat('0',64), '{}')`, []any{credential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
		{`INSERT INTO olp.code_accounts(id, project_id, provider_id, credential_id, principal, name, models, cycle_anchor, etag, created_by) VALUES($1, $2, $3, $4, 'cycle-anchor-principal', 'cycle-anchor-account', '["model"]', $5, $6, $7)`, []any{account, project, provider, credential, anchor, etag, user}},
	} {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name   string
		anchor *string
		action string
	}{
		{"unchanged", &anchor, "noop"},
		{"cleared", nil, "replace"},
		{"changed", &changedAnchor, "replace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			doc := &promotion.Document{
				Code:         &promotion.CodeConfiguration{Accounts: []promotion.CodeAccountEntry{{ID: account, Project: "cycle-anchor-journey", Provider: "cycle-anchor-provider", CredentialRef: "account-credential", Name: "cycle-anchor-account", Enabled: true, Models: []string{"model"}, CycleAnchor: tc.anchor}}},
				CodeBindings: promotion.CodeBindings{Credentials: map[string]string{"account-credential": credential}},
			}
			var plan promotion.PlanResult
			if err := codepromotion.PlanCode(t.Context(), tx, doc, &plan); err != nil {
				t.Fatal(err)
			}
			if len(plan.Actions) != 1 || plan.Actions[0].Kind != "code_account" || plan.Actions[0].Action != tc.action {
				t.Fatalf("promotion plan = %+v, want one account %s", plan.Actions, tc.action)
			}
			operator := access.Principal{User: access.User{ID: user}, AllProjects: true}
			if err := codepromotion.ApplyCode(t.Context(), tx, operator, doc); err != nil {
				t.Fatal(err)
			}
			var storedAnchor *string
			var storedETag string
			if err := tx.QueryRow(t.Context(), `SELECT cycle_anchor::text,etag FROM olp.code_accounts WHERE id=$1`, account).Scan(&storedAnchor, &storedETag); err != nil {
				t.Fatal(err)
			}
			if (storedAnchor == nil) != (tc.anchor == nil) || storedAnchor != nil && tc.anchor != nil && *storedAnchor != *tc.anchor {
				t.Fatalf("stored cycle anchor = %v, want %v", storedAnchor, tc.anchor)
			}
			if tc.action == "noop" && storedETag != etag {
				t.Fatal("unchanged account was replaced")
			}
		})
	}
}

func TestCodeExactModelOrderingThroughGateway(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	proxyURL, roots := newCodeVendorProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_" + uuid.NewString(), "object": "response", "status": "completed", "output": []any{}, "fixture_account": r.Header.Get("Chatgpt-Account-Id")})
	}))
	journey, accounts, key := newCodeGatewayJourney(t, s, owner, proxyURL, roots)
	consumer := &caller{name: "ordering", token: key.secret}
	published := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": journey.route.ProjectID, "slug": journey.route.Slug + "-http", "pool_id": journey.route.PoolID, "models": []string{"model"}, "enabled": true, "allowance_ordering": true}, idempotent(), http.StatusCreated)
	s.want(owner, "POST", "/api/v1/code/routes/"+published["id"].(string)+"/publish", nil, idempotent(etag(published)), http.StatusOK)
	slug := published["slug"].(string)
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, accounts[0], bandAllowanceJSON(t, 75, &reset, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, accounts[1], bandAllowanceJSON(t, 0, &reset, now)); err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"model": "model", "input": "hello"}
	s.eventually("advisory ordering reaches the gateway", 20*time.Second, func() (bool, string) {
		status, raw := s.do(consumer, "POST", "/code/"+slug+"/responses", request, nil)
		if status != http.StatusOK {
			return false, describe(status, raw)
		}
		var response map[string]any
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		if response["fixture_account"] != "account-0" {
			t.Fatalf("healthy account did not win: %s", raw)
		}
		return true, ""
	})
	call := func(thread string) string {
		status, raw := s.do(consumer, "POST", "/code/"+slug+"/responses", request, map[string]string{"thread-id": thread})
		if status != http.StatusOK {
			t.Fatalf("gateway selection failed: %s", describe(status, raw))
		}
		var response struct {
			Account string `json:"fixture_account"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			t.Fatal(err)
		}
		return response.Account
	}
	for _, phase := range []struct {
		name   string
		first  []byte
		second []byte
	}{
		{name: "unknown before low", second: bandAllowanceJSON(t, 10, &reset, now)},
		{name: "low before exhausted", first: bandAllowanceJSON(t, 10, &reset, now), second: bandAllowanceJSON(t, 0, &reset, now)},
	} {
		for index, allowance := range [][]byte{phase.first, phase.second} {
			if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, accounts[index], allowance); err != nil {
				t.Fatal(err)
			}
		}
		if selected := call(uuid.NewString()); selected != "account-0" {
			t.Fatalf("%s selected %s", phase.name, selected)
		}
	}
	for index, account := range accounts {
		if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, bandAllowanceJSON(t, 75, &reset, now)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_pool_accounts SET weight=$3 WHERE pool_id=$1 AND account_id=$2`, journey.route.PoolID, account, []int{3, 1}[index]); err != nil {
			t.Fatal(err)
		}
	}
	weightedThread := func(weightChangesWinner bool) string {
		for index := range 1000 {
			thread := fmt.Sprintf("gateway-share-%d", index)
			affinity := []byte(key.id + ":" + thread)
			first := routing.Score(uuid.MustParse(published["id"].(string)), uuid.MustParse(accounts[0]), 1, "responses", "code", "model", affinity)
			second := routing.Score(uuid.MustParse(published["id"].(string)), uuid.MustParse(accounts[1]), 1, "responses", "code", "model", affinity)
			if second > first && (3*first > second) == weightChangesWinner {
				return thread
			}
		}
		t.Fatal("could not find a request distinguishing account weights")
		return ""
	}
	if selected := call(weightedThread(true)); selected != "account-0" {
		t.Fatalf("higher weight did not change the gateway selection: %s", selected)
	}
	if selected := call(weightedThread(false)); selected != "account-1" {
		t.Fatalf("lower-weight account did not share gateway requests: %s", selected)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, accounts[1], bandAllowanceJSON(t, 0, &reset, now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, accounts[0]); err != nil {
		t.Fatal(err)
	}
	status, raw := s.do(consumer, "POST", "/code/"+slug+"/responses", request, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"fixture_account":"account-1"`) {
		t.Fatalf("last exhausted account excluded: %s", describe(status, raw))
	}
	checkCodeAllowanceReadRequestEndpoint(t, s, owner)
}

func codeBandsAssessEachAdapter(t *testing.T, pool *pgxpool.Pool) {
	journey := seedCooldownJourney(t, pool)
	provider, credential, account := uuid.NewString(), uuid.NewString(), uuid.NewString()
	statements := []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.providers(id,project_id,name,kind,state,configuration,etag,slots_etag,created_by) SELECT $1,project_id,'mixed',kind,state,configuration,etag,slots_etag,created_by FROM olp.providers WHERE id=$2`, []any{provider, journey.provider}},
		{`INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,grant_facts,profile_id) VALUES($1,$2,1,'mixed',repeat('0',64),'{}','minimax-plan')`, []any{credential, provider}},
		{`INSERT INTO olp.provider_grants(credential_id) VALUES($1)`, []any{credential}},
		{`INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,models,etag,created_by) SELECT $1,project_id,$2,$3,'mixed','mixed',models,etag,created_by FROM olp.code_accounts WHERE id=$4`, []any{account, provider, credential, journey.second}},
		{`INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, []any{journey.route.PoolID, account}},
		{`UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, []any{journey.second}},
		{`UPDATE olp.code_route_revisions SET connections=connections || jsonb_build_object($2::text,jsonb_build_object('kind','plugin','auth_mode','grant','profile_id','minimax-plan','profile_revision',repeat('0',64))) WHERE id=$1`, []any{journey.route.RevisionID, provider}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, account, bandAllowanceJSON(t, 75, &reset, now)); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []struct {
		remaining float64
		want      string
		band      codemode.Band
	}{{5, account, codemode.BandUnknown}, {75, journey.first, codemode.BandHealthy}} {
		if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2 WHERE id=$1`, journey.first, bandAllowanceJSON(t, phase.remaining, &reset, now)); err != nil {
			t.Fatal(err)
		}
		admission := journey.admission("model", uuid.NewString())
		admission.Route.AllowanceOrdering = true
		admission.Providers = []string{journey.provider, provider}
		permit, err := journey.store.Admit(t.Context(), admission)
		if err != nil {
			t.Fatal(err)
		}
		decision := codeDecisionForAttempt(t, pool, permit.Attempt.ID)
		if permit.Account.ID != phase.want || decision.Band == nil || *decision.Band != phase.band {
			t.Fatalf("adapter bands chose %s with %+v, want %s %s", permit.Account.ID, decision, phase.want, phase.band)
		}
	}
}

func checkCodePublicationPinsTranslatedOutputContract(t *testing.T, s *service, owner *caller) {
	t.Helper()
	project := s.want(owner, "POST", "/api/v1/projects", map[string]any{"name": "Output contract"}, idempotent(), http.StatusCreated)["id"].(string)
	pool := s.want(owner, "POST", "/api/v1/code/pools", map[string]any{"project_id": project, "name": "Output contract", "kind": "shared", "account_ids": []string{}, "api_key_ids": []string{}, "account_weights": map[string]int{}}, idempotent(), http.StatusCreated)
	route := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": project, "slug": "output-contract", "pool_id": pool["id"], "models": []string{"model"}, "enabled": false, "responses_to_antigravity": true, "responses_antigravity_compatibility": true, "messages_codex_safety_identifier": true, "messages_to_grok": true, "messages_grok_safety_identifier": true}, idempotent(), http.StatusCreated)
	path := "/api/v1/code/routes/" + route["id"].(string)
	first := s.want(owner, "POST", path+"/publish", nil, idempotent(etag(route)), http.StatusOK)
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_route_revisions SET document=document-'translated_codex_output_minimum'-'messages_codex_safety_identifier'-'messages_to_grok'-'messages_grok_safety_identifier'-'responses_to_antigravity'-'responses_antigravity_compatibility' WHERE id=$1`, first["revision_id"]); err != nil {
		t.Fatal(err)
	}
	s.want(owner, "POST", path+"/publish", nil, idempotent(etag(first)), http.StatusOK)
	revisions := s.want(owner, "GET", path+"/revisions", nil, nil, http.StatusOK)
	rows := revisions["items"].([]any)
	if len(rows) != 2 {
		t.Fatalf("revisions = %v", revisions)
	}
	for _, row := range rows {
		item := row.(map[string]any)
		contract := item["route"].(map[string]any)["translated_codex_output_minimum"]
		identity := item["route"].(map[string]any)["messages_codex_safety_identifier"]
		grok := item["route"].(map[string]any)["messages_to_grok"]
		grokSafety := item["route"].(map[string]any)["messages_grok_safety_identifier"]
		antigravity := item["route"].(map[string]any)["responses_to_antigravity"]
		antigravityCompatibility := item["route"].(map[string]any)["responses_antigravity_compatibility"]
		if item["id"] == first["revision_id"] {
			if contract != nil || identity != nil || grok != nil || grokSafety != nil || antigravity != nil || antigravityCompatibility != nil {
				t.Fatalf("legacy revision changed contract to %v", contract)
			}
			continue
		}
		if contract != true || identity != true || grok != true || grokSafety != true || antigravity != true || antigravityCompatibility != true {
			t.Fatalf("republished revision missing output contract, got %v", contract)
		}
	}

}

type codeGatewayKey struct{ id, secret string }

func newCodeGatewayJourney(t *testing.T, s *service, owner *caller, proxyURL, roots string) (cooldownJourney, []string, codeGatewayKey) {
	t.Helper()
	journey := seedCooldownJourney(t, s.pool)
	var user string
	if err := s.pool.QueryRow(t.Context(), `SELECT created_by::text FROM olp.projects WHERE id=$1`, journey.route.ProjectID).Scan(&user); err != nil {
		t.Fatal(err)
	}
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "ordering", "scopes": []string{"inference"}, "project_id": journey.route.ProjectID}, idempotent(), http.StatusCreated)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, journey.route.PoolID, key["id"]); err != nil {
		t.Fatal(err)
	}
	master, err := os.ReadFile(s.env.values["OLP_MASTER_KEY_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing(master)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("1", 64)
	manifest, err := json.Marshal(codexauth.Manifest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by,approved_by,approved_at) VALUES($1,1,$2,'\x00',1,$3,$4,$4,now()) ON CONFLICT(digest) DO NOTHING`, digest, manifest, uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	address := codexauth.Upstream
	config := providers.Configuration{
		Identity: providerconfig.Identity[*string]{Kind: "plugin", AuthMode: "grant", ProfileID: codexauth.ProfileID, ProfileRevision: digest, Endpoint: &address},
		Options:  providers.Options{Network: &egress.ConnectionOptions{ProxyURL: proxyURL, TrustRootsPEM: roots}},
	}
	configuration, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.providers SET configuration=$2 WHERE id=$1`, journey.provider, configuration); err != nil {
		t.Fatal(err)
	}
	accounts := []string{uuid.NewString(), uuid.NewString()}
	for index, source := range []string{journey.first, journey.second} {

		provider := func() string {
			if index == 0 {
				return journey.provider
			}
			id := uuid.NewString()
			if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.providers(id,project_id,name,kind,state,configuration,etag,slots_etag,created_by) SELECT $1,project_id,'gateway-second',kind,state,configuration,$3,$4,created_by FROM olp.providers WHERE id=$2`, id, journey.provider, uuid.NewString(), uuid.NewString()); err != nil {
				t.Fatal(err)
			}
			return id
		}()
		account := accounts[index]
		payload, err := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": fmt.Sprintf("account-%d", index), "chatgpt_user_id": "fixture-user"}})
		if err != nil {
			t.Fatal(err)
		}
		token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
		identity, err := codexauth.TokenIdentity(token, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		served, err := json.Marshal(connectors.GrantCredential{PluginDigest: digest, ProfileID: codexauth.ProfileID, AccessToken: token, Facts: identity.Facts()})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := s.pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		credential := uuid.NewString()
		var installation string
		facts, err := json.Marshal(identity.Facts())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,grant_facts,profile_id) VALUES($1,$2,$3,$4,$5,$6,'codex-subscription')`, credential, provider, index+20, identity.Principal(), digest, facts); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.provider_grants(credential_id,expires_at) VALUES($1,now()+interval '1 hour')`, credential); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,models,etag,created_by) SELECT $1,project_id,$5,$2,$3,name,models,etag,created_by FROM olp.code_accounts WHERE id=$4`, account, credential, identity.Principal(), source, provider); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(t.Context(), `SELECT id::text FROM olp.installation WHERE singleton`).Scan(&installation); err != nil {
			t.Fatal(err)
		}
		if err := ring.Store(t.Context(), tx, installation, credential, secrets.ProviderCredential, served, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, journey.route.PoolID, account); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, source); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_routes SET latest_revision_id=NULL WHERE id=$1`, journey.route.ID); err != nil {
		t.Fatal(err)
	}

	return journey, accounts, codeGatewayKey{id: key["id"].(string), secret: key["secret"].(string)}
}

type codeRateProbe struct {
	recover  bool
	fallback bool
	calls    atomic.Int64
}

type codeRateCase struct {
	path, phase, scope string
	secret, slug       string
	body               json.RawMessage
}

func TestCodeTranslatedRateReservationsThroughGateway(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	var active atomic.Pointer[codeRateProbe]
	proxy, roots := newCodeVendorProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		probe := active.Load()
		if probe == nil {
			http.Error(w, "fixture not ready", http.StatusServiceUnavailable)
			return
		}
		probe.calls.Add(1)
		var input struct {
			Model string `json:"model"`
			Cap   int    `json:"max_output_tokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if probe.fallback {
			expected := func() string {
				if r.Header.Get("Chatgpt-Account-Id") == "account-0" {
					return "first"
				}
				return "second"
			}()
			if input.Model != expected {
				t.Errorf("fallback dispatched model %q, want %q", input.Model, expected)
			}
		}
		if input.Cap != 16 {
			t.Errorf("dispatched output cap = %d, want effective minimum", input.Cap)
		}
		w.Header().Set("Content-Type", "application/json")
		if probe.recover && r.Header.Get("Chatgpt-Account-Id") == "account-0" {

			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"type":"usage_limit_reached","message":"fixture refusal","resets_in_seconds":60}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "resp_fixture", "object": "response", "model": input.Model, "status": "completed", "output": []any{map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "recovered"}}}}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}})
	}))
	journey, accounts, _ := newCodeGatewayJourney(t, s, owner, proxy, roots)

	var secondProvider string
	if err := s.pool.QueryRow(t.Context(), `SELECT provider_id::text FROM olp.code_accounts WHERE id=$1`, accounts[1]).Scan(&secondProvider); err != nil {
		t.Fatal(err)
	}

	for index, account := range accounts {
		models, err := json.Marshal([][]string{{"model", "first"}, {"model", "second"}}[index])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET models=$2 WHERE id=$1`, account, models); err != nil {
			t.Fatal(err)
		}
	}
	scenarios := []struct{ path, phase, scope string }{
		{"v1/messages", "initial", "key"},
		{"v1/messages", "initial", "provider"},
		{"v1/messages", "recovery", "key"},
		{"v1/messages", "recovery", "provider"},
		{"v1/messages", "recovery", "control"},
		{"v1/messages", "fallback", "key"},
		{"v1/messages", "fallback", "provider"},
		{"v1/messages", "fallback", "control"},
		{"v1/chat/completions", "initial", "key"},
		{"v1/chat/completions", "initial", "provider"},
		{"v1/chat/completions", "recovery", "key"},
		{"v1/chat/completions", "recovery", "provider"},
		{"v1/chat/completions", "recovery", "control"},
		{"v1/chat/completions", "fallback", "key"},
		{"v1/chat/completions", "fallback", "provider"},
		{"v1/chat/completions", "fallback", "control"},
	}
	prepared := slices.Collect(func(yield func(codeRateCase) bool) {
		for _, tc := range scenarios {
			phase, scope := tc.phase, tc.scope
			model := func() string {
				if phase == "fallback" {
					return "pool"
				}
				return "model"
			}()
			body := json.RawMessage(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"max_tokens":1}`, model))
			capacity := func() int {
				inputEstimate := max(1, len(body)/4)
				callerEstimate := inputEstimate + 1
				if scope == "key" && phase != "initial" {
					firstEffective := inputEstimate + 16
					return firstEffective + callerEstimate + 1
				}
				return callerEstimate + 1
			}()
			key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "effective-cap", "scopes": []string{"inference"}, "project_id": journey.route.ProjectID, "tokens_per_minute": func() int {
				if scope == "key" {
					return capacity
				}
				return 10000
			}()}, idempotent(), http.StatusCreated)
			if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, journey.route.PoolID, key["id"]); err != nil {
				t.Fatal(err)
			}
			for index, provider := range []string{journey.provider, secondProvider} {
				limit := func() string {
					if scope == "provider" && (phase == "initial" || index == 1) {
						return fmt.Sprintf(`{"tokens_per_minute":%d}`, capacity)
					}
					return "null"
				}()
				if _, err := s.pool.Exec(t.Context(), `UPDATE olp.providers SET configuration=jsonb_set(configuration::jsonb,'{options,limits}',$2::jsonb) WHERE id=$1`, provider, limit); err != nil {
					t.Fatal(err)
				}
			}
			route := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": journey.route.ProjectID, "slug": "rate-" + uuid.NewString(), "pool_id": journey.route.PoolID, "models": func() []string {
				if phase == "fallback" {
					return []string{"first", "second"}
				}
				return []string{"model"}
			}(), "aliases": func() map[string]string {
				if phase == "fallback" {
					return map[string]string{"pool": "first"}
				}
				return nil
			}(), "enabled": true, "messages_to_codex": true, "chat_to_codex": true, "allowance_ordering": true, "initial_recovery_attempts": 1, "model_rewriting": phase == "fallback", "model_pools": func() map[string][]string {
				if phase == "fallback" {
					return map[string][]string{"pool": {"first", "second"}}
				}
				return nil
			}()}, idempotent(), http.StatusCreated)
			s.want(owner, "POST", "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK)
			if !yield(codeRateCase{path: tc.path, phase: phase, scope: scope, secret: key["secret"].(string), slug: route["slug"].(string), body: body}) {
				return
			}
		}
	})
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	for _, tc := range prepared {
		path, phase, scope := tc.path, tc.phase, tc.scope
		if _, err := s.pool.Exec(t.Context(), `DELETE FROM olp.code_account_cooldowns WHERE account_id=ANY($1::uuid[])`, accounts); err != nil {
			t.Fatal(err)
		}
		for index, account := range accounts {
			if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_accounts SET allowance=$2,health='healthy',unavailable_until=NULL WHERE id=$1`, account, bandAllowanceJSON(t, []float64{75, 10}[index], &reset, now)); err != nil {
				t.Fatal(err)
			}
		}
		probe := &codeRateProbe{recover: phase != "initial", fallback: phase == "fallback"}
		active.Store(probe)
		consumer := &caller{name: "effective-cap", token: tc.secret}
		status, raw := func() (int, []byte) {
			for deadline := time.Now().Add(5 * time.Second); ; {
				status, raw := s.do(consumer, "POST", "/code/"+tc.slug+"/"+path, tc.body, nil)
				if status != http.StatusUnauthorized && !(status == http.StatusNotFound && strings.Contains(string(raw), "code_route_unavailable")) || time.Now().After(deadline) {
					return status, raw
				}
				time.Sleep(25 * time.Millisecond)
			}
		}()
		expectedCalls := func() int64 {
			if phase == "initial" {
				return 0
			}
			if scope == "control" {
				return 2
			}
			return 1
		}()
		code := func() string {
			if scope == "provider" {
				return "code_provider_rate_limited"
			}
			return "code_rate_limited"
		}()
		if scope == "control" {
			if status != http.StatusOK || !strings.Contains(string(raw), "recovered") {
				t.Fatalf("%s %s control = %s", path, phase, describe(status, raw))
			}
		} else if (status != http.StatusBadRequest && status != http.StatusTooManyRequests) || !strings.Contains(string(raw), code) {
			t.Fatalf("%s %s %s = %s", path, phase, scope, describe(status, raw))
		}
		if count := probe.calls.Load(); count != expectedCalls {
			t.Fatalf("%s %s %s dispatched %d attempts, want %d", path, phase, scope, count, expectedCalls)
		}
	}
}

type signedJourneyPart struct {
	Text      string          `json:"text,omitempty"`
	Call      json.RawMessage `json:"functionCall"`
	Signature string          `json:"thoughtSignature"`
	Response  json.RawMessage `json:"functionResponse"`
}

type signedJourneyContent struct {
	Parts []signedJourneyPart `json:"parts"`
}

func TestCodeSignedToolContinuation(t *testing.T) {
	s := startService(t)
	owner := s.owner()
	checkCodePublicationPinsTranslatedOutputContract(t, s, owner)
	var calls atomic.Int64
	proxy, roots := newCodeVendorProxy(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Model   string `json:"model"`
			Request struct {
				Contents          []signedJourneyContent `json:"contents"`
				SystemInstruction json.RawMessage        `json:"systemInstruction"`
			} `json:"request"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if body.Model != "gemini-fixture" {
			t.Errorf("model = %s", body.Model)
		}
		if bytes.Contains(body.Request.SystemInstruction, []byte("Summarize this complete conversation")) {
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"Preserve the PostgreSQL migration constraints"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10}}}`)
			return
		}
		encodedRequest, _ := json.Marshal(body.Request)
		if bytes.Contains(encodedRequest, []byte("Conversation summary")) {
			_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"The PostgreSQL migration constraints are restored"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10}}}`)
			return
		}
		continued := slices.ContainsFunc(body.Request.Contents, func(content signedJourneyContent) bool {
			return slices.ContainsFunc(content.Parts, func(part signedJourneyPart) bool { return len(part.Response) != 0 })
		})
		parts := `[{"functionCall":{"name":"read_file","args":{"path":"sample"}},"thoughtSignature":"private-signature"},{"functionCall":{"name":"list_files","args":{}},"thoughtSignature":"private-second"}]`
		if continued {
			encoded, _ := json.Marshal(body)
			if !strings.Contains(string(encoded), `"thoughtSignature":"private-signature"`) || !strings.Contains(string(encoded), "file result") {
				t.Errorf("missing restored continuation")
			}
			parts = `[{"text":"continued after restart"}]`
		}
		result := `{"response":{"candidates":[{"content":{"role":"model","parts":` + parts + `},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":2,"totalTokenCount":10}}}`
		if strings.Contains(r.URL.Path, "streamGenerateContent") {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: %s\n\n", result)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(result))
	}))
	journey, consumer := newSignedCodeJourney(t, s, owner, proxy, roots)
	summarySetup := prepareSummaryJourney(t, s, owner, journey)
	configuration, configErr := codeclient.Generate(journey.route, codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"gemini-fixture"}}, {Adapter: codemode.AdapterAntigravity, Models: []string{"gemini-fixture"}}}, codeclient.Request{GatewayURL: s.origin, Client: codeadapter.ClientCodex, Model: "gemini-fixture"})
	if configErr != nil || !strings.Contains(configuration.Configuration, "supports_websockets = false") || !strings.Contains(configuration.Configuration, `web_search = "disabled"`) {
		t.Fatalf("HTTP Codex configuration = %s, %v", configuration.Configuration, configErr)
	}
	surfaceJourneys := []signedSurfaceJourney{startSignedSurfaceJourney(t, s, consumer, journey.route.Slug, "responses"), startSignedSurfaceJourney(t, s, consumer, journey.route.Slug, "responses-compatible"), startSignedSurfaceJourney(t, s, consumer, journey.route.Slug, "messages")}
	path := "/code/" + journey.route.Slug + "/v1/chat/completions"
	initial := json.RawMessage(`{"model":"gemini-fixture","messages":[{"role":"user","content":"Inspect"}]}`)
	headers := map[string]string{"X-Opencode-Session-Id": "signed-root"}
	status, raw := func() (int, []byte) {
		for deadline := time.Now().Add(5 * time.Second); ; {
			status, raw := s.do(consumer, "POST", path, initial, headers)
			if status == http.StatusOK || time.Now().After(deadline) {
				return status, raw
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	if status != http.StatusOK || strings.Contains(string(raw), "private-signature") {
		t.Fatalf("initial = %s", describe(status, raw))
	}
	var result struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || len(result.Choices) != 1 {
		t.Fatalf("initial = %s, %v", raw, err)
	}
	var message struct {
		ToolCalls []struct {
			ID string `json:"id"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(result.Choices[0].Message, &message); err != nil || len(message.ToolCalls) != 2 {
		t.Fatalf("message = %s, %v", result.Choices[0].Message, err)
	}
	continuation := json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture","messages":[{"role":"user","content":"Inspect"},%s,{"role":"tool","tool_call_id":%q,"content":"directory result"},{"role":"tool","tool_call_id":%q,"content":"file result"}]}`, result.Choices[0].Message, message.ToolCalls[1].ID, message.ToolCalls[0].ID))
	partial := json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture","messages":[{"role":"user","content":"Inspect"},%s,{"role":"tool","tool_call_id":%q,"content":"file result"}]}`, result.Choices[0].Message, message.ToolCalls[0].ID))
	premature := json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture","messages":[{"role":"user","content":"Inspect"},{"role":"tool","tool_call_id":%q,"content":"file result"},%s,{"role":"tool","tool_call_id":%q,"content":"directory result"}]}`, message.ToolCalls[0].ID, result.Choices[0].Message, message.ToolCalls[1].ID))
	before := calls.Load()
	for _, request := range []struct {
		body    json.RawMessage
		session string
		code    string
	}{{partial, "signed-root", "code_continuation_unavailable"}, {premature, "signed-root", "code_translation_unsupported_semantics"}, {continuation, "foreign-root", "code_continuation_unavailable"}, {json.RawMessage(strings.Replace(string(continuation), "sample", "altered", 1)), "signed-root", "code_continuation_unavailable"}} {
		status, raw := s.do(consumer, "POST", path, request.body, map[string]string{"X-Opencode-Session-Id": request.session})
		if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), request.code) || calls.Load() != before {
			t.Fatalf("mismatch = %s, vendor calls %d", describe(status, raw), calls.Load()-before)
		}
	}
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE olp.code_continuations ADD CONSTRAINT fixture_signed_save_failure CHECK (false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	beforeFailure := calls.Load()
	status, raw = s.do(consumer, "POST", path, json.RawMessage(`{"model":"gemini-fixture","stream":true,"messages":[{"role":"user","content":"read sample"}]}`), map[string]string{"X-Opencode-Session-Id": "save-failure-root"})
	if calls.Load() != beforeFailure+1 || strings.Contains(string(raw), "read_file") || strings.Contains(string(raw), "private-signature") || !strings.Contains(string(raw), "code_continuation_unavailable") {
		t.Fatalf("failed save = %s, calls %d", describe(status, raw), calls.Load())
	}
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE olp.code_continuations DROP CONSTRAINT fixture_signed_save_failure`); err != nil {
		t.Fatal(err)
	}
	var reported int64
	if err := s.pool.QueryRow(t.Context(), `SELECT a.reported_tokens FROM olp.code_attempts a JOIN olp.code_bindings b ON b.id=a.binding_id WHERE b.conversation='save-failure-root' AND a.state='settled'`).Scan(&reported); err != nil || reported != 10 {
		t.Fatalf("failed-save accounting=%d, %v", reported, err)
	}
	summary := startSummaryJourney(t, s, owner, journey, summarySetup, consumer, &calls)
	rotateSummarySecrets(t, s)
	beforeRestart := calls.Load()
	s.stop()
	restarted := startProcess(t, s.pool, s.env)
	status, raw = restarted.do(consumer, "POST", path, continuation, headers)
	if status != http.StatusOK || !strings.Contains(string(raw), "continued after restart") || calls.Load() != beforeRestart+1 {
		t.Fatalf("restart = %s", describe(status, raw))
	}
	for _, surface := range surfaceJourneys {
		status, raw := restarted.do(consumer, "POST", surface.path, surface.continuation, surface.headers)
		if status != http.StatusOK || !strings.Contains(string(raw), "continued after restart") {
			t.Fatalf("%s restart = %s", surface.protocol, describe(status, raw))
		}
	}
	continueSummaryJourney(t, restarted, journey, consumer, summary, &calls)
	var root string
	if err := s.pool.QueryRow(t.Context(), `SELECT root_id::text FROM olp.code_bindings WHERE conversation='signed-root' AND route_id=$1`, journey.route.ID).Scan(&root); err != nil {
		t.Fatal(err)
	}
	foreign, err := journey.store.Admit(t.Context(), journey.admission("gemini-fixture", "foreign-bound-root"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_continuations SET root_id=$2 WHERE root_id=$1`, root, foreign.Binding.RootID); err != nil {
		t.Fatal(err)
	}
	status, raw = restarted.do(consumer, "POST", path, continuation, map[string]string{"X-Opencode-Session-Id": "foreign-bound-root"})
	if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "code_continuation_unavailable") {
		t.Fatalf("transplanted state = %s", describe(status, raw))
	}
	if _, err := s.pool.Exec(t.Context(), `UPDATE olp.code_continuations SET root_id=$2 WHERE root_id=$1`, foreign.Binding.RootID, root); err != nil {
		t.Fatal(err)
	}
	restarted.want(owner, "POST", "/api/v1/code/bindings/"+root+"/retire", nil, idempotent(), http.StatusOK)
	var remaining int
	summaryBindingPath := "/api/v1/code/bindings/" + summary.binding
	restarted.want(owner, "POST", summaryBindingPath+"/retire", nil, idempotent(), http.StatusOK)
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.secrets WHERE id=$1`, summary.key).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("retired summary key remains, %d, %v", remaining, err)
	}
	beforeRetiredSummary := calls.Load()
	retiredStatus, retiredBody := restarted.do(consumer, "POST", "/code/"+journey.route.Slug+"/responses", json.RawMessage(`{"model":"gemini-fixture","input":[`+string(summary.item)+`,{"role":"user","content":"Continue"}]}`), map[string]string{"Session-Id": summary.conversation + "-child", "X-Codex-Parent-Thread-Id": summary.conversation})
	if retiredStatus != http.StatusGone || calls.Load() != beforeRetiredSummary {
		t.Fatalf("retired child summary dispatched %d %s", retiredStatus, retiredBody)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.secrets WHERE id=$1 OR id=$2`, message.ToolCalls[0].ID, message.ToolCalls[1].ID).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("retired state count %d, %v", remaining, err)
	}
	status, raw = restarted.do(consumer, "POST", path, continuation, headers)
	if status != http.StatusGone || !strings.Contains(string(raw), "code_binding_retired") {
		t.Fatalf("retired continuation = %s", describe(status, raw))
	}
}

func newSignedCodeJourney(t *testing.T, s *service, owner *caller, proxy, roots string) (cooldownJourney, *caller) {
	t.Helper()
	journey := seedCooldownJourney(t, s.pool)
	var user, installation string
	credential, account := uuid.NewString(), uuid.NewString()
	if err := s.pool.QueryRow(t.Context(), `SELECT created_by::text FROM olp.projects WHERE id=$1`, journey.route.ProjectID).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.installation WHERE singleton`).Scan(&installation); err != nil {
		t.Fatal(err)
	}

	master, err := os.ReadFile(s.env.values["OLP_MASTER_KEY_FILE"])
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing(master)
	if err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(antigravityauth.Manifest())
	digest := strings.Repeat("2", 64)
	address := antigravityauth.Upstream
	configuration, _ := json.Marshal(providers.Configuration{Identity: providerconfig.Identity[*string]{Kind: "plugin", AuthMode: "grant", ProfileID: antigravityauth.ProfileID, ProfileRevision: digest, Endpoint: &address}, Options: providers.Options{PluginOptions: map[string]string{"client_id": "fixture.apps.googleusercontent.com"}, Network: &egress.ConnectionOptions{ProxyURL: proxy, TrustRootsPEM: roots}}})
	facts := map[string]string{"subject": "fixture-subject", "project_id": "fixture-project"}
	principal := antigravityauth.Principal(facts["subject"], facts["project_id"])
	encodedFacts, _ := json.Marshal(facts)
	served, _ := json.Marshal(connectors.GrantCredential{PluginDigest: digest, ProfileID: antigravityauth.ProfileID, AccessToken: "fixture-token", Facts: facts})
	tx, err := s.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO olp.plugins(digest,abi_version,manifest,module,size_bytes,etag,installed_by,approved_by,approved_at) VALUES($1,1,$2,'\x00',1,$3,$4,$4,now())`, []any{digest, manifest, uuid.NewString(), user}},
		{`UPDATE olp.providers SET configuration=$2 WHERE id=$1`, []any{journey.provider, configuration}},
		{`UPDATE olp.code_routes SET latest_revision_id=NULL WHERE id=$1`, []any{journey.route.ID}},
		{`INSERT INTO olp.provider_credentials(id,provider_id,version,principal,plugin_digest,profile_id,grant_facts) VALUES($1,$2,3,$3,$4,$5,$6)`, []any{credential, journey.provider, principal, digest, antigravityauth.ProfileID, encodedFacts}},
		{`INSERT INTO olp.code_accounts(id,project_id,provider_id,credential_id,principal,name,models,etag,created_by) VALUES($1,$2,$3,$4,$5,'signed-tools','["gemini-fixture"]',$6,$7)`, []any{account, journey.route.ProjectID, journey.provider, credential, principal, uuid.NewString(), user}},
		{`INSERT INTO olp.code_pool_accounts(pool_id,account_id,weight) VALUES($1,$2,1)`, []any{journey.route.PoolID, account}},
		{`UPDATE olp.code_accounts SET enabled=false WHERE id IN ($1,$2)`, []any{journey.first, journey.second}},
		{`INSERT INTO olp.provider_grants(credential_id,expires_at) VALUES($1,now()+interval '1 hour')`, []any{credential}},
	} {
		if _, err := tx.Exec(t.Context(), statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := ring.Store(t.Context(), tx, installation, credential, secrets.ProviderCredential, served, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := s.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "signed-tools", "scopes": []string{"inference"}, "project_id": journey.route.ProjectID}, idempotent(), http.StatusCreated)
	if _, err := s.pool.Exec(t.Context(), `INSERT INTO olp.code_pool_keys(pool_id,api_key_id) VALUES($1,$2)`, journey.route.PoolID, key["id"]); err != nil {
		t.Fatal(err)
	}
	route := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": journey.route.ProjectID, "slug": "signed-" + uuid.NewString(), "pool_id": journey.route.PoolID, "models": []string{"gemini-fixture"}, "enabled": true, "chat_to_antigravity": true, "responses_to_antigravity": true, "responses_antigravity_compatibility": true, "messages_to_antigravity": true}, idempotent(), http.StatusCreated)
	published := s.want(owner, "POST", "/api/v1/code/routes/"+route["id"].(string)+"/publish", nil, idempotent(etag(route)), http.StatusOK)
	journey.route.ETag = published["etag"].(string)
	journey.route.ChatToAntigravity, journey.route.ResponsesToAntigravity, journey.route.ResponsesAntigravityCompatibility, journey.route.MessagesToAntigravity = true, true, true, true
	journey.route.ID, journey.route.Slug, journey.route.Models = route["id"].(string), route["slug"].(string), []string{"gemini-fixture"}
	if err := s.pool.QueryRow(t.Context(), `SELECT latest_revision_id::text FROM olp.code_routes WHERE id=$1`, journey.route.ID).Scan(&journey.route.RevisionID); err != nil {
		t.Fatal(err)
	}
	journey.key = key["id"].(string)
	journey.store = codesessions.NewEncrypted(s.pool, installation, ring)
	return journey, &caller{name: "signed-tools", token: key["secret"].(string)}
}

type signedSurfaceJourney struct {
	protocol     string
	path         string
	headers      map[string]string
	continuation json.RawMessage
}

func startSignedSurfaceJourney(t *testing.T, s *service, consumer *caller, slug, protocol string) signedSurfaceJourney {
	t.Helper()
	compatibility := func() string {
		if protocol == "responses-compatible" {
			return `,"store":false,"parallel_tool_calls":true,"prompt_cache_key":"cache-hint","client_metadata":{"turn_id":"fixture-turn"},"reasoning":{"effort":"none"},"include":["reasoning.encrypted_content"]`
		}
		return ""
	}()
	surface := func() signedSurfaceJourney {
		if protocol == "messages" {
			return signedSurfaceJourney{protocol: protocol, path: "/code/" + slug + "/v1/messages", headers: map[string]string{"X-Claude-Code-Session-Id": "signed-messages-root"}, continuation: json.RawMessage(`{"model":"gemini-fixture","max_tokens":8,"messages":[{"role":"user","content":"Inspect"}]}`)}
		}
		return signedSurfaceJourney{protocol: protocol, path: "/code/" + slug + "/responses", headers: map[string]string{"Session-Id": "signed-" + protocol + "-root"}, continuation: json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture","input":"Inspect"%s}`, compatibility))}
	}()
	path, headers, initial := surface.path, surface.headers, surface.continuation
	status, raw := func() (int, []byte) {
		for deadline := time.Now().Add(5 * time.Second); ; {
			status, raw := s.do(consumer, "POST", path, initial, headers)
			if status == http.StatusOK || time.Now().After(deadline) {
				return status, raw
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	bodies := func() [2]json.RawMessage {
		if protocol == "messages" {
			var result struct {
				Content []struct {
					ID string `json:"id"`
				} `json:"content"`
			}
			if status != http.StatusOK || json.Unmarshal(raw, &result) != nil || len(result.Content) != 2 || strings.Contains(string(raw), "private-") {
				t.Fatalf("%s initial = %s", protocol, describe(status, raw))
			}
			var content struct {
				Content json.RawMessage `json:"content"`
			}
			_ = json.Unmarshal(raw, &content)
			prefix := fmt.Sprintf(`{"model":"gemini-fixture","max_tokens":8,"messages":[{"role":"user","content":"Inspect"},{"role":"assistant","content":%s},{"role":"user","content":[`, content.Content)
			return [2]json.RawMessage{json.RawMessage(fmt.Sprintf(`%s{"type":"tool_result","tool_use_id":%q,"content":"directory result"},{"type":"tool_result","tool_use_id":%q,"content":"file result"}]}]}`, prefix, result.Content[1].ID, result.Content[0].ID)), json.RawMessage(fmt.Sprintf(`%s{"type":"tool_result","tool_use_id":%q,"content":"file result"}]}]}`, prefix, result.Content[0].ID))}
		}
		var result struct {
			Output []struct {
				Type      string `json:"type"`
				CallID    string `json:"call_id"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"output"`
		}
		if status != http.StatusOK || json.Unmarshal(raw, &result) != nil || len(result.Output) != 2 || strings.Contains(string(raw), "private-") {
			t.Fatalf("%s initial = %s", protocol, describe(status, raw))
		}
		calls, _ := json.Marshal(result.Output)
		prefix := strings.TrimSuffix(string(calls), "]")
		return [2]json.RawMessage{json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture"%s,"input":%s,{"type":"function_call_output","call_id":%q,"output":"directory result"},{"type":"function_call_output","call_id":%q,"output":"file result"}]}`, compatibility, prefix, result.Output[1].CallID, result.Output[0].CallID)), json.RawMessage(fmt.Sprintf(`{"model":"gemini-fixture"%s,"input":%s,{"type":"function_call_output","call_id":%q,"output":"file result"}]}`, compatibility, prefix, result.Output[0].CallID))}
	}()
	continuation, partial := bodies[0], bodies[1]
	foreignHeaders := func() map[string]string {
		if protocol == "messages" {
			return map[string]string{"X-Claude-Code-Session-Id": "foreign-messages-root"}
		}
		return map[string]string{"Session-Id": "foreign-responses-root"}
	}()
	for _, request := range []struct {
		body    json.RawMessage
		headers map[string]string
	}{
		{partial, headers},
		{continuation, foreignHeaders},
		{json.RawMessage(strings.Replace(string(continuation), "sample", "altered", 1)), headers},
	} {
		status, raw := s.do(consumer, "POST", path, request.body, request.headers)
		if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), "code_continuation_unavailable") {
			t.Fatalf("%s mismatch = %s", protocol, describe(status, raw))
		}
	}
	return signedSurfaceJourney{protocol: protocol, path: path, headers: headers, continuation: continuation}
}

type summaryJourney struct {
	conversation string
	item         json.RawMessage
	binding      string
	key          string
}

type summaryJourneySetup struct {
	frozenSlug  string
	enabledETag string
}

func prepareSummaryJourney(t *testing.T, s *service, owner *caller, journey cooldownJourney) summaryJourneySetup {
	t.Helper()
	frozen := s.want(owner, "POST", "/api/v1/code/routes", map[string]any{"project_id": journey.route.ProjectID, "slug": "summary-frozen-" + uuid.NewString(), "pool_id": journey.route.PoolID, "models": []string{"gemini-fixture"}, "enabled": true, "responses_to_antigravity": true, "responses_antigravity_compatibility": true}, idempotent(), http.StatusCreated)
	frozenPath := "/api/v1/code/routes/" + frozen["id"].(string)
	published := s.want(owner, "POST", frozenPath+"/publish", nil, idempotent(etag(frozen)), http.StatusOK)
	s.want(owner, "PUT", frozenPath, map[string]any{"project_id": journey.route.ProjectID, "slug": frozen["slug"], "pool_id": journey.route.PoolID, "models": []string{"gemini-fixture"}, "enabled": true, "responses_to_antigravity": true, "responses_antigravity_compatibility": true, "summary_compaction": true}, idempotent(etag(published)), http.StatusOK)
	path := "/api/v1/code/routes/" + journey.route.ID
	changed := s.want(owner, "PUT", path, map[string]any{"project_id": journey.route.ProjectID, "slug": journey.route.Slug, "pool_id": journey.route.PoolID, "models": []string{"gemini-fixture"}, "enabled": true, "chat_to_antigravity": true, "responses_to_antigravity": true, "responses_antigravity_compatibility": true, "messages_to_antigravity": journey.route.MessagesToAntigravity, "summary_compaction": true}, idempotent(etag(map[string]any{"etag": journey.route.ETag})), http.StatusOK)
	enabled := s.want(owner, "POST", path+"/publish", nil, idempotent(etag(changed)), http.StatusOK)
	return summaryJourneySetup{frozenSlug: frozen["slug"].(string), enabledETag: enabled["etag"].(string)}
}

func startSummaryJourney(t *testing.T, s *service, owner *caller, journey cooldownJourney, setup summaryJourneySetup, consumer *caller, calls *atomic.Int64) summaryJourney {
	t.Helper()
	conversation := uuid.NewString()
	path := "/code/" + journey.route.Slug + "/responses/compact"
	body := `{"model":"gemini-fixture","instructions":"Keep the requirements","input":[{"role":"user","content":"Inspect the migration"},{"type":"function_call","call_id":"read","name":"read_file","arguments":"{}"},{"type":"function_call_output","call_id":"read","output":"Preserve the PostgreSQL migration constraints"}]}`
	request := func(path, conversation, body string) *http.Response {
		return summarySurfaceRequest(t, s, consumer, conversation, path, body)
	}
	before := calls.Load()
	off := request("/code/"+setup.frozenSlug+"/responses/compact", conversation, body)
	off.Body.Close()
	if off.StatusCode != http.StatusBadRequest || calls.Load() != before {
		t.Fatal("default-off published compaction policy leaked its enabled draft")
	}
	offTrigger := request("/code/"+setup.frozenSlug+"/responses", conversation, `{"model":"gemini-fixture","stream":true,"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`)
	offTrigger.Body.Close()
	if offTrigger.StatusCode != http.StatusUnprocessableEntity || calls.Load() != before {
		t.Fatal("default-off published trigger policy leaked its enabled draft")
	}
	managePath := "/api/v1/code/routes/" + journey.route.ID
	response, encoded := func() (*http.Response, []byte) {
		for deadline := time.Now().Add(5 * time.Second); ; {
			response := request(path, conversation, body)
			encoded, _ := io.ReadAll(response.Body)
			response.Body.Close()
			if response.StatusCode != http.StatusBadRequest || !bytes.Contains(encoded, []byte("code_operation_unsupported")) || time.Now().After(deadline) {
				return response, encoded
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
	var compact struct {
		Output []json.RawMessage `json:"output"`
	}
	if response.StatusCode != http.StatusOK || json.Unmarshal(encoded, &compact) != nil || len(compact.Output) != 1 || !bytes.Contains(compact.Output[0], []byte("olp.summary.v1.")) || bytes.Contains(encoded, []byte("PostgreSQL")) || calls.Load() != before+1 {
		t.Fatalf("compact %d %s, calls %d", response.StatusCode, encoded, calls.Load()-before)
	}
	var binding, secretID string
	if err := s.pool.QueryRow(t.Context(), `SELECT b.id::text,k.id::text FROM olp.code_bindings b JOIN olp.code_summary_keys k ON k.root_id=b.root_id AND k.account_id=b.account_id AND k.principal=b.principal WHERE b.conversation=$1 AND b.route_id=$2`, conversation, journey.route.ID).Scan(&binding, &secretID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_attempts WHERE binding_id=$1 AND operation='compact' AND state='settled' AND reported_tokens=10`, binding).Scan(&count); err != nil || count != 1 {
		t.Fatalf("summary accounting %d, %v", count, err)
	}
	item := compact.Output[0]
	continuation := `{"model":"gemini-fixture","input":[` + string(item) + `,{"role":"user","content":"Continue the migration"}]}`
	for _, invalid := range []struct{ conversation, body string }{
		{uuid.NewString(), continuation},
		{conversation, strings.Replace(continuation, "olp.summary.v1.", "olp.summary.v1.altered", 1)},
		{conversation, `{"model":"gemini-fixture","input":[{"type":"compaction","encrypted_content":"foreign"}]}`},
		{conversation, `{"model":"gemini-fixture","previous_response_id":"unresolved","input":"hello"}`},
		{conversation, `{"model":"gemini-fixture","input":[{"type":"function_call","call_id":"missing","name":"read","arguments":"{}"}]}`},
	} {
		refused := request(path, invalid.conversation, invalid.body)
		refused.Body.Close()
		if refused.StatusCode == http.StatusOK || calls.Load() != before+1 {
			t.Fatalf("unportable summary dispatched, %d", refused.StatusCode)
		}
	}
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE olp.code_summary_keys ADD CONSTRAINT fixture_summary_failure CHECK(false) NOT VALID`); err != nil {
		t.Fatal(err)
	}
	failedConversation := uuid.NewString()
	failed := request(path, failedConversation, body)
	failedBody, _ := io.ReadAll(failed.Body)
	failed.Body.Close()
	if _, err := s.pool.Exec(t.Context(), `ALTER TABLE olp.code_summary_keys DROP CONSTRAINT fixture_summary_failure`); err != nil {
		t.Fatal(err)
	}
	if failed.StatusCode != http.StatusBadGateway || bytes.Contains(failedBody, []byte("encrypted_content")) || calls.Load() != before+2 {
		t.Fatalf("summary persistence replayed %d %s, calls %d", failed.StatusCode, failedBody, calls.Load()-before)
	}
	if err := s.pool.QueryRow(t.Context(), `SELECT count(*) FROM olp.code_attempts a JOIN olp.code_bindings b ON b.id=a.binding_id WHERE b.conversation=$1 AND a.state='settled' AND a.reported_tokens=10`, failedConversation).Scan(&count); err != nil || count != 1 {
		t.Fatalf("failed summary usage %d, %v", count, err)
	}
	trigger := request("/code/"+journey.route.Slug+"/responses", conversation, `{"model":"gemini-fixture","stream":true,"input":[`+string(item)+`,{"role":"user","content":"Keep the migration requirements"},{"type":"compaction_trigger"}]}`)
	stream, _ := io.ReadAll(trigger.Body)
	trigger.Body.Close()
	if trigger.StatusCode != http.StatusOK || !strings.Contains(trigger.Header.Get("Content-Type"), "text/event-stream") || !bytes.Contains(stream, []byte("response.output_item.done")) || !bytes.Contains(stream, []byte("response.completed")) || bytes.Contains(stream, []byte("PostgreSQL")) || calls.Load() != before+3 {
		t.Fatalf("trigger %d %s, calls %d", trigger.StatusCode, stream, calls.Load()-before)
	}
	s.want(owner, "POST", managePath+"/publish", nil, idempotent(etag(map[string]any{"etag": setup.enabledETag})), http.StatusOK)
	return summaryJourney{conversation: conversation, item: item, binding: binding, key: secretID}
}

func continueSummaryJourney(t *testing.T, s *service, journey cooldownJourney, consumer *caller, summary summaryJourney, calls *atomic.Int64) {
	t.Helper()
	response := summarySurfaceRequest(t, s, consumer, summary.conversation, "/code/"+journey.route.Slug+"/responses", `{"model":"gemini-fixture","input":[`+string(summary.item)+`,{"role":"user","content":"Continue the migration"}]}`)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("PostgreSQL migration constraints are restored")) {
		t.Fatalf("summary after restart and rotation %d %s", response.StatusCode, body)
	}
	path := "/code/" + journey.route.Slug + "/responses"
	continuation := `{"model":"gemini-fixture","input":[` + string(summary.item) + `,{"role":"user","content":"Continue the migration"}]}`
	status, raw := s.do(consumer, "POST", path, json.RawMessage(continuation), map[string]string{"Session-Id": summary.conversation + "-child", "X-Codex-Parent-Thread-Id": summary.conversation})
	if status != http.StatusOK || !bytes.Contains(raw, []byte("PostgreSQL migration constraints are restored")) {
		t.Fatalf("authorized child summary after restart and rotation %d %s", status, raw)
	}
	foreignRoot := uuid.NewString()
	foreign := summarySurfaceRequest(t, s, consumer, foreignRoot, path+"/compact", `{"model":"gemini-fixture","input":"Keep the migration requirements"}`)
	foreign.Body.Close()
	if foreign.StatusCode != http.StatusOK {
		t.Fatalf("foreign root summary setup %d", foreign.StatusCode)
	}
	before := calls.Load()
	for _, headers := range []map[string]string{
		{"Session-Id": foreignRoot},
		{"Session-Id": foreignRoot + "-child", "X-Codex-Parent-Thread-Id": foreignRoot},
	} {
		status, raw := s.do(consumer, "POST", path, json.RawMessage(continuation), headers)
		if status != http.StatusUnprocessableEntity || calls.Load() != before {
			t.Fatalf("foreign root or child summary dispatched %d %s", status, raw)
		}
	}
}

type summaryMasterKey struct {
	Version int    `json:"version"`
	Key     string `json:"key"`
}

type summaryMasterDocument struct {
	Active int                `json:"active_version"`
	Keys   []summaryMasterKey `json:"keys"`
}

func rotateSummarySecrets(t *testing.T, s *service) {
	t.Helper()
	path := s.env.values["OLP_MASTER_KEY_FILE"]
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document summaryMasterDocument
	if err := json.Unmarshal(original, &document); err != nil {
		t.Fatal(err)
	}
	first, second := uuid.New(), uuid.New()
	newKey := summaryMasterKey{2, hex.EncodeToString(append(first[:], second[:]...))}
	rotated, err := json.Marshal(summaryMasterDocument{2, append(document.Keys, newKey)})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.ParseRing(rotated)
	if err != nil {
		t.Fatal(err)
	}
	var installation string
	if err := s.pool.QueryRow(t.Context(), `SELECT id::text FROM olp.installation WHERE singleton`).Scan(&installation); err != nil {
		t.Fatal(err)
	}
	if _, err := ring.Rotate(t.Context(), s.pool, installation); err != nil {
		t.Fatal(err)
	}
	active, err := json.Marshal(summaryMasterDocument{2, []summaryMasterKey{newKey}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, active, 0600); err != nil {
		t.Fatal(err)
	}
}

func summarySurfaceRequest(t *testing.T, s *service, consumer *caller, conversation, path, body string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, s.origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+consumer.token)
	request.Header.Set("Session-Id", conversation)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
