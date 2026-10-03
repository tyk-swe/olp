//go:build integration

package integration_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/resources"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestCodeWebSocketRechecksRevokedConnectionCredentials(t *testing.T) {
	for _, target := range []string{"account", "network"} {
		t.Run(target, func(t *testing.T) {
			f := newCodePublicFixtureWithPeer(t, codexfixture.New())
			h, owner := f.h, f.owner
			account := f.accounts[0]
			providerPath := "/api/v1/providers/" + account["provider_id"].(string)
			credentialID := account["credential_id"].(string)
			poolInput := f.poolInput([]string{f.keyID})
			poolInput["account_ids"] = []string{account["id"].(string)}
			h.want(owner, "PUT", "/api/v1/code/pools/"+f.pool["id"].(string), poolInput, etagHeader(f.pool), 200)
			if target == "network" {
				provider := h.want(owner, "GET", providerPath, nil, nil, 200)
				stored := h.want(owner, "POST", providerPath+"/network-credentials", map[string]any{"credential": `{"proxy_username":"fixture","proxy_password":"password"}`}, withMatch(provider, idem("socket-network")), 201)
				credentialID = stored["credential_id"].(string)
				provider = h.want(owner, "GET", providerPath, nil, nil, 200)
				configuration := provider["configuration"].(map[string]any)
				configuration["options"].(map[string]any)["network"].(map[string]any)["credential_id"] = credentialID
				h.want(owner, "PATCH", providerPath, map[string]any{"name": provider["name"], "configuration": configuration}, etagHeader(provider), 200)
				proxyHandler := f.codexAuthorities[0].proxy.Config.Handler
				f.codexAuthorities[0].proxy.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Proxy-Authorization") != "Basic Zml4dHVyZTpwYXNzd29yZA==" {
						w.WriteHeader(http.StatusProxyAuthRequired)
						return
					}
					proxyHandler.ServeHTTP(w, r)
				})
				routePath := "/api/v1/code/routes/" + f.route["id"].(string)
				draft := h.want(owner, "PUT", routePath, map[string]any{"project_id": f.project, "slug": "qualification", "pool_id": f.pool["id"], "models": []string{"gpt-5.4"}, "enabled": true}, etagHeader(f.route), 200)
				h.want(owner, "POST", routePath+"/publish", nil, withMatch(draft, idem("socket-network-publish")), 200)
			}
			h.refresh()
			h.Gateway.CodeLedger = &resources.CodeStore{Pool: h.Pool}
			h.Gateway.CodeAuthorizer = &providers.CodeAuthorizer{Pool: h.Pool, Credentials: h.Runtime, Plugins: grantRefresher(t, h).Plugins}
			mux := http.NewServeMux()
			gateway.NewCodeForwarder().RegisterCode(mux, h.Gateway)
			server := httptest.NewServer(mux)
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, server.URL+"/code/qualification/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + f.key}, "Thread-Id": {"credential-root"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			frame := []byte(`{"type":"response.create","model":"gpt-5.4","input":[]}`)
			generate := func() {
				t.Helper()
				if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
				if _, err := codexfixture.ReadGeneration(ctx, conn); err != nil {
					t.Fatal(err)
				}
			}
			generate()
			if target == "account" {
				enrollment := startGrantEnrollment(t, h, owner, providerPath)
				pollDue(t, h, enrollment)
				status := pollGrantEnrollment(h, owner, providerPath, enrollment, 200)
				wantStatus(t, status, "completed")
				replacement := status["completion"].(map[string]any)["credential_id"].(string)
				accountPath := "/api/v1/code/accounts/" + account["id"].(string)
				rotated := h.want(owner, "PUT", accountPath, map[string]any{"project_id": f.project, "provider_id": account["provider_id"], "credential_id": replacement, "name": account["name"], "enabled": true, "models": account["models"]}, etagHeader(account), 200)
				if replacement == credentialID || rotated["principal"] != account["principal"] {
					t.Fatal("fixture did not rotate a credential for the same principal")
				}
				generate()
			}
			before := len(f.peer.Requests())
			collection, reason := "credentials", "code_account_unavailable"
			if target == "network" {
				collection, reason = "network-credentials", "code_network_credential_unavailable"
			}
			provider := h.want(owner, "GET", providerPath, nil, nil, 200)
			h.want(owner, "POST", providerPath+"/"+collection+"/"+credentialID+"/revoke", nil, withMatch(provider, idem("socket-revoke")), 200)
			h.refresh()
			if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
				t.Fatal(err)
			}
			_, _, err = conn.Read(ctx)
			close, ok := errors.AsType[websocket.CloseError](err)
			if !ok || close.Code != websocket.StatusPolicyViolation || close.Reason != reason || len(f.peer.Requests()) != before {
				t.Fatalf("revoked %s credential dispatched on its existing socket: %v", target, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				var aborted int
				if err := h.Pool.QueryRow(ctx, `SELECT count(*) FROM olp.code_attempts WHERE state='aborted'`).Scan(&aborted); err != nil {
					t.Fatal(err)
				}
				if aborted == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("refused generation did not release its durable reservation")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
