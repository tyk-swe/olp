//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/internal/usage"
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

// A grant enrollment started on one control replica continues on another: its
// session state is persisted, encrypted, rather than held in memory.
func TestGrantEnrollmentContinuesOnAnotherControlReplica(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)
	enrollment := startGrantEnrollment(t, h, owner, path)
	var stored []byte
	if err := h.Pool.QueryRow(t.Context(), "SELECT ciphertext FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment'", enrollment["id"]).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	callback := signIn(t, enrollment)
	if strings.Contains(string(stored), callback.Query().Get("state")) {
		t.Fatal("the session state is stored in the clear")
	}
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	completed := continueGrantEnrollment(replica, owner, path, enrollment, callback.Query().Get("code")+"#"+callback.Query().Get("state"), 201)
	if completed["principal"] != "operator@reference.example" {
		t.Fatalf("completed on the other replica %v", completed)
	}
}

// A grant enrollment is continued once, by the principal that started it,
// before it expires, with what its own sign-in returned. Each refusal is
// typed, and a continuation that fails is audited without secret values.
func TestGrantEnrollmentRefusesAStateMismatchAnExpiredOrReusedSession(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)

	// Another sign-in's callback fails the state check, and the enrollment is
	// spent all the same.
	mismatched := startGrantEnrollment(t, h, owner, path)
	other := signIn(t, startGrantEnrollment(t, h, owner, path))
	if refusal := continueGrantEnrollment(h, owner, path, mismatched, other.String(), 422); problemCode(t, refusal) != "grant_state_mismatch" {
		t.Fatalf("continued with another sign-in: %v", refusal)
	}
	callback := signIn(t, mismatched)
	if refusal := continueGrantEnrollment(h, owner, path, mismatched, callback.String(), 409); problemCode(t, refusal) != "grant_enrollment_used" {
		t.Fatalf("continued a spent enrollment: %v", refusal)
	}

	reused := startGrantEnrollment(t, h, owner, path)
	callback = signIn(t, reused)
	continueGrantEnrollment(h, owner, path, reused, callback.String(), 201)
	if refusal := continueGrantEnrollment(h, owner, path, reused, callback.String(), 409); problemCode(t, refusal) != "grant_enrollment_used" {
		t.Fatalf("continued an enrollment twice: %v", refusal)
	}

	expired := startGrantEnrollment(t, h, owner, path)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.grant_enrollments SET expires_at=now()-interval '1 second' WHERE id=$1", expired["id"]); err != nil {
		t.Fatal(err)
	}
	if refusal := continueGrantEnrollment(h, owner, path, expired, signIn(t, expired).String(), 410); problemCode(t, refusal) != "grant_enrollment_expired" {
		t.Fatalf("continued an expired enrollment: %v", refusal)
	}

	// Only the principal that started an enrollment continues or cancels it.
	cancelled := startGrantEnrollment(t, h, owner, path)
	operator := h.invite(owner, "operator@example.com", "operator")
	continueGrantEnrollment(h, operator, path, cancelled, signIn(t, cancelled).String(), 404)
	h.want(operator, "DELETE", path+"/grant-enrollments/"+cancelled["id"].(string), nil, nil, 404)
	h.want(owner, "DELETE", path+"/grant-enrollments/"+cancelled["id"].(string), nil, nil, 204)
	continueGrantEnrollment(h, owner, path, cancelled, "code#state", 404)
	h.want(owner, "DELETE", path+"/grant-enrollments/"+reused["id"].(string), nil, nil, 409)

	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.enroll", nil, nil, 200)["items"].([]any)
	outcomes := map[string]int{}
	for _, event := range events {
		outcomes[event.(map[string]any)["outcome"].(string)]++
	}
	if outcomes["success"] != 1 || outcomes["failure"] != 1 {
		t.Fatalf("audit outcomes %v", outcomes)
	}
	var audited string
	if err := h.Pool.QueryRow(t.Context(), "SELECT string_agg(a::text, ' ') FROM olp.audit a WHERE action='provider.grant.enroll'").Scan(&audited); err != nil {
		t.Fatal(err)
	}
	for _, secret := range append(authority.Issued(), other.Query().Get("code"), other.Query().Get("state")) {
		if strings.Contains(audited, secret) {
			t.Fatal("audit recorded a secret value")
		}
	}

	// Maintenance purges expired enrollments and their session state.
	report, err := usage.RunMaintenance(t.Context(), h.Pool, time.Now().Add(2*time.Hour))
	if err != nil || report.GrantEnrollmentRows == 0 {
		t.Fatalf("maintenance purged %d enrollments: %v", report.GrantEnrollmentRows, err)
	}
	var left int
	if err = h.Pool.QueryRow(t.Context(), "SELECT (SELECT count(*) FROM olp.grant_enrollments)+(SELECT count(*) FROM olp.secrets WHERE purpose='grant_enrollment')").Scan(&left); err != nil || left != 0 {
		t.Fatalf("%d enrollment records survived their expiry: %v", left, err)
	}
}

// The plugin reaches its authority only over the provider's network path,
// under the egress policy: through the provider's proxy, and not at all when
// the policy refuses the authority.
func TestPluginHTTPTakesTheProviderNetworkPath(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	var tunnels atomic.Int64
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != strings.TrimPrefix(authority.URL, "http://") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		tunnels.Add(1)
		target, err := net.Dial("tcp", r.Host)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer target.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffered.Flush()
		go io.Copy(target, buffered)
		io.Copy(client, target)
	}))
	t.Cleanup(proxy.Close)
	digest := installGrantPlugin(t, h, owner, upstream, "0.1.0")
	path := grantProvider(t, h, owner, digest, map[string]any{"network": map[string]any{"proxy_url": proxy.URL}})
	enrollment := startGrantEnrollment(t, h, owner, path)
	continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
	if tunnels.Load() == 0 {
		t.Fatal("the plugin reached its authority around the provider's proxy")
	}

	// The egress policy admits plain HTTP only to 127.0.0.1, not to the same
	// authority named localhost, which the plugin may reach once approved.
	local := strings.Replace(authority.URL, "127.0.0.1", "localhost", 1)
	refused := installReferencePlugin(t, h, owner, upstream, "0.2.0", "-X=main.authority="+local)
	path = grantProvider(t, h, owner, refused, nil)
	enrollment = startGrantEnrollment(t, h, owner, path)
	before := len(authority.Issued())
	refusal := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 422)
	if problemCode(t, refusal) != "grant_enrollment_failed" || !strings.Contains(refusal["detail"].(string), "http_failed") || len(authority.Issued()) != before {
		t.Fatalf("the plugin reached an authority the egress policy refuses: %v", refusal)
	}
}

// A grant fact can address a provider: the reference plugin's grant profile
// sends each account's requests to the API base URL its token response
// names. Grant enrollment refuses a base URL outside the plugin's approved
// origins, and says why.
func TestGrantServesAtTheBaseURLItsTokenResponseNames(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	digest := installGrantPlugin(t, h, owner, upstream, "0.1.0")
	path := grantProvider(t, h, owner, digest, nil)

	elsewhere := newGrantUpstream(t, authority)
	authority.SignInAs(testutil.OAuthIdentity{Subject: "operator@reference.example", Account: "acct-elsewhere", APIBase: elsewhere.URL + "/v1"})
	enrollment := startGrantEnrollment(t, h, owner, path)
	refusal := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 422)
	if problemCode(t, refusal) != "grant_enrollment_failed" || !strings.Contains(refusal["detail"].(string), "places requests at "+elsewhere.URL+", which is not one of the plugin's approved origins") ||
		len(elsewhere.received()) != 0 || len(upstream.received()) != 0 {
		t.Fatalf("enrolled a grant whose API is at an unapproved origin: %v", refusal)
	}

	// An account on a regional API, at the upstream's approved origin, is
	// probed, certified and served there.
	authority.SignInAs(testutil.OAuthIdentity{Subject: "operator@reference.example", Account: "acct-eu", APIBase: upstream.URL + "/regions/eu/v1"})
	enrollment = startGrantEnrollment(t, h, owner, path)
	completed := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
	if completed["credential_version"] != float64(1) {
		t.Fatalf("completed %v", completed)
	}
	certifyPluginProvider(t, h, owner, path)
	key := publishRoute(t, h, owner, strictDraft(fidelityDraft("reference-regional", strings.TrimPrefix(path, "/api/v1/providers/"))), "Regional")
	h.refresh()
	before := len(upstream.receivedPaths())
	status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "reference-regional", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the reference upstream") || len(upstream.receivedPaths()) == before {
		t.Fatalf("serving at the grant's base URL: %d %v", status, reply)
	}
	headers := upstream.received()
	for i, received := range upstream.receivedPaths() {
		if received != "/regions/eu/v1/chat/completions" || headers[i].Get("X-Reference-Account") != "acct-eu" {
			t.Fatalf("the upstream received %s for %s", received, headers[i].Get("X-Reference-Account"))
		}
	}
	if len(elsewhere.received()) != 0 {
		t.Fatal("a request reached the unapproved origin")
	}
}
