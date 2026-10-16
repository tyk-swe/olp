package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

// newCodeVendorHarness serves the "coding" route through a coding-plan
// profile's connection instead of Codex's.
func newCodeVendorHarness(t *testing.T, profile string) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h, ledger, server := newCodeForwardHarness(t)
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	return h, ledger, server
}

func codeVendorDo(t *testing.T, server *httptest.Server, method, path string, body []byte, header http.Header) (*http.Response, []byte) {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/code/coding/"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = header.Clone()
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	got, _ := io.ReadAll(response.Body)
	return response, got
}

func TestCodeOpenCodeGoResponsesUseClientIdentityAndReferences(t *testing.T) {
	h, ledger, server := newCodeVendorHarness(t, codeplans.OpenCodeGoProfile)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/a/responses" || r.Header.Get("Authorization") != "Bearer upstream-only" {
			t.Errorf("request: %s %v", r.URL.Path, r.Header)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"codex.rate_limits\",\"rate_limits\":{\"primary\":{\"used_percent\":100}}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	})
	header := http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_1"}}
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"gpt-5.5","store":false}`), header); response.StatusCode != 200 {
		t.Fatalf("status %d", response.StatusCode)
	}
	if u := ledger.wait(t); *u.Total != 3 {
		t.Fatalf("usage %+v", u)
	}
	if response, _ := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"gpt-5.5","previous_response_id":"resp_1"}`), header); response.StatusCode != 200 {
		t.Fatalf("continuation status %d", response.StatusCode)
	}
	ledger.wait(t)
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if ledger.inputs[1].PreviousResponse != "resp_1" || ledger.inputs[0].Operation.Identity.Conversation != "ses:5f1" || len(ledger.allowances) != 0 {
		t.Fatalf("inputs %+v allowances %v", ledger.inputs, ledger.allowances)
	}
}

func TestCodeMixedRoutesDispatchEachPathToAnAccountOfItsAdapter(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	connection := func(profile string) routing.Configuration {
		return routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile, Endpoint: h.upstream.URL + "/a"}
	}
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{
		"revision:p-codex": connection(codexauth.ProfileID),
		"revision:p-glm":   connection(codeplans.ZAIProfile),
		"revision:p-go":    connection(codeplans.OpenCodeGoProfile),
	}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	type upstream struct {
		path   string
		header http.Header
	}
	requests := make(chan upstream, 1)
	h.mock.set("a", func(w http.ResponseWriter, r *http.Request) {
		requests <- upstream{r.URL.Path, r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	for _, test := range []struct {
		path      string
		model     string
		header    http.Header
		providers []string
		upstream  string
		codex     bool
	}{
		{"responses", "native-model", http.Header{"Authorization": {"Bearer " + fullKey}, "Session_id": {"root"}}, []string{"p-codex"}, "/a/responses", true},
		{"v1/messages", "minimax-m3", http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s1"}}, []string{"p-glm", "p-go"}, "/a/anthropic/v1/messages", false},
		{"v1/messages", "glm-5.3", http.Header{"X-Api-Key": {fullKey}, "X-Claude-Code-Session-Id": {"s2"}}, []string{"p-glm"}, "/a/anthropic/v1/messages", false},
		{"v1/chat/completions", "glm-5.3", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_a"}}, []string{"p-glm", "p-go"}, "/a/coding/paas/v4/chat/completions", false},
		{"v1/responses", "gpt-5.5", http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_b"}}, []string{"p-go"}, "/a/responses", false},
	} {
		response, body := codeVendorDo(t, server, http.MethodPost, test.path, []byte(`{"model":"`+test.model+`"}`), test.header)
		if response.StatusCode != 200 {
			t.Fatalf("%s: %d %s", test.path, response.StatusCode, body)
		}
		got := <-requests
		ledger.wait(t)
		ledger.mu.Lock()
		providers := ledger.inputs[len(ledger.inputs)-1].Providers
		ledger.mu.Unlock()
		if got.path != test.upstream || !slices.Equal(providers, test.providers) || (got.header.Get("Chatgpt-Account-Id") != "") != test.codex {
			t.Fatalf("%s: upstream %s providers %v headers %v", test.path, got.path, providers, got.header)
		}
	}
	ledger.mu.Lock()
	inputs := len(ledger.inputs)
	ledger.mu.Unlock()
	response, body := codeVendorDo(t, server, http.MethodPost, "v1/responses", []byte(`{"model":"glm-5.3"}`), http.Header{"Authorization": {"Bearer " + fullKey}, "X-Opencode-Session-Id": {"ses_c"}})
	ledger.mu.Lock()
	admitted := len(ledger.inputs) != inputs
	ledger.mu.Unlock()
	if response.StatusCode != 400 || !strings.Contains(string(body), "code_operation_unsupported") || admitted {
		t.Fatalf("model no adapter serves on the path: %d %s admitted %v", response.StatusCode, body, admitted)
	}
	response, _ = codeVendorDo(t, server, http.MethodPost, "v1/messages/count_tokens", []byte(`{}`), http.Header{"X-Api-Key": {fullKey}})
	ledger.mu.Lock()
	defer ledger.mu.Unlock()
	if response.StatusCode != 404 || !slices.Equal(ledger.refusals, []string{"code_operation_unsupported"}) {
		t.Fatalf("probe on a route serving Messages: %d refusals %v", response.StatusCode, ledger.refusals)
	}
}

type codeHeaderAuthorizer http.Header

func (a codeHeaderAuthorizer) AuthorizeCode(context.Context, routing.Configuration, codemode.Account, codemode.Dispatch) (codemode.Authorization, error) {
	return codemode.Authorization{Principal: "principal", Headers: http.Header(a)}, nil
}
