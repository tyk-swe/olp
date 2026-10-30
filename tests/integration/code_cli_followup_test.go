//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/tests/codecli"
	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

type cliIngress struct {
	mu       sync.Mutex
	requests []codexfixture.Request
}

func (c *cliIngress) captures() []codexfixture.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]codexfixture.Request(nil), c.requests...)
}

func followupClient(t *testing.T, f *codeFleet, ws bool) (*codecli.Client, *cliIngress, []string) {
	t.Helper()
	// This observer only proxies the public ingress; all admission, authorization
	// and provider traffic still runs in the real gateway process.
	var proxies []*httputil.ReverseProxy
	for _, gateway := range f.gateways {
		target, err := url.Parse(gateway.PublicOrigin)
		if err != nil {
			t.Fatal(err)
		}
		proxies = append(proxies, httputil.NewSingleHostReverseProxy(target))
	}
	capture := &cliIngress{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "capture failed", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		capture.mu.Lock()
		capture.requests = append(capture.requests, codexfixture.Request{Path: r.URL.Path, Header: r.Header.Clone(), Body: body, WebSocket: strings.EqualFold(r.Header.Get("Upgrade"), "websocket")})
		proxy := proxies[(len(capture.requests)-1)%len(proxies)]
		capture.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	client := codecli.New(t, server.URL+"/code/qualification", f.key, ws)
	configuration := f.h.want(f.owner, "GET", "/api/v1/code/routes/"+f.route["id"].(string)+"/client-config?gateway_url="+url.QueryEscape(server.URL), nil, nil, 200)
	client.UseConfiguration(t, configuration["configuration"].(string))
	flags := codecli.ControlledFlags(ws)
	return client, capture, flags
}

func TestCodeCLIFollowupFreshReviewParentEstablishment(t *testing.T) {
	for _, scenario := range []string{"http-only", "websocket", "websocket-http-fallback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.SetResponder(codexfixture.NewJourney("review").Reply)
			ws := scenario != "http-only"
			if scenario == "websocket-http-fallback" {
				f.peer.RejectWebSockets(http.StatusUpgradeRequired)
			}
			client, ingress, flags := followupClient(t, f, ws)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			args := append([]string{"exec", "--skip-git-repo-check", "--json"}, flags...)
			out, err := client.Command(ctx, append(args, "review", "Review the local controlled task.")...).CombinedOutput()
			if ws {
				if err != nil || ctx.Err() != nil {
					t.Fatalf("WebSocket review failed: %v\n%s", err, out)
				}
				requests := ingress.captures()
				bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
				if len(requests) < 2 || len(bindings) != 2 {
					t.Fatalf("expected root handshake and child binding: requests=%d bindings=%+v", len(requests), bindings)
				}
				root := requests[0].Header.Get("Thread-Id")
				if root == "" || !requests[0].WebSocket || requests[0].Header.Get("X-Codex-Parent-Thread-Id") != "" {
					t.Fatal("first handshake did not establish a root")
				}
				child := false
				for _, r := range f.peer.Requests() {
					if scenario == "websocket-http-fallback" && r.WebSocket {
						t.Fatal("review did not exercise HTTP fallback")
					}
					if r.Header.Get("Thread-Id") == root {
						var request struct {
							Generate *bool `json:"generate"`
						}
						if json.Unmarshal(r.Body, &request) != nil || request.Generate == nil || *request.Generate {
							t.Fatal("parent setup generated inference")
						}
					} else {
						child = true
						if r.Header.Get("X-Codex-Parent-Thread-Id") != root || r.Header.Get("X-Openai-Subagent") != "review" {
							t.Fatal("review child lost root identity")
						}
					}
				}
				for _, binding := range bindings {
					if binding.RootID != bindings[0].RootID || binding.AccountID != bindings[0].AccountID {
						t.Fatal("review changed account")
					}
				}
				if !child {
					t.Fatal("review child did not dispatch")
				}
				t.Log("Official WebSocket root handshake establishes a pin before review child; no parent inference.")
				return
			}
			if ctx.Err() != nil || err == nil || !bytes.Contains(out, []byte("409 Conflict")) || !bytes.Contains(out, []byte("full context")) {
				t.Fatalf("expected unresolved-parent refusal: %v\n%s", err, out)
			}
			requests := ingress.captures()
			if len(requests) == 0 {
				t.Fatal("review sent no request")
			}
			for _, r := range requests {
				if r.Header.Get("X-Openai-Subagent") != "review" || r.Header.Get("X-Codex-Parent-Thread-Id") == "" || r.Header.Get("X-Codex-Parent-Thread-Id") == r.Header.Get("Thread-Id") {
					t.Fatal("review no longer starts with distinct child and parent identities")
				}
			}
			refusals := codePublicDecode[[]contract.CodeRefusal](t, f.list("refusals"))
			if len(refusals) == 0 {
				t.Fatal("unresolved review child recorded no refusal")
			}
			for _, refusal := range refusals {
				if refusal.Code != "code_parent_unresolved" || refusal.ApiKeyId.String() != f.keyID || refusal.RouteId.String() != f.route["id"] {
					t.Fatalf("unexpected review refusal: %+v", refusal)
				}
			}
			if len(f.peer.Requests()) != 0 || len(f.peer.Handshakes()) != 0 || len(f.list("bindings")) != 0 || len(f.list("attempts")) != 0 {
				t.Fatal("unresolved review child bound or reached upstream")
			}
			t.Log("Official fresh exec review sends a review child before any parent request; no parent binding exists and OLP refuses before dispatch.")
		})
	}
}
