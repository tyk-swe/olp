//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// grantProvider creates a provider draft from the reference plugin's profile
// that authenticates with a grant, and returns its path.
func grantProvider(t *testing.T, h *accessHarness, owner *browser, digest string, options map[string]any) string {
	t.Helper()
	configuration := map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": "reference-grant-chat", "profile_revision": digest}
	if options != nil {
		configuration["options"] = options
	}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Reference account " + digest[:8], "model": vendorModel, "configuration": configuration}, idem(uuid.NewString()), 201)
	return "/api/v1/providers/" + created["id"].(string)
}

// signIn opens a grant enrollment's authorization URL as the operator's
// browser would, and returns the loopback callback URL the authority sends
// it to, which the operator pastes back.
func signIn(t *testing.T, enrollment map[string]any) *url.URL {
	t.Helper()
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := browser.Get(enrollment["authorization_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	callback, err := url.Parse(response.Header.Get("Location"))
	if err != nil || response.StatusCode != http.StatusFound || callback.Query().Get("code") == "" {
		t.Fatalf("sign-in answered %d %q", response.StatusCode, response.Header.Get("Location"))
	}
	return callback
}

func startGrantEnrollment(t *testing.T, h *accessHarness, owner *browser, path string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	return h.want(owner, "POST", path+"/grant-enrollments", nil, etagHeader(detail), 201)
}

func continueGrantEnrollment(h *accessHarness, owner *browser, path string, enrollment map[string]any, input string, status int) map[string]any {
	h.t.Helper()
	return h.want(owner, "POST", path+"/grant-enrollments/"+enrollment["id"].(string)+"/continue", map[string]any{"input": input}, nil, status)
}

// An operator enrolls a grant through the reference plugin: the plugin builds
// the authorization request, the operator signs in upstream and pastes back
// the callback URL, and the grant becomes a credential version that gateways
// serve with its access token and grant facts.
func TestGrantEnrollmentCreatesACredentialVersionThatGatewaysServe(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installGrantPlugin(t, h, owner, upstream, "0.1.0")

	// A grant authenticates the provider, so no pasted credential does.
	pasted := map[string]any{"name": "Pasted", "credential": "static", "configuration": map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": "reference-grant-chat", "profile_revision": digest}}
	if refusal := h.want(owner, "POST", "/api/v1/providers", pasted, idem(uuid.NewString()), 422); refusal["errors"].(map[string]any)["credential"] == nil {
		t.Fatalf("a grant provider took a pasted credential: %v", refusal)
	}
	path := grantProvider(t, h, owner, digest, nil)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if refusal := h.want(owner, "POST", path+"/credentials", map[string]any{"credential": "static"}, withMatch(detail, idem(uuid.NewString())), 422); problemCode(t, refusal) != "credential_forbidden" {
		t.Fatalf("rotated a pasted credential into a grant provider: %v", refusal)
	}
	if refusal := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 422); problemCode(t, refusal) != "credential_required" {
		t.Fatalf("probed without a grant: %v", refusal)
	}
	h.want(owner, "POST", path+"/grant-enrollments", nil, nil, 428)
	viewer := h.invite(owner, "viewer@example.com", "viewer")
	h.want(viewer, "POST", path+"/grant-enrollments", nil, etagHeader(detail), 403)

	enrollment := startGrantEnrollment(t, h, owner, path)
	if !strings.HasPrefix(enrollment["authorization_url"].(string), authority.URL+"/authorize?") {
		t.Fatalf("enrollment %v", enrollment)
	}
	completed := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
	if completed["principal"] != "operator@reference.example" || completed["credential_version"] != float64(1) {
		t.Fatalf("completed %v", completed)
	}

	// The credential version records the plugin, the observed principal and
	// the grant facts, and no grant material leaves OLP.
	response, raw := h.do(owner, "GET", path+"/credentials", nil, nil)
	if response.StatusCode != 200 {
		t.Fatalf("credentials: %d %s", response.StatusCode, raw)
	}
	var credentials struct {
		Items []struct {
			ID            string `json:"id"`
			DraftSelected bool   `json:"draft_selected"`
			Grant         struct {
				PluginDigest string            `json:"plugin_digest"`
				Principal    string            `json:"principal"`
				Facts        map[string]string `json:"facts"`
				ExpiresAt    *time.Time        `json:"expires_at"`
			} `json:"grant"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &credentials); err != nil {
		t.Fatal(err)
	}
	version := credentials.Items[0]
	if len(credentials.Items) != 1 || version.ID != completed["credential_id"] || !version.DraftSelected || version.Grant.PluginDigest != digest ||
		version.Grant.Principal != "operator@reference.example" || version.Grant.Facts["account"] != "acct-reference" ||
		version.Grant.ExpiresAt == nil || time.Until(*version.Grant.ExpiresAt) < 59*time.Minute {
		t.Fatalf("credential versions %s", raw)
	}
	issued := authority.Issued()
	for _, token := range issued {
		if strings.Contains(string(raw), token) {
			t.Fatal("the credential list shows grant material")
		}
	}
	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.enroll", nil, nil, 200)["items"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["outcome"] != "success" || events[0].(map[string]any)["resource_id"] != version.ID {
		t.Fatalf("audit %v", events)
	}

	// Gateway code reads only the access token: with the refresh token made
	// unreadable, the published provider still serves.
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.secrets SET ciphertext='unreadable' WHERE purpose='provider_grant_refresh'"); err != nil {
		t.Fatal(err)
	}
	certifyPluginProvider(t, h, owner, path)
	key := publishRoute(t, h, owner, strictDraft(fidelityDraft("reference-account", strings.TrimPrefix(path, "/api/v1/providers/"))), "Account")
	h.refresh()
	served, _, err := h.Runtime.Secret(t.Context(), h.Runtime.Release(), version.ID)
	if err != nil || !strings.Contains(string(served), issued[0]) || strings.Contains(string(served), issued[1]) {
		t.Fatalf("the credential source serves %s: %v", served, err)
	}
	before := len(upstream.received())
	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-account", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the reference upstream") {
		t.Fatalf("serving with the grant: %d %v", status, reply)
	}
	for _, headers := range upstream.received()[before:] {
		if headers.Get("Authorization") != "Bearer "+issued[0] || headers.Get("X-Reference-Account") != "acct-reference" {
			t.Fatalf("the upstream received %v", headers)
		}
	}
}

// Another sign-in's callback fails the state check, and the enrollment is
// spent all the same.

// Only the principal that started an enrollment continues or cancels it.

// Maintenance purges expired enrollments and their session state.

// The egress policy admits plain HTTP only to 127.0.0.1, not to the same
// authority named localhost, which the plugin may reach once approved.

// An account on a regional API, at the upstream's approved origin, is
// probed, certified and served there.
