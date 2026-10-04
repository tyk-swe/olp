//go:build integration

package integration_test

import (
	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/testutil"
	"testing"
)

func TestRetainedResourceKeepsHistoricalGrantRefreshed(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	original := installGrantPlugin(t, h, owner, upstream, "0.1.0")
	replacement := installGrantPlugin(t, h, owner, upstream, "0.2.0")
	path := grantProvider(t, h, owner, original, nil)
	credential, _ := servingGrant(t, h, owner, path)
	slot := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	resource := uuid.NewString()
	// Record a retained object with the exact historical provider and slot pin.
	if _, err := h.Pool.Exec(t.Context(), `INSERT INTO olp.provider_resources
 (id,kind,api_key_id,route_slug,provider_id,provider_revision_id,route_revision_id,slot_id,credential_id,upstream_id,state,expires_at)
 SELECT $1,'file',k.id,v.slug,p.id,p.active_revision_id,v.id,$2,$3,'retained-file','processed',now()+interval '1 hour'
 FROM olp.providers p JOIN olp.provider_credentials c ON c.provider_id=p.id
 CROSS JOIN olp.api_keys k CROSS JOIN olp.route_revisions v WHERE c.id=$3 LIMIT 1`, resource, slot, credential); err != nil {
		t.Fatal(err)
	}
	moveToBuild(t, h, owner, path, replacement)
	enrollSlot(t, h, owner, path, slot)
	certifyPluginProvider(t, h, owner, path)
	refresher := grantRefresher(t, h)
	dueNow(t, h, credential)
	if !pass(t, refresher) {
		t.Fatal("historical grant was not processed")
	}
	if g := readGrant(t, h, credential); g.lapsed != nil || g.refreshToken == nil || g.generation != 2 {
		t.Fatalf("retained grant: %+v", g)
	}
	// Once the resource expires, its historical grant can be retired.
	if _, err := h.Pool.Exec(t.Context(), "UPDATE olp.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1", resource); err != nil {
		t.Fatal(err)
	}
	if !pass(t, refresher) {
		t.Fatal("expired resource did not release its grant")
	}
	if g := readGrant(t, h, credential); g.lapsed == nil || g.refreshToken != nil {
		t.Fatalf("unused grant: %+v", g)
	}
}
