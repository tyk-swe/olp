//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Hold the provider reply until a row lock is in place, then cancel the
// client's request while the accepted resource write is visibly waiting on
// PostgreSQL. This exercises the real public/client and storage boundary.
func TestGeminiAcceptedResourceCommitsOutliveClientDisconnect(t *testing.T) {
	h := newAccessHarness(t)
	provider := newGeminiLifecycleProvider(t)
	slug, key, _ := provisionGeminiLifecycle(t, h, h.owner(), "gemini-interactions", provider)
	var ownerID string
	if err := h.Pool.QueryRow(t.Context(), `SELECT id::text FROM olp_go.api_keys WHERE name='gemini-interactions key'`).Scan(&ownerID); err != nil {
		t.Fatal(err)
	}
	path := "/gemini/v1beta/interactions"
	type requestResult struct{ err error }
	call := func(method, target string, body []byte) (context.CancelFunc, <-chan requestResult) {
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan requestResult, 1)
		go func() {
			req, err := http.NewRequestWithContext(ctx, method, h.HTTP.URL+target, bytes.NewReader(body))
			if err == nil {
				req.Header.Set("X-Goog-Api-Key", key)
				if body != nil {
					req.Header.Set("Content-Type", "application/json")
				}
				var response *http.Response
				response, err = http.DefaultClient.Do(req)
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
				}
			}
			finished <- requestResult{err}
		}()
		return cancel, finished
	}
	waitForLock := func(fragment string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var waiting bool
			err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE pid<>pg_backend_pid() AND datname=current_database() AND wait_event_type='Lock' AND query LIKE $1)`, "%"+fragment+"%").Scan(&waiting)
			if err != nil {
				t.Fatal(err)
			}
			if waiting {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("accepted resource commit did not wait on %s", fragment)
	}
	gateCall := func(method, providerSuffix, publicPath string, body []byte, lock func(pgx.Tx)) {
		t.Helper()
		gate := &geminiReplyGate{method: method, suffix: providerSuffix, reached: make(chan struct{}), release: make(chan struct{})}
		provider.replyGate.Store(gate)
		cancel, finished := call(method, publicPath, body)
		select {
		case <-gate.reached:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("provider did not accept the public resource call")
		}
		tx, err := h.Pool.Begin(t.Context())
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		lock(tx)
		close(gate.release)
		provider.replyGate.Store(nil)
		if method == http.MethodPost && providerSuffix == "/v1beta/interactions" {
			waitForLock("olp_go.api_keys")
		} else {
			waitForLock("UPDATE olp_go.provider_resources SET state")
		}
		cancel()
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatal(err)
		}
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("disconnected public call did not terminate")
		}
	}
	lockOwner := func(tx pgx.Tx) {
		var id string
		if err := tx.QueryRow(t.Context(), `SELECT id::text FROM olp_go.api_keys WHERE id=$1 FOR UPDATE`, ownerID).Scan(&id); err != nil {
			t.Fatal(err)
		}
	}
	lockResource := func(tx pgx.Tx) {
		var id string
		if err := tx.QueryRow(t.Context(), `SELECT id::text FROM olp_go.provider_resources WHERE api_key_id=$1 AND kind='interaction' AND state<>'deleted' FOR UPDATE`, ownerID).Scan(&id); err != nil {
			t.Fatal(err)
		}
	}
	gateCall(http.MethodPost, "/v1beta/interactions", path,
		[]byte(fmt.Sprintf(`{"model":%q,"input":"accepted before disconnect","background":true,"store":true}`, slug)), lockOwner)
	var resourceID, state string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		err := h.Pool.QueryRow(t.Context(), `SELECT id::text,state FROM olp_go.provider_resources WHERE api_key_id=$1 AND kind='interaction'`, ownerID).Scan(&resourceID, &state)
		if err == nil && state == "in_progress" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if state != "in_progress" {
		t.Fatal("accepted background work lost its encrypted owner mapping after reader disconnect")
	}
	local := "interaction_" + strings.ReplaceAll(resourceID, "-", "")
	_, upstream, err := h.Gateway.Resources.ReadInteractionContract(t.Context(), ownerID, local)
	if err != nil {
		t.Fatal(err)
	}
	gateCall(http.MethodGet, "/v1beta/interactions/"+upstream, path+"/"+local, nil, lockResource)
	// The client has disconnected, but the detached write is still allowed to
	// finish. A plain MVCC SELECT can read the old committed row while that
	// UPDATE waits; a row-locking read joins the already-queued writer.
	readCtx, stopRead := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopRead()
	if err := h.Pool.QueryRow(readCtx, `SELECT state FROM olp_go.provider_resources WHERE id=$1 FOR UPDATE`, resourceID).Scan(&state); err != nil || state != "completed" {
		t.Fatalf("accepted status refresh was lost after disconnect: state=%q err=%v", state, err)
	}
	gateCall(http.MethodDelete, "/v1beta/interactions/"+upstream, path+"/"+local, nil, lockResource)
	var ciphertext bool
	deleteCtx, stopDelete := context.WithTimeout(t.Context(), 3*time.Second)
	defer stopDelete()
	if err := h.Pool.QueryRow(deleteCtx, `SELECT state FROM olp_go.provider_resources WHERE id=$1 FOR UPDATE`, resourceID).Scan(&state); err != nil || state != "deleted" {
		t.Fatalf("accepted delete was not tombstoned after disconnect: state=%q err=%v", state, err)
	}
	if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM olp_go.secrets WHERE id=$1)`, resourceID).Scan(&ciphertext); err != nil || ciphertext {
		t.Fatalf("accepted delete retained encrypted native ID: ciphertext=%t err=%v", ciphertext, err)
	}
	if response, _ := geminiPublic(t, h, http.MethodGet, path+"/"+local, key, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted accepted Interaction remained public: %d", response.StatusCode)
	}
}
