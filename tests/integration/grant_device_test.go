//go:build integration

package integration_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// deviceProvider creates a provider draft from a plugin profile that enrolls
// grants by device authorization, and returns its path.
func deviceProvider(t *testing.T, h *accessHarness, owner *browser, digest, profile string) string {
	t.Helper()
	configuration := map[string]any{"kind": "plugin", "auth_mode": "grant", "profile_id": profile, "profile_revision": digest}
	created := h.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Device account " + digest[:8], "model": vendorModel, "configuration": configuration}, idem(uuid.NewString()), 201)
	return "/api/v1/providers/" + created["id"].(string)
}

func pollGrantEnrollment(h *accessHarness, b *browser, path string, enrollment map[string]any, status int) map[string]any {
	h.t.Helper()
	return h.want(b, "POST", path+"/grant-enrollments/"+enrollment["id"].(string)+"/poll", nil, nil, status)
}

// pollDue makes an enrollment's next poll due now, as if its interval passed.
func pollDue(t *testing.T, h *accessHarness, enrollment map[string]any) {
	t.Helper()
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.grant_enrollments SET poll_at=now() WHERE id=$1", enrollment["id"]); err != nil {
		t.Fatal(err)
	}
}

func wantStatus(t *testing.T, got map[string]any, status string) {
	t.Helper()
	if got["status"] != status {
		t.Fatalf("status %v, want %s", got, status)
	}
}

// An operator enrolls a grant by device authorization (RFC 8628) through the
// reference plugin: OLP shows the verification URL and user code, status
// requests poll the authority through the plugin no more often than its
// interval, slowing down when asked, and once the operator approves the device
// upstream the grant becomes a credential version, whichever control replica
// serves the status request.
func TestDeviceAuthorizationGrantEnrollmentPollsUntilTheOperatorApproves(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	path := deviceProvider(t, h, owner, digest, "reference-device-chat")

	enrollment := startGrantEnrollment(t, h, owner, path)
	device, _ := enrollment["device"].(map[string]any)
	expires, err := time.Parse(time.RFC3339Nano, enrollment["expires_at"].(string))
	if err != nil || enrollment["authorization_url"] != nil || device == nil || device["verification_url"] != authority.URL+"/device" ||
		device["user_code"] == "" || device["interval"] != float64(30) || time.Until(expires) < 9*time.Minute || time.Until(expires) > 10*time.Minute {
		t.Fatalf("enrollment %v", enrollment)
	}

	// A status request before the interval has passed polls nothing.
	if status := pollGrantEnrollment(h, owner, path, enrollment, 200); status["status"] != "pending" || status["interval"] != float64(30) || authority.DevicePolls() != 0 {
		t.Fatalf("status %v after %d polls", status, authority.DevicePolls())
	}
	pollDue(t, h, enrollment)
	wantStatus(t, pollGrantEnrollment(h, owner, path, enrollment, 200), "pending")
	// Polled again within its interval, the authority asks OLP to slow down:
	// it waits 5 seconds longer from then on.
	pollDue(t, h, enrollment)
	if status := pollGrantEnrollment(h, owner, path, enrollment, 200); status["status"] != "pending" || status["interval"] != float64(35) || authority.DevicePolls() != 2 {
		t.Fatalf("status %v after %d polls", status, authority.DevicePolls())
	}
	wantStatus(t, pollGrantEnrollment(h, owner, path, enrollment, 200), "pending")
	if authority.DevicePolls() != 2 {
		t.Fatalf("polled %d times within the interval", authority.DevicePolls())
	}

	// The operator approves the device upstream, and another control replica
	// serves the next status request.
	testutil.DecideDevice(t, device["verification_url"].(string), device["user_code"].(string), "approve")
	replica := newAccessHarnessOn(t, h.Pool, h.DBURL)
	pollDue(t, h, enrollment)
	status := pollGrantEnrollment(replica, owner, path, enrollment, 200)
	completion, _ := status["completion"].(map[string]any)
	if status["status"] != "completed" || completion["principal"] != "operator@reference.example" || completion["credential_version"] != float64(1) {
		t.Fatalf("status %v", status)
	}
	// It stays completed, polling no more.
	pollDue(t, h, enrollment)
	if again := pollGrantEnrollment(h, owner, path, enrollment, 200); again["status"] != "completed" ||
		again["completion"].(map[string]any)["credential_id"] != completion["credential_id"] || authority.DevicePolls() != 3 {
		t.Fatalf("status %v after %d polls", again, authority.DevicePolls())
	}

	// The grant is an ordinary grant-backed credential version on the draft,
	// which the connection authenticates with.
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if detail["draft_credential_id"] != completion["credential_id"] || detail["etag"] != completion["etag"] {
		t.Fatalf("draft %v", detail)
	}
	if probe := h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 200); probe["succeeded"] != true {
		t.Fatalf("probe %v", probe)
	}
	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.enroll", nil, nil, 200)["items"].([]any)
	if len(events) != 1 || events[0].(map[string]any)["outcome"] != "success" || events[0].(map[string]any)["resource_id"] != completion["credential_id"] {
		t.Fatalf("audit %v", events)
	}
	var sessions int
	if err = h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose='grant_enrollment'").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("%d session states survived completion: %v", sessions, err)
	}
}

// Polling stops once the operator denies the device, once it expires, as the
// authority reports or by the enrollment's own expiry, and once the enrollment
// is cancelled; a poll that fails ends it. Only the principal that started a
// device authorization polls it, and it is never continued with pasted input.
func TestDeviceAuthorizationGrantEnrollmentStopsWhenDeniedExpiredCancelledOrFailed(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	path := deviceProvider(t, h, owner, digest, "reference-device-chat")
	decide := func(enrollment map[string]any, decision string) {
		device := enrollment["device"].(map[string]any)
		testutil.DecideDevice(t, device["verification_url"].(string), device["user_code"].(string), decision)
	}
	// wantEnded checks that a status request reports the outcome again
	// without polling the authority.
	wantEnded := func(enrollment map[string]any, status string) {
		t.Helper()
		polls := authority.DevicePolls()
		pollDue(t, h, enrollment)
		wantStatus(t, pollGrantEnrollment(h, owner, path, enrollment, 200), status)
		if authority.DevicePolls() != polls {
			t.Fatalf("polled a %s device authorization", status)
		}
	}

	denied := startGrantEnrollment(t, h, owner, path)
	decide(denied, "deny")
	pollDue(t, h, denied)
	wantStatus(t, pollGrantEnrollment(h, owner, path, denied, 200), "denied")
	wantEnded(denied, "denied")

	expired := startGrantEnrollment(t, h, owner, path)
	authority.ExpireDevices()
	pollDue(t, h, expired)
	wantStatus(t, pollGrantEnrollment(h, owner, path, expired, 200), "expired")
	wantEnded(expired, "expired")

	lapsed := startGrantEnrollment(t, h, owner, path)
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.grant_enrollments SET expires_at=now()-interval '1 second' WHERE id=$1", lapsed["id"]); err != nil {
		t.Fatal(err)
	}
	wantEnded(lapsed, "expired")

	cancelled := startGrantEnrollment(t, h, owner, path)
	h.want(owner, "DELETE", path+"/grant-enrollments/"+cancelled["id"].(string), nil, nil, 204)
	polls := authority.DevicePolls()
	pollGrantEnrollment(h, owner, path, cancelled, 404)

	// A device authorization is polled, never continued, and only by the
	// principal that started it; a pasted-back one is never polled.
	pending := startGrantEnrollment(t, h, owner, path)
	pollDue(t, h, pending)
	continueGrantEnrollment(h, owner, path, pending, "code#state", 404)
	operator := h.invite(owner, "operator@example.com", "operator")
	pollGrantEnrollment(h, operator, path, pending, 404)
	if authority.DevicePolls() != polls {
		t.Fatal("polled for another principal")
	}
	pastedPath := grantProvider(t, h, owner, digest, nil)
	pollGrantEnrollment(h, owner, pastedPath, startGrantEnrollment(t, h, owner, pastedPath), 404)

	// A poll that fails, such as one that can't reach the authority, ends the
	// enrollment.
	authority.Close()
	refusal := pollGrantEnrollment(h, owner, path, pending, 422)
	if problemCode(t, refusal) != "grant_enrollment_failed" || !strings.Contains(refusal["detail"].(string), "http_failed") {
		t.Fatalf("a failed poll: %v", refusal)
	}
	pollDue(t, h, pending)
	if refusal = pollGrantEnrollment(h, owner, path, pending, 409); problemCode(t, refusal) != "grant_enrollment_used" {
		t.Fatalf("polled an ended enrollment: %v", refusal)
	}

	// Each outcome the plugin reported is audited as a failure, and the
	// enrollments that ended hold no session state.
	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.enroll", nil, nil, 200)["items"].([]any)
	for _, event := range events {
		if event.(map[string]any)["outcome"] != "failure" {
			t.Fatalf("audit %v", events)
		}
	}
	var sessions int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose='grant_enrollment' AND id = ANY($1::uuid[])",
		[]string{denied["id"].(string), expired["id"].(string), cancelled["id"].(string), pending["id"].(string)}).Scan(&sessions); err != nil || len(events) != 3 || sessions != 0 {
		t.Fatalf("%d audit events, %d session states of ended enrollments: %v", len(events), sessions, err)
	}
}

// deviceLoginAuthority is a fake upstream authority with its own variant of
// device authorization, like ChatGPT Codex's device login: a JSON request
// issues a user code, polls answer 403 until the operator approves, and then
// return an authorization code and its PKCE verifier, which /oauth/token
// exchanges once.
type deviceLoginAuthority struct {
	*httptest.Server
	mu       sync.Mutex
	userCode string
	approved bool
	// code is the authorization code the approved device login returned,
	// until it is exchanged.
	code string
}

func newDeviceLoginAuthority(t *testing.T) *deviceLoginAuthority {
	a := &deviceLoginAuthority{}
	const verifier = "device-login-verifier"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.userCode = "DL-" + uuid.NewString()[:4]
		writeJSON(w, map[string]string{"device_auth_id": "auth-1", "user_code": a.userCode, "interval": "7"})
	})
	mux.HandleFunc("POST /api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		var polled struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if json.NewDecoder(r.Body).Decode(&polled) != nil || polled.DeviceAuthID != "auth-1" || polled.UserCode != a.userCode || !a.approved {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		a.code = uuid.NewString()
		writeJSON(w, map[string]string{"authorization_code": a.code, "code_verifier": verifier})
	})
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.ParseForm() != nil || a.code == "" || r.PostForm.Get("code") != a.code || r.PostForm.Get("code_verifier") != verifier ||
			r.PostForm.Get("redirect_uri") != a.URL+"/deviceauth/callback" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		a.code = ""
		writeJSON(w, map[string]any{"access_token": "device-login-access", "refresh_token": "device-login-refresh", "expires_in": 3600, "account_id": "account-7"})
	})
	a.Server = httptest.NewServer(mux)
	t.Cleanup(a.Close)
	return a
}

// A plugin runs its upstream's own variant of device authorization through
// the same grant enrollment steps, with nothing in OLP specific to it.
func TestCustomDeviceAuthorizationVariantEnrollsThroughPluginSteps(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := newDeviceLoginAuthority(t)
	module := testutil.BuildPlugin(t, "./tests/integration/testdata/devicelogin", "-X=main.authority="+authority.URL)
	installed := h.want(owner, "POST", "/api/v1/plugins", module, wasm, 201)
	h.want(owner, "POST", "/api/v1/plugins/"+digestOf(module)+"/approve", map[string]any{"origins": installed["manifest"].(map[string]any)["origins"]}, etagHeader(installed), 200)
	path := deviceProvider(t, h, owner, digestOf(module), "devicelogin-chat")

	enrollment := startGrantEnrollment(t, h, owner, path)
	device := enrollment["device"].(map[string]any)
	if device["verification_url"] != authority.URL+"/codex/device" || !strings.HasPrefix(device["user_code"].(string), "DL-") || device["interval"] != float64(7) {
		t.Fatalf("enrollment %v", enrollment)
	}
	pollDue(t, h, enrollment)
	wantStatus(t, pollGrantEnrollment(h, owner, path, enrollment, 200), "pending")
	authority.mu.Lock()
	authority.approved = true
	authority.mu.Unlock()
	pollDue(t, h, enrollment)
	status := pollGrantEnrollment(h, owner, path, enrollment, 200)
	if status["status"] != "completed" || status["completion"].(map[string]any)["principal"] != "account-7" {
		t.Fatalf("status %v", status)
	}
}
