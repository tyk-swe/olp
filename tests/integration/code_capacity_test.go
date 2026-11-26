//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codesessions"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestCodeAccountGenerationCapacityAcrossReplicas(t *testing.T) {
	t.Parallel()
	pool, address := harness.Database(t)
	home := seedCooldownJourney(t, pool)
	remote := secondCodeReplica(t, pool, address, home)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET max_concurrency=1 WHERE project_id=$1`, home.route.ProjectID); err != nil {
		t.Fatal(err)
	}
	assertBusy := func(err error) {
		t.Helper()
		refusal, ok := errors.AsType[*codemode.Refusal](err)
		if !ok || refusal.Code != "code_account_busy" || refusal.Status != 429 || refusal.RetryAfter(time.Now()) != time.Second {
			t.Fatalf("admission = %v, want retryable local saturation", err)
		}
	}
	type result struct {
		journey cooldownJourney
		request codesessions.Admission
		permit  codesessions.Permit
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 3)
	for index, journey := range []cooldownJourney{home, remote, home} {
		go func() {
			<-start
			request := journey.admission("model", fmt.Sprintf("concurrent-%d", index))
			permit, err := journey.store.Admit(t.Context(), request)
			results <- result{journey: journey, request: request, permit: permit, err: err}
		}()
	}
	close(start)
	var admitted []result
	for range 3 {
		response := <-results
		if response.err != nil {
			assertBusy(response.err)
			continue
		}
		admitted = append(admitted, response)
	}
	if len(admitted) != 2 || admitted[0].permit.Account.ID == admitted[1].permit.Account.ID {
		t.Fatalf("admissions = %v, want one generation on each account of the shared connection", admitted)
	}
	pinned, sibling := admitted[0], admitted[1]
	socket := codemode.Identity{Conversation: "waiting-socket"}
	_, err := home.store.BindConnection(t.Context(), home.route, home.key, socket, "model", pinned.request.Providers)
	assertBusy(err)
	if err := sibling.journey.store.MarkDispatched(t.Context(), sibling.permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := sibling.journey.store.Settle(t.Context(), sibling.permit.Attempt.ID, codemode.Usage{Total: new(int64(0))}); err != nil {
		t.Fatal(err)
	}
	connection, err := home.store.BindConnection(t.Context(), home.route, home.key, socket, "model", pinned.request.Providers)
	if err != nil || connection.Account.ID != sibling.permit.Account.ID {
		t.Fatalf("new WebSocket bind = %s, %v, want the available account %s", connection.Account.ID, err, sibling.permit.Account.ID)
	}
	_, err = pinned.journey.store.Admit(t.Context(), pinned.request)
	assertBusy(err)
	if err := pinned.journey.store.MarkDispatched(t.Context(), pinned.permit.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if err := pinned.journey.store.Settle(t.Context(), pinned.permit.Attempt.ID, codemode.Usage{}); err != nil {
		t.Fatal(err)
	}
	retried, err := pinned.journey.store.Admit(t.Context(), pinned.request)
	if err != nil || retried.Pin.AccountID != pinned.permit.Pin.AccountID || retried.Account.Health != pinned.permit.Account.Health {
		t.Fatalf("retry = pin %s, health %s, error %v, want original pin and unchanged health", retried.Pin.AccountID, retried.Account.Health, err)
	}
}

func TestCodeAccountDrainAcrossReplicas(t *testing.T) {
	t.Parallel()
	pool, address := harness.Database(t)
	home := seedCooldownJourney(t, pool)
	remote := secondCodeReplica(t, pool, address, home)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	admit := func(store *codesessions.Store, request codesessions.Admission, want string) codesessions.Permit {
		t.Helper()
		permit, err := store.Admit(t.Context(), request)
		if err != nil || permit.Account.ID != want {
			t.Fatalf("admission for %s = %s, %v, want %s", request.Operation.Identity.Conversation, permit.Account.ID, err, want)
		}
		return permit
	}
	refused := func(err error, code string, status int) {
		t.Helper()
		if refusal, ok := errors.AsType[*codemode.Refusal](err); !ok || refusal.Code != code || refusal.Status != status {
			t.Fatalf("refusal = %v, want %d %s", err, status, code)
		}
	}

	exec(`UPDATE olp.code_accounts SET max_concurrency=1,enabled=(id=$1) WHERE project_id=$2`, home.first, home.route.ProjectID)
	pinned := home.admission("model", "pinned")
	initial := admit(home.store, pinned, home.first)
	exec(`UPDATE olp.code_accounts SET enabled=true,draining=(id=$1) WHERE project_id=$2`, home.first, home.route.ProjectID)
	_, err := remote.store.Admit(t.Context(), pinned)
	refused(err, "code_account_busy", 429)
	if err := home.store.Abort(t.Context(), initial.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	admit(remote.store, remote.admission("model", "new-on-sibling"), home.second)
	_, err = home.store.Admit(t.Context(), home.admission("model", "no-slot-on-sibling"))
	refused(err, "code_account_busy", 429)
	connection, err := remote.store.BindConnection(t.Context(), home.route, home.key, pinned.Operation.Identity, "model", pinned.Providers)
	if err != nil || connection.Pin != initial.Pin {
		t.Fatalf("reconnect = %+v, %v, want original pin %+v", connection.Pin, err, initial.Pin)
	}
	continued := admit(remote.store, pinned, home.first)
	if continued.Binding.ID != initial.Binding.ID || continued.Pin != initial.Pin {
		t.Fatal("drain replaced the conversation binding or model pin")
	}
	if err := remote.store.Abort(t.Context(), continued.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	child := home.admission("model", "child")
	child.Operation.Identity.Parent = "pinned"
	admit(remote.store, child, home.first)
	_, err = home.store.Admit(t.Context(), home.admission("other", "pinned"))
	refused(err, "code_account_unavailable", 503)

	revision := home.route
	revision.RevisionID, revision.Revision = uuid.NewString(), 2
	document, err := json.Marshal(revision)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO olp.code_route_revisions(id,route_id,revision,document,connections,created_by)
		SELECT $1,route_id,2,$2,connections,created_by FROM olp.code_route_revisions WHERE id=$3`, revision.RevisionID, document, home.route.RevisionID)
	exec(`UPDATE olp.code_routes SET latest_revision_id=$2 WHERE id=$1`, revision.ID, revision.RevisionID)
	exec(`UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, home.second)
	fresh := home.admission("model", "new-after-publish")
	fresh.Route = revision
	_, err = remote.store.Admit(t.Context(), fresh)
	refused(err, "code_account_unavailable", 503)
	exec(`UPDATE olp.code_accounts SET draining=false,max_concurrency=NULL WHERE id=$1`, home.first)
	admit(remote.store, fresh, home.first)
	exec(`UPDATE olp.code_accounts SET draining=true WHERE id=$1`, home.first)
	pinned.Route = revision
	exec(`UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, home.first)
	_, err = remote.store.Admit(t.Context(), pinned)
	refused(err, "code_account_unavailable", 503)
	exec(`UPDATE olp.code_accounts SET enabled=true WHERE id=$1`, home.first)
	exec(`UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, initial.Account.CredentialID)
	_, err = remote.store.Admit(t.Context(), pinned)
	refused(err, "code_account_unavailable", 503)
	exec(`UPDATE olp.code_accounts SET draining=false WHERE id=$1`, home.first)
	_, err = home.store.Admit(t.Context(), home.admission("model", "after-revocation"))
	refused(err, "code_account_unavailable", 503)
}

func TestCodeAccountGenerationCapacityIgnoresIdleAndAbandonedWork(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	j := seedCooldownJourney(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_accounts SET max_concurrency=1 WHERE project_id=$1`, j.route.ProjectID); err != nil {
		t.Fatal(err)
	}
	request := j.admission("model", "idle-socket")
	socket, err := j.store.BindConnection(t.Context(), j.route, j.key, request.Operation.Identity, "model", request.Providers)
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := j.store.Admit(t.Context(), request)
	if err != nil || abandoned.Account.ID != socket.Account.ID {
		t.Fatalf("first generation on idle socket = %s, %v, want admission on %s", abandoned.Account.ID, err, socket.Account.ID)
	}
	if _, err := pool.Exec(t.Context(), `UPDATE olp.code_attempts SET created_at=now()-interval '1 hour' WHERE id=$1`, abandoned.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	next, err := j.store.Admit(t.Context(), request)
	if err != nil || next.Account.ID != socket.Account.ID {
		t.Fatalf("generation after abandoned lease = %s, %v, want admission on %s", next.Account.ID, err, socket.Account.ID)
	}
	if err := j.store.MarkDispatched(t.Context(), abandoned.Attempt.ID); err == nil {
		t.Fatal("an expired reservation could still dispatch")
	}
	if err := j.store.Abort(t.Context(), next.Attempt.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := j.store.Admit(t.Context(), request); err != nil {
		t.Fatalf("generation after cancellation before dispatch: %v", err)
	}
}

func TestCodeRecoverySkipsDrainingAccounts(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	j := seedCooldownJourney(t, pool)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(t.Context(), sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	j.route.InitialRecoveryAttempts = 1
	document, err := json.Marshal(j.route)
	if err != nil {
		t.Fatal(err)
	}
	exec(`UPDATE olp.code_route_revisions SET document=$2 WHERE id=$1`, j.route.RevisionID, document)
	exec(`UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, j.second)
	request := j.admission("model", "recover-after-drain")
	request.InitialRecovery = true
	first, err := j.store.Admit(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	settleRejectedCodeAttempt(t, j.store, first.Attempt.ID, 401)
	exec(`UPDATE olp.code_accounts SET draining=true,health='unavailable',unavailable_until=now() WHERE id=$1`, j.first)
	exec(`UPDATE olp.provider_grants SET generation=2 WHERE credential_id=$1`, first.Account.CredentialID)
	exec(`UPDATE olp.code_accounts SET enabled=true,draining=true WHERE id=$1`, j.second)
	alternate := &codesessions.Recovery{From: first.Attempt.ID, Envelope: "codex_invalid_token", Number: 1}
	request.Recovery = alternate
	_, err = j.store.Admit(t.Context(), request)
	if refusal, ok := errors.AsType[*codemode.Refusal](err); !ok || refusal.Code != "code_account_unavailable" {
		t.Fatalf("recovery with only draining accounts = %v, want code_account_unavailable", err)
	}
	exec(`UPDATE olp.code_accounts SET draining=false WHERE id=$1`, j.second)
	request.Recovery = &codesessions.Recovery{
		From: first.Attempt.ID, Envelope: "codex_invalid_token", Number: 1, AccountID: j.first,
		CredentialID: first.Account.CredentialID, GrantGeneration: 1, RejectedUntil: time.Now().Add(time.Minute),
	}
	_, err = j.store.Admit(t.Context(), request)
	if refusal, ok := errors.AsType[*codemode.Refusal](err); !ok || refusal.Code != "code_account_unavailable" {
		t.Fatalf("same-account recovery on a draining account = %v, want code_account_unavailable", err)
	}
	request.Recovery = alternate
	recovered, err := j.store.Admit(t.Context(), request)
	if err != nil || recovered.Account.ID != j.second {
		t.Fatalf("recovery with a draining source account = %s, %v, want sibling %s", recovered.Account.ID, err, j.second)
	}
}
