//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oapi-codegen/nullable"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codemode/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode/codeclient"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/internal/codesessions/codeaccounts"
	"github.com/tyk-swe/olp/internal/codesessions/codeallowance"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	codepromotion "github.com/tyk-swe/olp/internal/promotion/code"
	providerpromotion "github.com/tyk-swe/olp/internal/promotion/provider"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestCodeCooldownJourneys(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
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
	settleRejectedCodeAttempt(t, journey.store, first.Attempt.ID, 429)
	now := time.Now().UTC()
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
	if decision.Reason != codemode.DecisionChosen || decision.ChosenAccountID != recovered.Account.ID || decision.RecoveryEnvelope == nil || *decision.RecoveryEnvelope != "codex_usage_limit_reached" || decision.RecoveryOrdinal == nil || *decision.RecoveryOrdinal != 1 || len(decision.Skipped) != 1 || decision.Skipped[0].Class != codemode.SkipWeight || decision.Skipped[0].AccountID == recovered.Account.ID {
		t.Fatalf("recovery decision = %+v", decision)
	}
}

func settleRejectedCodeAttempt(t *testing.T, store *codesessions.Store, id string, status int) {
	t.Helper()
	if err := store.MarkDispatched(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := store.ObserveOutcome(t.Context(), id, codemode.Outcome{Origin: "upstream", Kind: "rejected", UpstreamStatus: &status, ObservedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmNonexecution(t.Context(), id, status); err != nil {
		t.Fatal(err)
	}
	if err := store.Settle(t.Context(), id, codemode.Usage{Total: new(int64(0))}); err != nil {
		t.Fatal(err)
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
	t.Parallel()
	pool, _ := harness.Database(t)
	t.Run("Codex", func(t *testing.T) { codeActiveAllowanceDoesNotAffectBands(t, pool) })
	t.Run("MiniMax", func(t *testing.T) { codeMiniMaxActiveRemainsDoesNotAffectBands(t, pool) })
}

func codeActiveAllowanceDoesNotAffectBands(t *testing.T, pool *pgxpool.Pool) {
	user, project, provider := uuid.NewString(), uuid.NewString(), uuid.NewString()
	active, unknown := uuid.NewString(), uuid.NewString()
	key, poolID, routeID, revisionID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "active-allowance", PoolID: poolID, Models: []string{"model"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
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
	route := codemode.Route{ID: routeID, ProjectID: project, Slug: "minimax-active-allowance", PoolID: poolID, Models: []string{"MiniMax-M3"}, Enabled: true, RevisionID: revisionID, Revision: 1, ETag: uuid.NewString()}
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
}

func TestCodeAccountExtraUsageRoundTripsAndRefusesNonKimiAccounts(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
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
	operator := authz.Principal{User: authz.User{ID: user}, AllProjects: true}
	enabled, disabled := true, false
	save := func(id string, input codeaccounts.Input, existing bool) (map[string]any, error) {
		t.Helper()
		tx, err := pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(t.Context())
		body, err := codeaccounts.Save(t.Context(), tx, operator, id, ids.New(), input, existing)
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
	kimiInput := codeaccounts.Input{ProjectID: project, ProviderID: kimiProvider, CredentialID: kimiCredential, Settings: codeaccounts.Settings{Name: "kimi", Enabled: true, Models: []string{"model"}, ExtraUsageEnabled: &enabled}}
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
	codexInput := codeaccounts.Input{ProjectID: project, ProviderID: codexProvider, CredentialID: codexCredential, Settings: codeaccounts.Settings{Name: "codex", Enabled: true, Models: []string{"model"}, ExtraUsageEnabled: &enabled}}
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
	exported, err := codepromotion.Read(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(exported.Accounts, func(entry codepromotion.Account) bool { return entry.ID == kimiAccount })
	if index < 0 || exported.Accounts[index].ExtraUsageEnabled == nil || !*exported.Accounts[index].ExtraUsageEnabled {
		t.Fatalf("exported accounts = %+v, want the kimi account carrying extra usage on", exported.Accounts)
	}
}

func TestCodeAccountPromotionCycleAnchor(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
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
	date := func(value string) nullable.Nullable[openapi_types.Date] {
		parsed, err := time.Parse(time.DateOnly, value)
		if err != nil {
			t.Fatal(err)
		}
		return nullable.NewNullableWithValue(openapi_types.Date{Time: parsed})
	}
	for _, tc := range []struct {
		name     string
		declared nullable.Nullable[openapi_types.Date]
		anchor   *string
		action   contract.ConfigurationPlanItemAction
	}{
		{"unchanged", date(anchor), &anchor, contract.ConfigurationPlanItemActionNoop},
		{"cleared", nullable.NewNullNullable[openapi_types.Date](), nil, contract.ConfigurationPlanItemActionReplace},
		{"changed", date(changedAnchor), &changedAnchor, contract.ConfigurationPlanItemActionReplace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx, err := pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			document := &contract.ConfigurationCode{Accounts: nullable.NewNullableWithValue([]contract.ConfigurationCodeAccount{{Id: uuid.MustParse(account), Project: "cycle-anchor-journey", Provider: "cycle-anchor-provider", CredentialRef: "account-credential", Name: "cycle-anchor-account", Enabled: true, Models: []string{"model"}, CycleAnchor: tc.declared}})}
			bindings := &contract.ConfigurationCodeBindings{Credentials: &map[string]openapi_types.UUID{"account-credential": uuid.MustParse(credential)}}
			desired, err := codepromotion.Parse(document, bindings, promotion.Names{}, providerpromotion.Deps{})
			if err != nil {
				t.Fatal(err)
			}
			items, err := codepromotion.Planner(desired, providerpromotion.Deps{}).Plan(t.Context(), tx)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Kind != contract.ConfigurationPlanItemKindCodeAccount || items[0].Action != tc.action {
				t.Fatalf("promotion plan = %+v, want one account %s", items, tc.action)
			}
			operator := authz.Principal{User: authz.User{ID: user}, AllProjects: true}
			if err := codepromotion.Applier(desired, providerpromotion.Deps{})(t.Context(), tx, operator); err != nil {
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
			if tc.action == contract.ConfigurationPlanItemActionNoop && storedETag != etag {
				t.Fatal("unchanged account was replaced")
			}
		})
	}
}
