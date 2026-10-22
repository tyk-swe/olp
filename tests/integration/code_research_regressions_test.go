//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/limits"
	"github.com/tyk-swe/olp/internal/resources"
)

func TestCodeResearchIgnoresInferenceTokenRates(t *testing.T) {
	f := researchFixture(t)
	_, _, limiter := limSetup(t, "research-token-rates")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, nil, slog.New(slog.DiscardHandler))
	digest := access.DigestEndUser(f.h.Server.Auth, &f.project, "research-user")
	endUsers := map[string]any{"defaults": map[string]any{"tokens_per_minute": 1000}, "overrides": map[string]any{digest: map[string]any{"tokens_per_minute": 2000}}}
	policy, _ := json.Marshal(map[string]any{"tokens_per_minute": 1000, "requests_per_minute": 100, "route_limits": map[string]any{"qualification": map[string]any{"tokens_per_minute": 1000}}, "end_user_source": "header", "end_user_policy": endUsers})
	group := f.h.want(f.owner, "POST", "/api/v1/budget-groups", map[string]any{"name": "Research token group", "project_id": f.project, "tokens_per_minute": 1000}, idem("research-token-group"), 201)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET budget_group_id=$2,policy=policy||$3::jsonb WHERE id=$1`, f.keyID, group["id"], policy); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.projects SET end_user_policy=$2 WHERE id=$1`, f.project, endUsers); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/providers/" + f.providers[0]
	provider := f.h.want(f.owner, "GET", path, nil, nil, 200)
	configuration := provider["configuration"].(map[string]any)
	configuration["options"].(map[string]any)["limits"] = map[string]any{"tokens_per_minute": 1000}
	f.h.want(f.owner, "PATCH", path, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
	f.publishResearch(t, []string{"search"})
	response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200, "research-user")
	session := response.Header.Get("Mcp-Session-Id")
	for _, user := range []string{"research-user", "default-user"} {
		// Cover end-user overrides as well as defaults at both boundaries.
		f.research(t, "POST", "search", f.key, session, researchList, 200, user)
	}
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=jsonb_set(policy,'{requests_per_minute}','1') WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	f.research(t, "POST", "search", f.key, session, researchList, 429, "research-user")
}

func TestCodeResearchServerPingsRespectLiveEndUserBlocks(t *testing.T) {
	for _, boundary := range []string{"key", "project"} {
		t.Run(boundary, func(t *testing.T) {
			f := researchFixture(t)
			digest := access.DigestEndUser(f.h.Server.Auth, &f.project, "research-user")
			policy := map[string]any{"defaults": map[string]any{"tokens_per_minute": 1000}}
			if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy||'{"end_user_source":"header"}' WHERE id=$1`, f.keyID); err != nil {
				t.Fatal(err)
			}
			setPolicy := func() {
				t.Helper()
				var err error
				if boundary == "key" {
					_, err = f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=jsonb_set(policy,'{end_user_policy}',$2) WHERE id=$1`, f.keyID, policy)
				} else {
					_, err = f.h.Pool.Exec(t.Context(), `UPDATE olp.projects SET end_user_policy=$2 WHERE id=$1`, f.project, policy)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			setPolicy()
			response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200, "research-user")
			session := response.Header.Get("Mcp-Session-Id")
			trigger := make(chan struct{})
			var answered atomic.Int32
			f.peer.mu.Lock()
			f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					answered.Add(1)
					w.WriteHeader(http.StatusAccepted)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, ": heartbeat\n\n")
				w.(http.Flusher).Flush()
				select {
				case <-trigger:
				case <-r.Context().Done():
					return
				}
				fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":\"blocked-user-ping\",\"method\":\"ping\"}\n\n")
				w.(http.Flusher).Flush()
			}
			f.peer.mu.Unlock()
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, "GET", f.gateway.URL+"/code/qualification/mcp/search", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Authorization", "Bearer "+f.key)
			request.Header.Set("Mcp-Session-Id", session)
			request.Header.Set("X-OLP-End-User", "research-user")
			stream, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Body.Close()
			policy["blocked"] = []string{digest}
			setPolicy()
			close(trigger)
			body, err := io.ReadAll(stream.Body)
			if err != nil || answered.Load() != 0 || strings.Contains(string(body), "blocked-user-ping") {
				t.Fatalf("ping bypassed %s end-user block: replies=%d body=%s err=%v", boundary, answered.Load(), body, err)
			}
		})
	}
}

func TestCodeResearchStaleInitializationPreservesCurrentSessions(t *testing.T) {
	for _, change := range []string{"publication", "generation", "credential"} {
		t.Run(change, func(t *testing.T) {
			f := researchFixture(t)
			account := f.accounts[0]
			stale := codemode.ResearchSessionBinding{APIKeyID: f.keyID, RevisionID: f.route["revision_id"].(string), Service: "search", AccountID: account["id"].(string), CredentialID: account["credential_id"].(string)}
			if err := f.h.Pool.QueryRow(t.Context(), `SELECT generation FROM olp.provider_grants WHERE credential_id=$1`, stale.CredentialID).Scan(&stale.Generation); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "publication":
				f.publishResearch(t, []string{"search"})
			case "generation":
				if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.provider_grants SET generation=generation+1 WHERE credential_id=$1`, stale.CredentialID); err != nil {
					t.Fatal(err)
				}
				f.h.refresh()
			case "credential":
				replacement := f.enroll(t, account["provider_id"].(string), "research-vendor-key")
				path := "/api/v1/code/accounts/" + stale.AccountID
				f.h.want(f.owner, "PUT", path, map[string]any{"project_id": f.project, "provider_id": account["provider_id"], "credential_id": replacement, "name": account["name"], "enabled": true, "models": account["models"]}, etagHeader(account), 200)
				if replacement == stale.CredentialID {
					t.Fatal("fixture did not rotate the credential")
				}
				f.h.refresh()
			}
			response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200)
			current := response.Header.Get("Mcp-Session-Id")
			// Complete a handshake using the binding captured before the change.
			// It must not prune the session that finished with the current binding.
			_, err := (&resources.CodeStore{Pool: f.h.Pool}).CreateResearchSession(t.Context(), stale, "delayed-native-session")
			var refusal *codemode.Refusal
			if !errors.As(err, &refusal) || refusal.Status != 409 {
				t.Fatalf("stale initialization: %v", err)
			}
			researchSessionCount(t, f, 1)
			f.research(t, "POST", "search", f.key, current, researchList, 200)
		})
	}
}

func TestCodeResearchEligibilityIgnoresInferenceHealthAndAllowance(t *testing.T) {
	f := researchFixture(t)
	accountID := f.accounts[0]["id"].(string)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.code_accounts SET health='unavailable',unavailable_until=now()+interval '1 hour',allowance='{"remaining_tokens":0,"remaining_requests":null,"remaining_percent":null,"resets_at":null,"observed_at":"2026-10-21T10:00:00Z"}' WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		items := f.h.want(f.owner, "GET", "/api/v1/code/accounts?project_id="+f.project, nil, nil, 200)["items"].([]any)
		for _, item := range items {
			account := item.(map[string]any)
			if account["id"] == accountID {
				if account["eligible"] != false || account["research_eligible"] != want {
					t.Fatalf("research/inference eligibility: %v", account)
				}
				return
			}
		}
		t.Fatal("research account missing")
	}
	check(true)
	f.publishResearch(t, []string{"search"})
	f.research(t, "POST", "search", f.key, "", researchInit, 200)
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.code_accounts SET enabled=false WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestCodeResearchCancellationBypassesWorkConcurrency(t *testing.T) {
	f := researchFixture(t)
	_, _, limiter := limSetup(t, "research-cancellation")
	f.h.Gateway.Admission = gateway.NewAdmission(limiter, func() limits.OutagePolicy { return limits.FailClosed }, slog.New(slog.DiscardHandler))
	// Exercise key, route and provider concurrency together. The frozen provider
	// connection must enforce the same one-slot limit as the live key policies.
	if _, err := f.h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET policy=policy||'{"max_concurrency":1,"requests_per_minute":100,"tokens_per_minute":1000,"route_limits":{"qualification":{"max_concurrency":1,"tokens_per_minute":1000}}}' WHERE id=$1`, f.keyID); err != nil {
		t.Fatal(err)
	}
	path := "/api/v1/providers/" + f.providers[0]
	provider := f.h.want(f.owner, "GET", path, nil, nil, 200)
	configuration := provider["configuration"].(map[string]any)
	configuration["options"].(map[string]any)["limits"] = map[string]any{"max_concurrency": 1, "requests_per_minute": 100, "tokens_per_minute": 1000}
	f.h.want(f.owner, "PATCH", path, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
	f.publishResearch(t, []string{"search"})
	response, _ := f.research(t, "POST", "search", f.key, "", researchInit, 200)
	session := response.Header.Get("Mcp-Session-Id")
	started, cancelled := make(chan struct{}), make(chan struct{})
	f.peer.mu.Lock()
	f.peer.respond = func(w http.ResponseWriter, r *http.Request) {
		var message struct{ Method string }
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Error(err)
			return
		}
		if message.Method == "notifications/cancelled" {
			close(cancelled)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		close(started)
		select {
		case <-cancelled:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":3,"error":{"code":-32000,"message":"cancelled"}}`)
		case <-r.Context().Done():
		}
	}
	f.peer.mu.Unlock()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "POST", f.gateway.URL+"/code/qualification/mcp/search", strings.NewReader(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"webSearchPrime","arguments":{"query":"slow"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+f.key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Mcp-Session-Id", session)
	done := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			defer response.Body.Close()
			_, err = io.ReadAll(response.Body)
			if err == nil && response.StatusCode != 200 {
				err = fmt.Errorf("tool call status: %d", response.StatusCode)
			}
		}
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("tool call did not reach upstream")
	}
	// Work still needs its slot, while session-scoped control can reach it.
	f.research(t, "POST", "search", f.key, session, researchList, 429)
	f.research(t, "POST", "search", f.key, session, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`, 202)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
