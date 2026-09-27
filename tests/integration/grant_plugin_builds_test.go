//go:build integration

package integration_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/grants"
	"github.com/tyk-swe/olp/internal/testutil"
)

// subscribeToLapses subscribes a webhook destination to grant lapses and
// returns the number of deliveries enqueued so far.
func subscribeToLapses(t *testing.T, h *accessHarness, owner *browser) func() int {
	t.Helper()
	h.Server.Egress = alertPolicy()
	hook := newWebhookFixture(t)
	destination := h.want(owner, "POST", "/api/v1/notifications/destinations", map[string]any{"name": "lapses", "url": hook.URL}, idem(uuid.NewString()), 201)["id"].(string)
	h.want(owner, "POST", "/api/v1/notifications/rules", map[string]any{"name": "Lapses", "event": "provider.grant.lapsed", "destination_id": destination}, idem(uuid.NewString()), 201)
	return func() int {
		t.Helper()
		var deliveries int
		if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.notification_deliveries").Scan(&deliveries); err != nil {
			t.Fatal(err)
		}
		return deliveries
	}
}

// moveToBuild moves a provider draft to another build of its plugin and
// returns the draft.
func moveToBuild(t *testing.T, h *accessHarness, owner *browser, path, digest string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	moved := detail["configuration"].(map[string]any)
	moved["profile_revision"] = digest
	return h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": moved}, etagHeader(detail), 200)
}

// retirements returns the audited retirements of grants.
func retirements(t *testing.T, h *accessHarness, owner *browser) []any {
	t.Helper()
	return h.want(owner, "GET", "/api/v1/audit?action=provider.grant.retire", nil, nil, 200)["items"].([]any)
}

// A grant serves only the plugin build that enrolled it (ADR 0005): a
// provider moved to another build refuses the grant its slot holds until the
// slot is enrolled again, as a configuration plan pinning that build does.
// Until the provider is activated with the new build, its active revision
// serves and keeps its grant refreshed; then a worker retires the grant no
// configuration uses instead of refreshing it, and notifies nobody.
func TestAGrantServesOnlyThePluginBuildThatEnrolledIt(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	deliveries := subscribeToLapses(t, h, owner)
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	enrolling := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	upgrade := installReferencePlugin(t, h, owner, upstream, "0.2.0", "-X=main.authority="+authority.URL)
	path := grantProvider(t, h, owner, enrolling, nil)
	enrolled := enrollGrant(t, h, owner, path)
	certifyPluginProvider(t, h, owner, path)

	detail := moveToBuild(t, h, owner, path, upgrade)
	slot := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	for operation, refusal := range map[string]map[string]any{
		"probe":    h.want(owner, "POST", path+"/probe", nil, etagHeader(detail), 422),
		"validate": h.want(owner, "POST", path+"/credential-slots/"+slot+"/validate", nil, nil, 422),
		"activate": h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 422),
	} {
		if problemCode(t, refusal) != "credential_mismatch" {
			t.Fatalf("%s with another build's grant: %v", operation, refusal)
		}
	}

	// A configuration plan reuses the slot's grant only for the build that
	// enrolled it.
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	provider := document["providers"].([]any)[0].(map[string]any)
	ref := provider["name"].(string) + "/default"
	planned := func(digest string) any {
		t.Helper()
		provider["configuration"].(map[string]any)["profile_revision"] = digest
		for _, item := range h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)["actions"].([]any) {
			if action := item.(map[string]any); action["kind"] == "credential" && action["key"] == ref {
				return action["action"]
			}
		}
		return nil
	}
	if reused, upgraded := planned(enrolling), planned(upgrade); reused != "reuse" || upgraded != "enroll" {
		t.Fatalf("the plan %v the grant for the enrolling build and %v it for another", reused, upgraded)
	}

	// The active revision pins the enrolling build and selects the grant,
	// which keeps refreshing on its behalf.
	refresher := grantRefresher(t, h)
	dueNow(t, h, enrolled)
	if !pass(t, refresher) || readGrant(t, h, enrolled).generation != 2 {
		t.Fatalf("the served grant was not refreshed: %+v", readGrant(t, h, enrolled))
	}

	// Re-enrolled through the new build, the provider activates with it, and
	// no configuration uses the first grant any more: when it comes due, a
	// worker retires it rather than refresh it.
	reenrolled := enrollSlot(t, h, owner, path, slot)["credential_id"].(string)
	certifyPluginProvider(t, h, owner, path)
	issued := len(authority.Issued())
	dueNow(t, h, enrolled)
	if !pass(t, refresher) || len(authority.Issued()) != issued {
		t.Fatal("a worker refreshed a grant no configuration uses")
	}
	if grant := readGrant(t, h, enrolled); grant.lapsed == nil || grant.refreshToken != nil || grant.refresh != nil {
		t.Fatalf("the unused grant is %+v", grant)
	}
	if events := retirements(t, h, owner); len(events) != 1 || events[0].(map[string]any)["resource_id"] != enrolled ||
		events[0].(map[string]any)["actor_type"] != "system" || events[0].(map[string]any)["user_agent_family"] != "olp-worker" {
		t.Fatalf("audited retirements %v", events)
	}
	if n := deliveries(); n != 0 {
		t.Fatalf("the retirement was notified %d times", n)
	}
	dueNow(t, h, reenrolled)
	if !pass(t, refresher) || readGrant(t, h, reenrolled).generation != 2 {
		t.Fatalf("the served grant was not refreshed: %+v", readGrant(t, h, reenrolled))
	}
}

// Uninstalling a plugin retires the grants it enrolled: no provider pins it,
// so none can serve them. Their refresh tokens are discarded at once and they
// lapse, which the slots holding them show until each is enrolled again.
// Nothing served them, so nobody is notified.
func TestUninstallingAPluginRetiresTheGrantsItEnrolled(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	deliveries := subscribeToLapses(t, h, owner)
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	enrolling := installReferencePlugin(t, h, owner, upstream, "0.1.0", "-X=main.authority="+authority.URL)
	upgrade := installReferencePlugin(t, h, owner, upstream, "0.2.0", "-X=main.authority="+authority.URL)
	path := grantProvider(t, h, owner, enrolling, nil)
	enrolled := enrollGrant(t, h, owner, path)
	moveToBuild(t, h, owner, path, upgrade)

	plugin := h.want(owner, "GET", "/api/v1/plugins/"+enrolling, nil, nil, 200)
	h.want(owner, "DELETE", "/api/v1/plugins/"+enrolling, nil, etagHeader(plugin), 204)
	if grant := readGrant(t, h, enrolled); grant.lapsed == nil || grant.refreshToken != nil || grant.refresh != nil {
		t.Fatalf("the uninstalled build's grant is %+v", grant)
	}
	var tokens int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.secrets WHERE purpose=$1", grants.RefreshPurpose).Scan(&tokens); err != nil || tokens != 0 {
		t.Fatalf("%d refresh tokens survived the uninstall: %v", tokens, err)
	}
	if events := retirements(t, h, owner); len(events) != 1 || events[0].(map[string]any)["resource_id"] != enrolled || events[0].(map[string]any)["actor_type"] != "user" {
		t.Fatalf("audited retirements %v", events)
	}
	if n := deliveries(); n != 0 {
		t.Fatalf("the retirement was notified %d times", n)
	}
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	slot := slots["items"].([]any)[0].(map[string]any)["id"].(string)
	if health := slots["health"].(map[string]any)[slot].(map[string]any); health["lapsed"] != true {
		t.Fatalf("slot health %v", health)
	}
}
