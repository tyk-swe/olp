//go:build integration

package integration_test

import (
	"strings"
	"testing"

	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

func TestCodePublishedConnectionsPreventPluginUninstall(t *testing.T) {
	f := newCodePublicFixture(t)
	h, owner := f.h, f.owner
	var oldDigest string
	replacement := installGrantPlugin(t, h, owner, f.upstream, "0.2.0")
	for _, account := range f.accounts {
		path := "/api/v1/providers/" + account["provider_id"].(string)
		draft := h.want(owner, "GET", path, nil, nil, 200)
		configuration := draft["configuration"].(map[string]any)
		oldDigest = configuration["profile_revision"].(string)
		configuration["profile_revision"] = replacement
		h.want(owner, "PATCH", path, map[string]any{
			"name": draft["name"], "configuration": configuration,
		}, etagHeader(draft), 200)
	}
	plugin := h.want(owner, "GET", "/api/v1/plugins/"+oldDigest, nil, nil, 200)
	refusal := h.want(owner, "DELETE", "/api/v1/plugins/"+oldDigest, nil, etagHeader(plugin), 409)
	if problemCode(t, refusal) != "plugin_pinned" || !strings.Contains(refusal["detail"].(string), `"Controlled code account 0"`) {
		t.Fatalf("published code connections did not protect their plugin: %v", refusal)
	}
	for _, account := range f.accounts {
		grant := readGrant(t, h, account["credential_id"].(string))
		if grant.lapsed != nil || grant.refreshToken == nil || grant.refreshTokens != 1 {
			t.Fatal("refused uninstall retired a published code account's grant")
		}
	}
	f.noSyntheticInference(t)
}

func TestCodeAuthRefreshRetainsPublishedConnectionAfterSlotRotation(t *testing.T) {
	f := newCodePublicFixtureWithPeer(t, codexfixture.New())
	h, owner := f.h, f.owner
	account := f.accounts[0]
	path := "/api/v1/providers/" + account["provider_id"].(string)
	oldCredential := account["credential_id"].(string)
	enrollment := startGrantEnrollment(t, h, owner, path)
	pollDue(t, h, enrollment)
	status := pollGrantEnrollment(h, owner, path, enrollment, 200)
	wantStatus(t, status, "completed")
	if status["completion"].(map[string]any)["credential_id"] == oldCredential {
		t.Fatal("reenrollment did not rotate the provider slot")
	}
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)["items"].([]any)
	for _, item := range slots {
		if item.(map[string]any)["credential_version_id"] == oldCredential {
			t.Fatal("old credential still selected in a provider slot")
		}
	}
	draft := h.want(owner, "GET", path, nil, nil, 200)
	// A credential-boundary edit is refused while a grant is live; a benign
	// rename still proves the refresh survives provider revisions.
	h.want(owner, "PATCH", path, map[string]any{
		"name": draft["name"].(string) + " retained", "configuration": draft["configuration"],
	}, etagHeader(draft), 200)
	dueNow(t, h, oldCredential)
	if !pass(t, grantRefresher(t, h)) {
		t.Fatal("code account grant was not retained for refresh")
	}
	refreshed := readGrant(t, h, oldCredential)
	if refreshed.generation != 2 || refreshed.lapsed != nil || refreshed.failures != 0 {
		t.Fatal("refresh lost the pinned connection or retired the code account credential")
	}
	f.noSyntheticInference(t)
}
