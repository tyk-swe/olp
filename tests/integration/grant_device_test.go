//go:build integration

package integration_test

import (
	"path/filepath"
	"strings"
	"testing"

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

// enrollByDevice approves a device authorization for a provider's credential
// slot, or for its default slot when slot is empty, and returns the
// completion.
func enrollByDevice(t *testing.T, h *accessHarness, owner *browser, path, slot string) map[string]any {
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

func wantStatus(t *testing.T, got map[string]any, status string) {
	t.Helper()
	if got["status"] != status {
		t.Fatalf("status %v, want %s", got, status)
	}
}

// A status request before the interval has passed polls nothing.

// Polled again within its interval, the authority asks OLP to slow down:
// it waits 5 seconds longer from then on.

// Slowing down never stretches the interval beyond the five minutes a
// device authorization may ask for.

// The operator approves the device upstream, and another control replica
// serves the next status request.

// It stays completed, polling no more.

// The grant is an ordinary grant-backed credential version on the draft,
// which the connection authenticates with.

// Polling stops once the operator denies the device, once it expires, as the
// authority reports or by the enrollment's own expiry, and once the enrollment
// is cancelled; a poll that fails ends it. Only the principal that started a
// device authorization polls it, and it is never continued with pasted input.
// Every way an enrollment ends without a grant, a start the plugin fails
// included, is audited once.
func TestDeviceAuthorizationGrantEnrollmentStopsWhenDeniedExpiredCancelledOrFailed(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installGrantPlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0")
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
	// So does a start that can't reach the authority.
	detail := h.want(owner, "GET", path, nil, nil, 200)
	if refusal = h.want(owner, "POST", path+"/grant-enrollments", nil, etagHeader(detail), 422); problemCode(t, refusal) != "grant_enrollment_failed" {
		t.Fatalf("a failed start: %v", refusal)
	}

	// Each way an enrollment ended without a grant is audited once, as a
	// failure: denied, expired as the authority reported and by its own
	// expiry, the failed poll and the failed start. The enrollments that
	// ended hold no session state.
	events := h.want(owner, "GET", "/api/v1/audit?action=provider.grant.enroll", nil, nil, 200)["items"].([]any)
	for _, event := range events {
		if event.(map[string]any)["outcome"] != "failure" || event.(map[string]any)["resource_id"] != strings.TrimPrefix(path, "/api/v1/providers/") {
			t.Fatalf("audit %v", events)
		}
	}
	var sessions int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose='grant_enrollment' AND id = ANY($1::uuid[])",
		[]string{denied["id"].(string), expired["id"].(string), lapsed["id"].(string), cancelled["id"].(string), pending["id"].(string)}).Scan(&sessions); err != nil || len(events) != 5 || sessions != 0 {
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
	digest := h.want(owner, "POST", "/api/v1/unconfined-plugins/reference/review", nil, nil, 200)["digest"].(string)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "plugin_permit"}, nil, 204)
	h.want(owner, "POST", "/api/v1/unconfined-plugins/reference/permit", map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 201)
	path := deviceProvider(t, h, owner, digest, "reference-device-chat")
	enroll := func(slot string) map[string]any { return enrollByDevice(t, h, owner, path, slot) }

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

// A status request abandoned while its poll runs leaves the enrollment
// pending, whatever the poll found, and the next one polls again.

// Once the poll settles, the claim's lease gives way to the interval.
