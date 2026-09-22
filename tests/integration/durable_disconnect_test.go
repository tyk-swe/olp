//go:build integration

package integration_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The provider can accept a queued batch before its local mapping commits.
// A cancelled client must not cancel that already-accepted mapping write.
func TestAcceptedBatchMappingSurvivesClientDisconnect(t *testing.T) {
	fixture := newOpenAIFixture(t, "input-file")
	h := newAccessHarness(t)
	_, _, slug, key := provisionOpenAI(t, h, fixture.URL,
		[]any{map[string]any{"operation": "batch", "surface": "openai", "mode": "unary"}}, []string{"batch"})
	status, uploaded := h.uploadTestFile(slug, key, "batch-line\n")
	if status != http.StatusOK {
		t.Fatalf("upload status=%d body=%v", status, uploaded)
	}
	fileID := uploaded["id"].(string)
	before := fixture.dials.Load()

	// Block the local INSERT after the scripted provider returns its accepted
	// batch ID. This makes the disconnect happen at the actual durability
	// boundary without adding a production test hook or sleeping on a race.
	const lockKey int64 = 21416014
	locker, err := h.Pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	t.Cleanup(func() {
		if locked {
			locker.Exec(context.Background(), "SELECT pg_advisory_unlock($1)", lockKey)
		}
		locker.Release()
	})
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Pool.Exec(t.Context(), `CREATE FUNCTION olp_go.wait_batch_mapping() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.kind='batch' THEN PERFORM pg_advisory_xact_lock(21416014); END IF;
  RETURN NEW;
END $$;
CREATE TRIGGER wait_batch_mapping BEFORE INSERT ON olp_go.provider_resources
FOR EACH ROW EXECUTE FUNCTION olp_go.wait_batch_mapping()`); err != nil {
		t.Fatal(err)
	}

	clientCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(clientCtx, http.MethodPost, h.HTTP.URL+"/v1/batches",
		strings.NewReader(`{"input_file_id":"`+fileID+`","endpoint":"/v1/chat/completions","completion_window":"24h"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	finished := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if resp != nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		finished <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := h.Pool.QueryRow(t.Context(), `SELECT EXISTS(
SELECT 1 FROM pg_stat_activity WHERE datname=current_database()
AND wait_event_type='Lock' AND wait_event='advisory')`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("accepted batch never reached its local commit; provider calls=%d", fixture.dials.Load()-before)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if _, err := locker.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lockKey); err != nil {
		t.Fatal(err)
	}
	locked = false
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled client did not release its HTTP request")
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		var mapped int
		if err := h.Pool.QueryRow(t.Context(), `SELECT count(*) FROM olp_go.provider_resources
WHERE kind='batch' AND upstream_id='batch-up-1'`).Scan(&mapped); err != nil {
			t.Fatal(err)
		}
		if mapped == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("provider accepted a batch but the cancelled client lost its owner mapping")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fixture.dials.Load() != before+1 {
		t.Fatalf("cancelled batch submission dispatched %d times", fixture.dials.Load()-before)
	}
}
