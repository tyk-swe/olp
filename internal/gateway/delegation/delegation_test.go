package delegation

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/admission"
	"github.com/tyk-swe/olp/internal/gateway/ingress"
	"github.com/tyk-swe/olp/internal/release"
)

const (
	agentSecret  = "olp_agent_key"
	memberSecret = "olp_member_key"
)

type plainKeys struct {
	bySecret map[string]keyauthority.Authority
}

func (k plainKeys) Authenticate(secret string) (*keyauthority.Authority, error) {
	authority, ok := k.bySecret[secret]
	if !ok {
		return nil, release.ErrInvalidKey
	}
	return &authority, nil
}

type delegationKeys struct {
	plainKeys
	mu      sync.Mutex
	grant   release.DelegationGrant
	err     error
	agent   keyauthority.Authority
	key     keyauthority.Authority
	consent string
	bounds  release.DelegationBounds
	creates int
}

func (k *delegationKeys) CreateDelegation(_ context.Context, agent, key keyauthority.Authority, bounds release.DelegationBounds) (release.DelegationGrant, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.agent, k.key, k.bounds, k.creates = agent, key, bounds, k.creates+1
	return k.grant, k.err
}

func (k *delegationKeys) ExchangeMemberConsent(_ context.Context, agent keyauthority.Authority, consent string, bounds release.DelegationBounds) (release.DelegationGrant, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.agent, k.consent, k.bounds, k.creates = agent, consent, bounds, k.creates+1
	return k.grant, k.err
}

func testAuthorities(agentScopes []string) map[string]keyauthority.Authority {
	return map[string]keyauthority.Authority{
		agentSecret:  {ID: "agent-key", Policy: keyauthority.KeyPolicy{Scopes: agentScopes}},
		memberSecret: {ID: "member-key", Policy: keyauthority.KeyPolicy{Scopes: []string{"delegate"}}},
	}
}

func newTestHandler(keys *delegationKeys, capacity int) *Handler {
	gate := ingress.NewGate(keys, admission.NewPool(capacity), nil, 0, time.Now)
	return New(Dependencies{Gate: gate, Keys: keys})
}

func delegationRequest(body string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/delegations", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

type gatewayError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func serve(t *testing.T, handler http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, r)
	return recorder
}

func wantError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code, message string) {
	t.Helper()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d, body %s", recorder.Code, status, recorder.Body)
	}
	var got gatewayError
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatalf("error body is not JSON: %v", err)
	}
	if got.Error.Code != code || got.Error.Message != message {
		t.Errorf("error = %q %q, want %q %q", got.Error.Code, got.Error.Message, code, message)
	}
}

func TestCreateUnavailableWithoutCreator(t *testing.T) {
	t.Parallel()
	keys := plainKeys{bySecret: testAuthorities([]string{"inference"})}
	gate := ingress.NewGate(keys, admission.NewPool(1), nil, 0, time.Now)
	handler := New(Dependencies{
		Gate: gate,
		Keys: keys,
		Begin: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("X-Request-Id", "req_test")
		},
	})
	recorder := serve(t, handler, delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`))
	wantError(t, recorder, http.StatusServiceUnavailable, "authority_unavailable", "Delegation is unavailable.")
	if got := recorder.Header().Get("X-Request-Id"); got != "req_test" {
		t.Errorf("X-Request-Id = %q, want the begin hook to run first", got)
	}
}

func TestCreateOverloaded(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})}}
	recorder := serve(t, newTestHandler(keys, 0), delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`))
	wantError(t, recorder, http.StatusServiceUnavailable, "request_admission_overloaded", "The gateway is at its in-flight request limit. Retry shortly.")
}

func TestCreateRequiresWorkloadIdentity(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})}}
	recorder := serve(t, newTestHandler(keys, 1), delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`))
	wantError(t, recorder, http.StatusUnauthorized, "invalid_api_key", "Provide a workload identity.")
}

func TestCreateRejectsMalformedBounds(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})}}
	r := delegationRequest(`{`)
	r.Header.Set("Authorization", "Bearer "+agentSecret)
	recorder := serve(t, newTestHandler(keys, 1), r)
	wantError(t, recorder, http.StatusBadRequest, "invalid_request", "Provide bounded delegation scopes and resources.")
}

func TestCreateRejectsConsentWithDelegatingKey(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})}}
	r := delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`)
	r.Header.Set("Authorization", "Bearer "+agentSecret)
	r.Header.Set("X-OLP-Delegation-Consent", "consent-token")
	r.Header.Set("X-OLP-Delegating-Key", memberSecret)
	recorder := serve(t, newTestHandler(keys, 1), r)
	wantError(t, recorder, http.StatusBadRequest, "invalid_request", "Provide either a delegating key or member consent.")
}

func TestCreateWithDelegatingKey(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{
		plainKeys: plainKeys{bySecret: testAuthorities([]string{"tools"})},
		grant:     release.DelegationGrant{Token: "olpd_test", DelegationPairDigest: "pair-digest"},
	}
	r := delegationRequest(`{"scopes":["tools"],"allowed_routes":["team-chat"]}`)
	r.Header.Set("Authorization", "Bearer "+agentSecret)
	r.Header.Set("X-OLP-Delegating-Key", memberSecret)
	recorder := serve(t, newTestHandler(keys, 1), r)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %s", recorder.Code, recorder.Body)
	}
	var grant release.DelegationGrant
	if err := json.Unmarshal(recorder.Body.Bytes(), &grant); err != nil {
		t.Fatalf("grant body is not JSON: %v", err)
	}
	if grant.Token != "olpd_test" || grant.DelegationPairDigest != "pair-digest" {
		t.Errorf("grant = %+v, want the creator grant echoed", grant)
	}
	if keys.creates != 1 || keys.agent.ID != "agent-key" || keys.key.ID != "member-key" {
		t.Errorf("creator saw agent %q key %q after %d calls, want one agent-key/member-key call", keys.agent.ID, keys.key.ID, keys.creates)
	}
	if len(keys.bounds.Scopes) != 1 || keys.bounds.Scopes[0] != "tools" || len(keys.bounds.AllowedRoutes) != 1 || keys.bounds.AllowedRoutes[0] != "team-chat" {
		t.Errorf("bounds = %+v, want the submitted scopes and routes", keys.bounds)
	}
}

func TestCreateWithMemberConsent(t *testing.T) {
	t.Parallel()
	keys := &delegationKeys{
		plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})},
		grant:     release.DelegationGrant{Token: "olpd_consent"},
	}
	r := delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`)
	r.Header.Set("Authorization", "Bearer "+agentSecret)
	r.Header.Set("X-OLP-Delegation-Consent", "consent-token")
	recorder := serve(t, newTestHandler(keys, 1), r)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %s", recorder.Code, recorder.Body)
	}
	if keys.consent != "consent-token" || keys.agent.ID != "agent-key" {
		t.Errorf("consenter saw consent %q for agent %q, want the submitted consent for agent-key", keys.consent, keys.agent.ID)
	}
}

func TestCreateGrantErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		err     error
		status  int
		code    string
		message string
	}{
		{name: "bounds", err: release.ErrDelegationBounds, status: http.StatusBadRequest, code: "invalid_request", message: "Provide bounded delegation scopes and resources."},
		{name: "invalid key", err: release.ErrInvalidKey, status: http.StatusUnauthorized, code: "invalid_api_key", message: "The member consent is invalid or expired."},
		{name: "forbidden", err: access.Forbidden(), status: http.StatusForbidden, code: "permission_denied", message: "The principals cannot delegate the requested rights."},
		{name: "unavailable", err: errors.New("database is down"), status: http.StatusServiceUnavailable, code: "authority_unavailable", message: "Delegation is unavailable."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			keys := &delegationKeys{plainKeys: plainKeys{bySecret: testAuthorities([]string{"inference"})}, err: c.err}
			r := delegationRequest(`{"scopes":["inference"],"allowed_routes":["team-chat"]}`)
			r.Header.Set("Authorization", "Bearer "+agentSecret)
			r.Header.Set("X-OLP-Delegating-Key", memberSecret)
			wantError(t, serve(t, newTestHandler(keys, 1), r), c.status, c.code, c.message)
		})
	}
}
