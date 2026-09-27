//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// Device authorization enrolls grants wherever a pasted-back sign-in does:
// through an unconfined plugin, whose poll steps run in its subprocess, and for
// the credential slot the enrollment names, re-enrolling a slot's grant or
// enrolling one with no credential version yet, such as an imported slot. A
// worker refreshes its grants like any other.
func TestDeviceAuthorizationEnrollsAnySlotThroughAnUnconfinedPlugin(t *testing.T) {
	base := newAccessHarness(t)
	owner := base.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "reference"), "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1", "-X=main.authority="+authority.URL)
	h := newUnconfinedHarness(t, base.Pool, base.DBURL, dir)
	digest := h.want(owner, "GET", "/api/v1/unconfined-plugins/reference", nil, nil, 200)["digest"].(string)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "plugin_permit"}, nil, 204)
	h.want(owner, "POST", "/api/v1/unconfined-plugins/reference/permit", map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 201)
	path := deviceProvider(t, h, owner, digest, "reference-device-chat")
	// enroll approves a device authorization for a slot, or for the default
	// slot when slot is empty, and returns the completion.
	enroll := func(slot string) map[string]any {
		t.Helper()
		var body any
		if slot != "" {
			body = map[string]any{"slot_id": slot}
		}
		detail := h.want(owner, "GET", path, nil, nil, 200)
		enrollment := h.want(owner, "POST", path+"/grant-enrollments", body, etagHeader(detail), 201)
		device, _ := enrollment["device"].(map[string]any)
		if device == nil || slot != "" && enrollment["slot_id"] != slot {
			t.Fatalf("enrollment %v for slot %q", enrollment, slot)
		}
		testutil.DecideDevice(t, device["verification_url"].(string), device["user_code"].(string), "approve")
		pollDue(t, h, enrollment)
		status := pollGrantEnrollment(h, owner, path, enrollment, 200)
		wantStatus(t, status, "completed")
		return status["completion"].(map[string]any)
	}

	first := enroll("")
	certifyPluginProvider(t, h, owner, path)
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	primary := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	if rotated := enroll(primary); rotated["principal"] != first["principal"] || rotated["credential_version"] != float64(2) {
		t.Fatalf("re-enrolled %v", rotated)
	}
	standby := uuid.NewString()
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	h.want(owner, "PUT", path+"/credential-slots/"+standby, map[string]any{"slot": map[string]any{"name": "Standby"}}, withMatch(slots, idem(uuid.NewString())), 200)
	enrolled := enroll(standby)
	for _, item := range h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)["items"].([]any) {
		if slot := item.(map[string]any); slot["id"] == standby && slot["credential_version_id"] != enrolled["credential_id"] {
			t.Fatalf("the enrolled grant does not back its slot: %v", slot)
		}
	}
	activateEnrolled(t, h, owner, path, 200, primary, standby)

	credentialID := enrolled["credential_id"].(string)
	dueNow(t, h, credentialID)
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("the due grant was not refreshed")
	}
	if grant := readGrant(t, h, credentialID); grant.generation != 2 || grant.failures != 0 {
		t.Fatalf("refreshed grant %+v", grant)
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
	// hold, while set, holds each poll until it is closed, once the poll is
	// announced on arrived.
	hold, arrived chan struct{}
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
		a.mu.Lock()
		hold := a.hold
		a.mu.Unlock()
		if hold != nil {
			a.arrived <- struct{}{}
			<-hold
		}
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

	// A status request abandoned while its poll runs leaves the enrollment
	// pending, whatever the poll found, and the next one polls again.
	hold := make(chan struct{})
	authority.mu.Lock()
	authority.hold, authority.arrived = hold, make(chan struct{}, 1)
	authority.mu.Unlock()
	pollDue(t, h, enrollment)
	ctx, abandon := context.WithCancel(t.Context())
	request, err := http.NewRequestWithContext(ctx, "POST", h.HTTP.URL+path+"/grant-enrollments/"+enrollment["id"].(string)+"/poll", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", h.Server.Origin)
	request.Header.Set("X-CSRF-Token", owner.CSRF)
	for _, cookie := range owner.Cookies {
		request.AddCookie(cookie)
	}
	abandoned := make(chan error, 1)
	go func() {
		_, err := http.DefaultClient.Do(request)
		abandoned <- err
	}()
	<-authority.arrived
	abandon()
	if err = <-abandoned; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned status request returned %v", err)
	}
	authority.mu.Lock()
	authority.hold = nil
	authority.mu.Unlock()
	close(hold)
	// Once the poll settles, the claim's lease gives way to the interval.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		var ended, leased bool
		if err = h.Pool.QueryRow(t.Context(), "SELECT continued_at IS NOT NULL, poll_at>=now()+interval '30 seconds' FROM olp.grant_enrollments WHERE id=$1", enrollment["id"]).Scan(&ended, &leased); err != nil {
			t.Fatal(err)
		}
		if ended {
			t.Fatal("an abandoned status request ended the enrollment")
		}
		if !leased {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the abandoned poll never settled")
		}
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
