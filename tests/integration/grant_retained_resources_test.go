//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/internal/testutil"
)

func TestRetainedResourceKeepsHistoricalGrantRefreshed(t *testing.T) {
	for _, end := range []string{"expiry", "deletion", "revocation"} {
		t.Run(end, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			authority := testutil.NewOAuthServer(t)
			upstream := newGrantUpstream(t, authority)
			original := installGrantPlugin(t, h, owner, upstream, "0.1.0")
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
			if end == "deletion" {
				detail := h.want(owner, "GET", path, nil, nil, 200)
				cfg := detail["configuration"].(map[string]any)
				cfg["profile_id"] = "reference-device-chat"
				h.want(owner, "PATCH", path, map[string]any{"name": detail["name"], "configuration": cfg}, etagHeader(detail), 200)
				enrollByDevice(t, h, owner, path, slot)
			} else {
				replacement := installGrantPlugin(t, h, owner, upstream, "0.2.0")
				moveToBuild(t, h, owner, path, replacement)
				enrollSlot(t, h, owner, path, slot)
			}
			certifyPluginProvider(t, h, owner, path)
			h.refresh()
			refresher := grantRefresher(t, h)
			dueNow(t, h, credential)
			if !pass(t, refresher) {
				t.Fatal("historical grant was not processed")
			}
			if g := readGrant(t, h, credential); g.lapsed != nil || g.refreshToken == nil || g.generation != 2 {
				t.Fatalf("retained grant: %+v", g)
			}
			secret, generation, err := h.Runtime.Secret(t.Context(), nil, credential)
			var served connectors.GrantCredential
			if err != nil || generation != 2 || json.Unmarshal(secret, &served) != nil || served.PluginDigest != original || served.ProfileID != "reference-grant-chat" {
				t.Fatalf("historical credential lost its current token or enrollment identity: generation=%d, error=%v", generation, err)
			}
			if end == "revocation" {
				detail := h.want(owner, "GET", path, nil, nil, 200)
				h.want(owner, "POST", path+"/credentials/"+credential+"/revoke", nil, withMatch(detail, idem(uuid.NewString())), 200)
				if pass(t, refresher) || readGrant(t, h, credential).refreshToken != nil {
					t.Fatal("retained resource kept an explicitly revoked grant alive")
				}
			} else {
				// Once the resource expires or is deleted, its historical grant can retire.
				query := "UPDATE olp.provider_resources SET expires_at=now()-interval '1 second' WHERE id=$1"
				if end == "deletion" {
					query = "UPDATE olp.provider_resources SET state='deleted' WHERE id=$1"
				}
				if _, err := h.Pool.Exec(t.Context(), query, resource); err != nil {
					t.Fatal(err)
				}
				if !pass(t, refresher) {
					t.Fatal("expired resource did not release its grant")
				}
				if g := readGrant(t, h, credential); g.lapsed == nil || g.refreshToken != nil {
					t.Fatalf("unused grant: %+v", g)
				}
			}
			if _, _, err := h.Runtime.Secret(t.Context(), nil, credential); !errors.Is(err, runtime.ErrCredentialUnavailable) {
				t.Fatalf("historical credential remained usable before the next authority poll: %v", err)
			}
		})
	}
}
