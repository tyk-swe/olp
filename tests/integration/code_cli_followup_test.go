//go:build integration && codecli

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/codemode"
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
	if err := os.WriteFile(filepath.Join(client.Home, "config.toml"), []byte(configuration["configuration"].(string)), 0600); err != nil {
		t.Fatal(err)
	}
	flags := []string{"-c", `approval_policy="never"`, "-c", `sandbox_mode="read-only"`, "-c", `web_search="disabled"`, "-c", fmt.Sprintf("model_providers.olp.supports_websockets=%t", ws)}
	return client, capture, flags
}

func followupAccounting(t *testing.T, f *codeFleet, uncertain int) {
	t.Helper()
	requests := f.peer.Requests()
	var attempts []codemode.Attempt
	deadline := time.Now().Add(5 * time.Second)
	for {
		attempts = codePublicDecode[[]codemode.Attempt](t, f.list("attempts"))
		finished := len(attempts) == len(requests)
		for _, a := range attempts {
			finished = finished && a.FinishedAt != nil
		}
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d upstream generations vs unfinished attempts: %+v", len(requests), attempts)
		}
		time.Sleep(20 * time.Millisecond)
	}
	bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
	if len(bindings) != 1 {
		t.Fatalf("expected one durable root, got %+v", bindings)
	}
	unknown := 0
	for _, a := range attempts {
		if a.AccountID != bindings[0].AccountID || a.BindingID != bindings[0].ID {
			t.Fatal("retry changed account or binding")
		}
		if a.Operation == "prewarm" {
			if a.ReservedTokens != 0 || a.ReportedTokens != nil {
				t.Fatal("prewarm fabricated usage or reserved a budget")
			}
			continue
		}
		if a.State == "uncertain" && a.ReportedTokens == nil {
			unknown++
		}
	}
	if unknown != uncertain {
		t.Fatalf("uncertain attempts=%d, want %d: %+v", unknown, uncertain, attempts)
	}
	for _, r := range requests {
		if r.Header.Get("Authorization") != requests[0].Header.Get("Authorization") || r.Header.Get("Thread-Id") != bindings[0].Conversation || r.Header.Get("X-OLP-Code-Model") != "" {
			t.Fatal("pin, identity or OLP control boundary changed")
		}
	}
}

func TestCodeCLIFollowupAutomaticRetriesAndRecovery(t *testing.T) {
	for _, scenario := range []string{"http-request-retry", "http-stream-retry", "websocket-reconnect", "websocket-http-fallback"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.SetResponder(codexfixture.NewJourney("complete").Reply)
			ws := strings.HasPrefix(scenario, "websocket")
			unknown := 1
			switch scenario {
			case "http-request-retry":
				f.peer.Faults("unavailable")
			case "http-stream-retry", "websocket-reconnect":
				f.peer.Faults("disconnect")
			case "websocket-http-fallback":
				f.peer.RejectWebSockets(http.StatusUpgradeRequired)
				unknown = 0
			}
			client, ingress, flags := followupClient(t, f, ws)
			if scenario == "http-request-retry" {
				ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
				defer cancel()
				args := append([]string{"exec", "--skip-git-repo-check", "--json"}, flags...)
				out, err := client.Command(ctx, append(args, "Complete this controlled recovery task.")...).CombinedOutput()
				if err == nil || ctx.Err() != nil || !bytes.Contains(out, []byte("code_account_unavailable")) {
					t.Fatalf("expected bounded retry exhaustion during account quarantine: %v\n%s", err, out)
				}
				if len(ingress.captures()) < 2 || len(f.peer.Requests()) != 1 {
					t.Fatal("client retry bypassed account quarantine or OLP replayed")
				}
				followupAccounting(t, f, 1)
				return
			}
			client.Exec(t, append(flags, "Complete this controlled recovery task.")...)
			requests := f.peer.Requests()
			incoming := ingress.captures()
			for _, r := range incoming {
				if r.Header.Get("X-OLP-Code-Model") != codecli.Model {
					t.Fatal("generated model hint missing at ingress")
				}
			}
			if !ws {
				if len(incoming) != 2 || len(requests) != 2 {
					t.Fatalf("client attempts=%d upstream=%d; expected one dispatch per client retry", len(incoming), len(requests))
				}
				for i, r := range incoming {
					if !bytes.Equal(r.Body, requests[i].Body) {
						t.Fatal("retry body changed at OLP")
					}
				}
			} else if scenario == "websocket-reconnect" {
				if len(f.peer.Handshakes()) < 2 {
					t.Fatal("client did not reconnect")
				}
				for _, r := range requests {
					if !r.WebSocket {
						t.Fatal("reconnect silently fell back instead")
					}
				}
			} else {
				if len(f.peer.Handshakes()) == 0 || len(requests) != 1 || requests[0].WebSocket {
					t.Fatal("automatic HTTP fallback not observed")
				}
			}
			followupAccounting(t, f, unknown)
		})
	}
}

func TestCodeCLIFollowupRetryRechecksAuthority(t *testing.T) {
	for _, target := range []string{"pool", "account", "key", "project", "retirement"} {
		for _, ws := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/websocket=%t", target, ws), func(t *testing.T) {
				f := newCodeFleet(t)
				key, memberID := f.memberKey(t)
				f.peer.SetResponder(codexfixture.NewJourney("complete").Reply)
				f.peer.Faults("disconnect")
				client, ingress, flags := followupClient(t, f, ws)
				ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
				defer cancel()
				cmd := client.Command(ctx, append([]string{"exec", "--skip-git-repo-check", "--json"}, append(flags, "Complete the controlled retry task.")...)...)
				var output bytes.Buffer
				cmd.Stdout, cmd.Stderr = &output, &output
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				deadline := time.Now().Add(10 * time.Second)
				before := 0
				for before == 0 {
					requests := f.peer.Requests()
					for _, r := range requests {
						var request struct {
							Generate *bool `json:"generate"`
						}
						if json.Unmarshal(r.Body, &request) != nil {
							t.Fatal("invalid captured request")
						}
						if request.Generate == nil || *request.Generate {
							before = len(requests)
							break
						}
					}
					if time.Now().After(deadline) {
						t.Fatal("client never dispatched")
					}
					if before == 0 {
						time.Sleep(10 * time.Millisecond)
					}
				}
				f.revoke(t, target, key, memberID)
				err := cmd.Wait()
				if err == nil || ctx.Err() != nil {
					t.Fatalf("expected prompt refusal after client retry, err=%v\n%s", err, output.String())
				}
				if len(ingress.captures()) < 2 || len(f.peer.Requests()) != before {
					t.Fatal("retry was absent or bypassed fresh authority")
				}
				if target == "pool" && len(f.list("refusals")) == 0 {
					t.Fatal("missing refusal metadata")
				}
				followupAccounting(t, f, 1)
			})
		}
	}
}

func TestCodeCLIFollowupFilesAndLocalSearch(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.SetResponder(codexfixture.NewJourney("files").Reply)
			client, _, flags := followupClient(t, f, ws)
			flags = append(flags, "-c", `sandbox_mode="workspace-write"`)
			output := client.Exec(t, append(flags, "Create the controlled file and search/read it with local tools.")...)
			body, err := os.ReadFile(filepath.Join(client.Work, "qualified.txt"))
			if err != nil || string(body) != "OLP_CONTROLLED_FILE\n" {
				t.Fatalf("official local tool did not write file: %q %v\n%s", body, err, output)
			}
			write, search := false, false
			for _, request := range f.peer.Requests() {
				if request.WebSocket != ws {
					t.Fatal("tool workflow changed transport")
				}
				write = write || bytes.Contains(request.Body, []byte(`function_call_output`)) && bytes.Contains(request.Body, []byte(`OLP_FILE_WRITTEN`))
				search = search || bytes.Contains(request.Body, []byte(`function_call_output`)) && bytes.Contains(request.Body, []byte(`1:OLP_CONTROLLED_FILE`))
			}
			if !write || !search {
				t.Fatalf("tool result continuation missing: write=%t search=%t", write, search)
			}
			followupAccounting(t, f, 0)
		})
	}
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
			if ctx.Err() != nil || err == nil || !bytes.Contains(out, []byte("code_parent_unresolved")) {
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
			if len(f.peer.Requests()) != 0 || len(f.peer.Handshakes()) != 0 || len(f.list("bindings")) != 0 || len(f.list("attempts")) != 0 {
				t.Fatal("unresolved review child bound or reached upstream")
			}
			t.Log("Official fresh exec review sends a review child before any parent request; no parent binding exists and OLP refuses before dispatch.")
		})
	}
}

func TestCodeCLIFollowupReviewAfterExplicitParentTask(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(fmt.Sprintf("websocket=%t", ws), func(t *testing.T) {
			f := newCodeFleet(t)
			f.peer.SetResponder(codexfixture.NewJourney("review").Reply)
			client, _, flags := followupClient(t, f, ws)
			parent := client.ReviewAfterTask(t, flags...)
			bindings := codePublicDecode[[]codemode.Binding](t, f.list("bindings"))
			if len(bindings) != 2 {
				t.Fatalf("expected parent and review child: %+v", bindings)
			}
			for _, binding := range bindings {
				if binding.RootID != bindings[0].RootID || binding.AccountID != bindings[0].AccountID {
					t.Fatal("review changed root or account")
				}
			}
			child := false
			for _, request := range f.peer.Requests() {
				if request.WebSocket != ws {
					t.Fatal("review changed transport")
				}
				if request.Header.Get("X-Openai-Subagent") == "review" {
					child = true
					if request.Header.Get("X-Codex-Parent-Thread-Id") != parent {
						t.Fatal("review lost explicit parent")
					}
				}
			}
			if !child {
				t.Fatal("no official review child observed")
			}
		})
	}
}
