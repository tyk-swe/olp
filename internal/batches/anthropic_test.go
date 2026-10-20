package batches

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

func anthropicFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("../../tests/fixtures/vendors/anthropic/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestAnthropicReaderPollAndPagedResults(t *testing.T) {
	inProgress := anthropicFixture(t, "batch.in_progress.json")
	ended := anthropicFixture(t, "batch.ended.json")
	results := anthropicFixture(t, "batch.results.jsonl")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("X-Api-Key") != "anthropic-secret" || r.Header.Get("Anthropic-Version") != "2023-06-01" {
			t.Errorf("pinned Anthropic credentials/version missing: %v", r.Header)
		}
		switch r.URL.Path {
		case "/v1/messages/batches/msgbatch_01ABCDEF":
			body := inProgress
			if requests > 1 {
				body = ended
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		case "/v1/messages/batches/msgbatch_01ABCDEF/results":
			w.Header().Set("Content-Type", "application/jsonl")
			_, _ = w.Write(results)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	policy := &egress.Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		PlainHTTPHosts:  []string{"127.0.0.1"},
	}
	reader := &AnthropicReader{Auth: connectors.NewAuth(policy), Egress: policy}
	target := &PinnedTarget{
		Config: connectors.Config{Kind: "anthropic", AuthMode: "api_key", Endpoint: server.URL + "/v1"},
		Model:  "claude-test", Secret: []byte("anthropic-secret"), Client: server.Client(),
	}
	settlement := Settlement{UpstreamID: stringPointer("msgbatch_01ABCDEF")}
	first, err := reader.Poll(context.Background(), settlement, target)
	if err != nil || first.Terminal || first.Expected == nil || *first.Expected != 1 {
		t.Fatalf("in-progress poll = %+v, %v", first, err)
	}
	snapshot, err := reader.Poll(context.Background(), settlement, target)
	if err != nil || !snapshot.Terminal || snapshot.Expected == nil || *snapshot.Expected != 4 {
		t.Fatalf("ended poll = %+v, %v", snapshot, err)
	}
	var committed []Result
	var cursor Cursor
	next, done, err := reader.ReadResults(context.Background(), target, snapshot, Cursor{}, func(page []Result, next Cursor) error {
		committed = append(committed, page...)
		cursor = next
		return nil
	})
	if err != nil || !done || next.Phase != "done" || cursor.Line != 4 {
		t.Fatalf("result read cursor=%+v done=%v err=%v", next, done, err)
	}
	if len(committed) != 4 {
		t.Fatalf("committed %d results, want 4", len(committed))
	}
	if got := []ResultKind{committed[0].Kind, committed[1].Kind, committed[2].Kind, committed[3].Kind}; !slices.Equal(got, []ResultKind{ResultSucceeded, ResultErrored, ResultCancelled, ResultExpired}) {
		t.Fatalf("result kinds = %v", got)
	}
	usage := committed[0].Usage
	if !usage.Observed || !usage.Complete || usage.InputTokens == nil || *usage.InputTokens != 17 ||
		usage.OutputTokens == nil || *usage.OutputTokens != 7 ||
		usage.CachedInputTokens == nil || *usage.CachedInputTokens != 2 ||
		usage.CacheWriteInputTokens == nil || *usage.CacheWriteInputTokens != 5 ||
		usage.CacheWrite5MInputTokens == nil || *usage.CacheWrite5MInputTokens != 3 ||
		usage.CacheWrite1HInputTokens == nil || *usage.CacheWrite1HInputTokens != 2 {
		t.Fatalf("Anthropic cache usage = %+v", usage)
	}
	reconciled, err := reader.Reconcile(context.Background(), settlement, target, "file", "/v1/messages")
	if err != nil || len(reconciled.Matches) != 0 {
		t.Fatalf("Anthropic reconciliation = %+v, %v", reconciled, err)
	}
}

func TestAnthropicReaderRejectsUnknownResultTypesAndRegisters(t *testing.T) {
	line := []byte(`{"custom_id":"x","result":{"type":"unknown"}}`)
	if _, err := parseAnthropicResultLine(line, "claude-test"); err != ErrCursorConflict {
		t.Fatalf("unknown result type error = %v", err)
	}
	worker := &Worker{}
	worker.setup()
	if worker.Readers["anthropic"] == nil {
		t.Fatal("Anthropic batch reader was not registered")
	}
}

func stringPointer(value string) *string { return &value }
