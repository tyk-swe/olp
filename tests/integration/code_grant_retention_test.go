//go:build integration

package integration_test

import (
	"testing"

	codexfixture "github.com/tyk-swe/olp/tests/fixtures/codex-qualified"
)

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
	configuration := draft["configuration"].(map[string]any)
	configuration["options"] = map[string]any{"network": map[string]any{"proxy_url": "http://127.0.0.1:1"}}
	h.want(owner, "PATCH", path, map[string]any{
		"name": draft["name"], "configuration": configuration,
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
