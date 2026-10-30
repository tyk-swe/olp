package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/resources"
)

type codeTestLedger struct {
	mu         sync.Mutex
	inputs     []resources.CodeAdmission
	marks      []string
	aborts     []string
	usages     []codemode.Usage
	health     []string
	allowances []codemode.Allowance
	refusals   []string
	refuse     string
	done       chan codemode.Usage
	references map[string]bool
	outcomes   []codemode.Outcome
	offers     codeclient.Offers
	modelsErr  error
}

func (l *codeTestLedger) Models(context.Context, codemode.Route, string) (codeclient.Offers, error) {
	return l.offers, l.modelsErr
}

func (l *codeTestLedger) ObserveOutcome(_ context.Context, _ string, outcome codemode.Outcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.outcomes = append(l.outcomes, outcome)
	return nil
}

func (l *codeTestLedger) Admit(_ context.Context, in resources.CodeAdmission) (resources.CodePermit, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inputs = append(l.inputs, in)
	if in.PreviousResponse != "" && !l.references[in.PreviousResponse] {
		return resources.CodePermit{}, codemode.Refuse(409, "code_parent_unresolved")
	}
	if l.refuse != "" {
		return resources.CodePermit{}, codemode.Refuse(409, l.refuse)
	}
	permit, err := codeTestPermit(in.APIKeyID, in.Providers)
	permit.Attempt = codemode.Attempt{ID: fmt.Sprint(len(l.inputs))}
	return permit, err
}

// codeTestPermit pins an account of the first provider serving the request.
func codeTestPermit(key string, providers []string) (resources.CodePermit, error) {
	if len(providers) == 0 {
		return resources.CodePermit{}, codemode.Refuse(503, "code_account_unavailable")
	}
	return resources.CodePermit{
		Authority: access.Authority{ID: key, Policy: access.KeyPolicy{Scopes: []string{"inference", "models_read"}}},
		Account:   codemode.Account{ID: "account", ProviderID: providers[0], Principal: "principal"},
		Binding:   codemode.Binding{ID: "binding", AccountID: "account", Principal: "principal"},
		Pin:       codemode.Pin{AccountID: "account", Principal: "principal"},
	}, nil
}
func (l *codeTestLedger) MarkDispatched(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.marks = append(l.marks, id)
	return nil
}
func (l *codeTestLedger) ObserveReference(_ context.Context, _, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.references == nil {
		l.references = map[string]bool{}
	}
	l.references[id] = true
	return nil
}
func (l *codeTestLedger) BindConnection(_ context.Context, _ codemode.Route, key string, _ codemode.Identity, _ string, providers []string) (resources.CodePermit, error) {
	return codeTestPermit(key, providers)
}
func (l *codeTestLedger) Settle(_ context.Context, _ string, u codemode.Usage) error {
	l.mu.Lock()
	l.usages = append(l.usages, u)
	l.mu.Unlock()
	l.done <- u
	return nil
}
func (l *codeTestLedger) Abort(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.aborts = append(l.aborts, id)
	return nil
}
func (l *codeTestLedger) ObserveHealth(_ context.Context, _ string, status string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.health = append(l.health, status)
	return nil
}
func (l *codeTestLedger) ObserveAllowance(_ context.Context, _ string, a codemode.Allowance) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.allowances = append(l.allowances, a)
	return nil
}
func (l *codeTestLedger) RecordRefusal(_ context.Context, _ codemode.Route, _, _ string, reason string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refusals = append(l.refusals, reason)
	return nil
}
func (l *codeTestLedger) wait(t *testing.T) codemode.Usage {
	t.Helper()
	select {
	case u := <-l.done:
		return u
	case <-time.After(5 * time.Second):
		t.Fatal("attempt not settled")
		return codemode.Usage{}
	}
}

type codeTestAuthorizer struct{ principal string }

func (a codeTestAuthorizer) AuthorizeCode(_ context.Context, _ routing.Configuration, _ codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error) {
	vendor, _ := codeadapter.Lookup(dispatch.Adapter)
	endpoint, _ := vendor.Endpoint(dispatch.ClientPath)
	value := "upstream-only"
	if endpoint.CredentialHeader == "Authorization" {
		value = "Bearer " + value
	}
	headers := http.Header{endpoint.CredentialHeader: {value}}
	if endpoint.Observation == codeadapter.ObserveCodex {
		headers.Set("Chatgpt-Account-Id", "upstream-account")
	}
	return codemode.Authorization{Principal: a.principal, Headers: headers}, nil
}

func newCodeForwardHarness(t *testing.T) (*harness, *codeTestLedger, *httptest.Server) {
	t.Helper()
	h := newHarness(t, Config{})
	ledger := &codeTestLedger{done: make(chan codemode.Usage, 100)}
	publishedAt := time.Now()
	route := codemode.Route{ID: "route", Slug: "coding", ProjectID: "project", PoolID: "pool", RevisionID: "revision", Revision: 1, PublishedAt: &publishedAt, Enabled: true, Models: []string{"native-model"}}
	authority := h.rt.keys[fullKey]
	authority.ProjectID = &route.ProjectID
	h.rt.keys[fullKey] = authority
	h.rt.release.Snapshot.CodeRoutes = map[string]codemode.Route{"coding": route}
	h.rt.release.Snapshot.CodeConnections = map[string]routing.Configuration{"revision:provider": {Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, Endpoint: h.upstream.URL + "/a"}}
	if err := h.rt.release.Snapshot.Compile(); err != nil {
		t.Fatal(err)
	}
	h.gateway.CodeLedger = ledger
	h.gateway.CodeAuthorizer = codeTestAuthorizer{principal: "principal"}
	mux := http.NewServeMux()
	h.gateway.RegisterCode(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return h, ledger, server
}

func codeDo(t *testing.T, server *httptest.Server, body []byte, extra http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/code/coding/responses", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header = extra.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	r.Header.Set("Authorization", "Bearer "+fullKey)
	r.Header.Set("Session_id", "root")
	client := &http.Client{Transport: &http.Transport{DisableCompression: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	t.Cleanup(client.CloseIdleConnections)
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func TestCodeModelDiscoveryUsesAuthorizedOffers(t *testing.T) {
	h, ledger, server := newCodeForwardHarness(t)
	authority := h.rt.keys[fullKey]
	authority.Policy.Scopes = []string{"inference"}
	h.rt.keys[fullKey] = authority
	route := h.rt.release.Snapshot.CodeRoutes["coding"]
	route.Models = []string{"private-code-v42", "unavailable-model"}
	h.rt.release.Snapshot.CodeRoutes["coding"] = route
	ledger.offers = codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"private-code-v42", "unpublished-model"}}}
	for _, path := range []string{"models", "v1/models"} {
		t.Run(path, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/"+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Authorization", "Bearer "+fullKey)
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			var body struct {
				Object string `json:"object"`
				Data   []struct {
					ID           string `json:"id"`
					Capabilities []struct {
						Protocol   string   `json:"protocol"`
						Path       string   `json:"path"`
						Transports []string `json:"transports"`
					} `json:"capabilities"`
				} `json:"data"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" || body.Object != "list" || len(body.Data) != 1 || body.Data[0].ID != "private-code-v42" {
				t.Fatalf("discovery status %d body %+v", response.StatusCode, body)
			}
			if len(body.Data[0].Capabilities) == 0 || body.Data[0].Capabilities[0].Protocol != "responses" || body.Data[0].Capabilities[0].Path != "responses" {
				t.Fatalf("missing native protocol capabilities %+v", body)
			}
		})
	}
}

func TestCodeModelDiscoveryEnforcesAuthority(t *testing.T) {
	for _, tc := range []struct {
		name      string
		key       string
		liveError error
		status    int
	}{
		{name: "missing key", status: 401},
		{name: "read scope only", key: readKey, status: 403},
		{name: "other route", key: otherKey, status: 404},
		{name: "pool assignment revoked", key: fullKey, liveError: codemode.Refuse(403, "code_pool_denied"), status: 403},
		{name: "key revoked", key: fullKey, liveError: codemode.Refuse(403, "code_permission_denied"), status: 403},
		{name: "revision changed", key: fullKey, liveError: codemode.Refuse(409, "code_route_revision_changed"), status: 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ledger, server := newCodeForwardHarness(t)
			ledger.offers = codeclient.Offers{{Adapter: codemode.AdapterCodex, Models: []string{"native-model"}}}
			ledger.modelsErr = tc.liveError
			r, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/code/coding/models", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.key != "" {
				r.Header.Set("Authorization", "Bearer "+tc.key)
			}
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status || strings.Contains(string(body), "native-model") {
				t.Fatalf("unauthorized discovery %d %s", response.StatusCode, body)
			}
		})
	}
}

// The revision froze another provider's connection, not the account's.
