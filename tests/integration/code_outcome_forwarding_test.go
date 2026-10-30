//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/codemode"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func codeSettledAttempts(t *testing.T, f *codeFleet, count int) []codemode.Attempt {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		attempts := codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
		settled := len(attempts) == count
		for _, attempt := range attempts {
			settled = settled && attempt.FinishedAt != nil
		}
		if settled {
			return attempts
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("attempts did not settle")
	return nil
}

func TestCodeQualificationForwardedOutcomesRetainStatusAndUncertainty(t *testing.T) {
	for _, test := range []struct {
		mode, origin, outcome string
		status, upstream      int
	}{
		{"unauthorized", "upstream", "rejected", 401, 401},
		{"quota", "upstream", "rejected", 429, 429},
		{"error", "upstream", "rejected", 500, 500},
		{"unavailable", "upstream", "rejected", 503, 503},
		{"disconnect", "upstream", "incomplete", 200, 200},
		{"transport_error", "gateway", "transport_error", 502, 0},
	} {
		t.Run(test.mode, func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.Mode(test.mode)
			response, _, err := f.request(t.Context(), 0, "outcome-root", "", codeBody())
			if err != nil || response.StatusCode != test.status {
				t.Fatalf("response: %v %v", response, err)
			}
			attempt := codeSettledAttempts(t, f, 1)[0]
			if attempt.State != "uncertain" || attempt.ReportedTokens != nil || attempt.Outcome == nil || *attempt.Outcome != test.outcome || attempt.OutcomeOrigin == nil || *attempt.OutcomeOrigin != test.origin || attempt.OutcomeObservedAt == nil {
				t.Fatalf("uncertainty or outcome changed: %+v", attempt)
			}
			if test.upstream == 0 && attempt.UpstreamStatus != nil || test.upstream != 0 && (attempt.UpstreamStatus == nil || *attempt.UpstreamStatus != test.upstream) {
				t.Fatalf("upstream status changed: %+v", attempt)
			}
			if attempt.Nonexecution || attempt.RecoveryFrom != nil || attempt.RecoveryReason != nil {
				t.Fatal("unknown consumption was presented as proven safe recovery")
			}
			if len(f.peer.Requests()) != 1 {
				t.Fatal("attempt was replayed")
			}
		})
	}
}

func TestCodeQualificationPartialOutputDiagnosticsRemainUncertain(t *testing.T) {
	f := newCodeFleet(t).initialRecovery(t, 1)
	f.peer.SetResponder(func(context.Context, http.Header, []byte) [][]byte {
		return [][]byte{
			[]byte(`{"type":"response.created","response":{"id":"partial-private-response"}}`),
			[]byte(`{"type":"response.output_text.delta","delta":"CONTROLLED_PARTIAL_PRIVATE_OUTPUT"}`),
		}
	})
	response, body, err := f.request(t.Context(), 0, "partial-output-diagnostics", "", codeBody(), true)
	if err != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("CONTROLLED_PARTIAL_PRIVATE_OUTPUT")) {
		t.Fatal("partial output did not reach the caller")
	}
	attempt := codeSettledAttempts(t, f, 1)[0]
	if attempt.State != "uncertain" || attempt.ReportedTokens != nil || attempt.Nonexecution || attempt.RecoveryFrom != nil || attempt.RecoveryReason != nil || attempt.Outcome == nil || *attempt.Outcome != "incomplete" {
		t.Fatalf("partial output lost consumption uncertainty: %+v", attempt)
	}
	if len(f.peer.Requests()) != 1 {
		t.Fatal("partial output was replayed")
	}
	status, listing, _ := f.h.request(f.owner, "GET", "/api/v1/code/attempts?project_id="+f.project+"&binding_id="+attempt.BindingID, nil, nil)
	diagnostics, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	if status != http.StatusOK || bytes.Contains(diagnostics, []byte("CONTROLLED_PARTIAL_PRIVATE_OUTPUT")) || bytes.Contains(diagnostics, []byte("CONTROLLED_PRIVATE_PROMPT")) || bytes.Contains(diagnostics, []byte("partial-private-response")) {
		t.Fatal("partial-output diagnostics exposed content or upstream state")
	}
}

func TestCodeQualificationWebSocketHealthRequiresRealGeneration(t *testing.T) {
	f := newCodeFleet(t)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	connection := codeReviewSocket(t, ctx, f, 0, f.key, "health-root", "gpt-5.4")
	for i, generate := range []bool{false, true} {
		body := fmt.Appendf(nil, `{"type":"response.create","model":"gpt-5.4","input":[],"generate":%t}`, generate)
		if err := connection.Write(ctx, websocket.MessageText, body); err != nil {
			t.Fatal(err)
		}
		if _, err := codexfixture.ReadGeneration(ctx, connection); err != nil {
			t.Fatal(err)
		}
		attempt := codeSettledAttempts(t, f, i+1)[0]
		if attempt.Outcome == nil || *attempt.Outcome != "completed" || attempt.OutcomeOrigin == nil || *attempt.OutcomeOrigin != "upstream" || attempt.UpstreamStatus != nil {
			t.Fatalf("WebSocket outcome: %+v", attempt)
		}
		accounts := codePublicDecode[[]codemode.Account](t, f.list("accounts"))
		found := false
		for _, account := range accounts {
			if account.ID != attempt.AccountID {
				continue
			}
			found = true
			want := "unknown"
			if generate {
				want = "healthy"
			}
			if account.Health != want {
				t.Fatalf("generate=%t health=%s", generate, account.Health)
			}
		}
		if !found {
			t.Fatal("account was not visible")
		}
	}
}
