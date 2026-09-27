//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// An owner permits the reference plugin, built natively from the SDK, as an
// unconfined plugin in a deployment that enables the tier: its signing hook
// then signs control's probe and certification and the gateway's unary and
// streaming requests from a subprocess. A replica of the same installation
// whose deployment doesn't enable the tier lists none of it, pins none of it,
// refuses to activate a revision pinning it and refuses its targets.
func TestOwnerPermitsAnUnconfinedPluginThatSignsRequests(t *testing.T) {
	disabled := newAccessHarness(t)
	owner := disabled.owner()
	upstream := newSignedUpstream(t, pluginCredential)
	dir := t.TempDir()
	testutil.BuildExecutablePlugin(t, filepath.Join(dir, "reference"), "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1")
	h := newUnconfinedHarness(t, disabled.Pool, disabled.DBURL, dir)

	if listed := disabled.want(owner, "GET", "/api/v1/plugins", nil, nil, 200); listed["unconfined_plugins_enabled"] != false {
		t.Fatalf("a deployment without the tier reported %v", listed)
	}
	if code := problemCode(t, disabled.want(owner, "GET", "/api/v1/unconfined-plugins", nil, nil, 404)); code != "unconfined_plugins_disabled" {
		t.Fatalf("listing executables without the tier: %s", code)
	}
	disabled.want(owner, "GET", "/api/v1/unconfined-plugins/reference", nil, nil, 404)

	// With the tier, the owner sees the executable and reviews its manifest;
	// no one else does.
	if listed := h.want(owner, "GET", "/api/v1/plugins", nil, nil, 200); listed["unconfined_plugins_enabled"] != true {
		t.Fatalf("a deployment with the tier reported %v", listed)
	}
	operator := h.invite(owner, "operator@example.com", "operator")
	h.want(operator, "GET", "/api/v1/unconfined-plugins", nil, nil, 403)
	h.want(operator, "GET", "/api/v1/unconfined-plugins/reference", nil, nil, 403)
	items := h.want(owner, "GET", "/api/v1/unconfined-plugins", nil, nil, 200)["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["name"] != "reference" || items[0].(map[string]any)["permitted"] != false {
		t.Fatalf("executables %v", items)
	}
	digest := items[0].(map[string]any)["digest"].(string)
	review := h.want(owner, "GET", "/api/v1/unconfined-plugins/reference", nil, nil, 200)
	manifest := review["manifest"].(map[string]any)
	if review["digest"] != digest || manifest["name"] != "reference" || !slices.ContainsFunc(manifest["profiles"].([]any), func(p any) bool { return p.(map[string]any)["id"] == "reference-signed-chat" }) {
		t.Fatalf("review %v", review)
	}
	h.want(owner, "GET", "/api/v1/unconfined-plugins/missing", nil, nil, 404)

	// Permitting takes the owner's acknowledgement of the risk and a recent
	// authentication, for the build the owner reviewed.
	permit := "/api/v1/unconfined-plugins/reference/permit"
	h.want(operator, "POST", permit, map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 403)
	h.want(owner, "POST", permit, map[string]any{"digest": digest, "acknowledge_risk": false}, nil, 422)
	if code := problemCode(t, h.want(owner, "POST", permit, map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 428)); code != "reauthentication_required" {
		t.Fatalf("permitting without reauthentication: %s", code)
	}
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "plugin_permit"}, nil, 204)
	if code := problemCode(t, h.want(owner, "POST", permit, map[string]any{"digest": strings.Repeat("0", 64), "acknowledge_risk": true}, nil, 409)); code != "plugin_executable_changed" {
		t.Fatalf("permitting another build: %s", code)
	}
	permitted := h.want(owner, "POST", permit, map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 201)
	if permitted["digest"] != digest || permitted["executable"] != "reference" || permitted["approved_at"] == nil {
		t.Fatalf("permitted %v", permitted)
	}
	// The recent authentication is spent.
	h.want(owner, "POST", permit, map[string]any{"digest": digest, "acknowledge_risk": true}, nil, 428)
	var audited int
	if err := h.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.audit a JOIN olp.users u ON u.id=a.actor_user_id WHERE a.action='plugin.permit' AND a.resource_type='plugin' AND a.resource_id=$1 AND u.email='owner@example.com' AND a.outcome='success'", digest).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("the permission was audited %d times: %v", audited, err)
	}
	if listed := h.want(owner, "GET", "/api/v1/unconfined-plugins", nil, nil, 200)["items"].([]any); listed[0].(map[string]any)["permitted"] != true {
		t.Fatalf("the permitted executable listed %v", listed)
	}

	// Without the tier, nothing lists or pins the unconfined plugin.
	if listed := disabled.want(owner, "GET", "/api/v1/plugins", nil, nil, 200)["items"].([]any); len(listed) != 0 {
		t.Fatalf("a deployment without the tier listed %v", listed)
	}
	disabled.want(owner, "GET", "/api/v1/plugins/"+digest, nil, nil, 404)
	pinned := func(profiles []any) bool {
		return slices.ContainsFunc(profiles, func(p any) bool { return p.(map[string]any)["revision"] == digest })
	}
	if profiles := disabled.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any); pinned(profiles) {
		t.Fatal("a deployment without the tier offered the unconfined plugin's profiles")
	}
	if profiles := h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any); !pinned(profiles) {
		t.Fatal("a deployment with the tier did not offer the unconfined plugin's profiles")
	}
	create := map[string]any{"name": "Reference unconfined", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-signed-chat", "profile_revision": digest}}
	if code := problemCode(t, disabled.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 422)); code != "plugin_unconfined_disabled" {
		t.Fatalf("pinning without the tier: %s", code)
	}

	// With the tier, the subprocess signs every upstream request.
	created := h.want(owner, "POST", "/api/v1/providers", create, idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	certifyPluginProvider(t, h, owner, path)
	certified := upstream.verified.Load()
	if certified < 3 || upstream.refused.Load() != 0 {
		t.Fatalf("control sent %d signed requests, and %d the upstream refused", certified, upstream.refused.Load())
	}
	draft := fidelityDraft("reference-unconfined", created["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	key := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "Reference unconfined", "scopes": []string{"inference"}, "allowed_routes": []string{"reference-unconfined"}}, idem(uuid.NewString()), 201)["secret"].(string)
	h.refresh()
	chat := map[string]any{"model": "reference-unconfined", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	if status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, chat); status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the signed upstream") {
		t.Fatalf("unary through the unconfined plugin: %d %v", status, reply)
	}
	streamed, _ := json.Marshal(map[string]any{"model": "reference-unconfined", "stream": true, "messages": chat["messages"]})
	code, body, _ := h.gatewayRaw("POST", "/v1/chat/completions", key, bytes.NewReader(streamed), map[string]string{"Content-Type": "application/json"})
	if code != 200 || !bytes.Contains(body, []byte("data: [DONE]")) {
		t.Fatalf("stream through the unconfined plugin: %d %s", code, body)
	}
	if served := upstream.verified.Load() - certified; served != 2 || upstream.refused.Load() != 0 {
		t.Fatalf("the gateway sent %d signed requests, and %d the upstream refused", served, upstream.refused.Load())
	}

	// A gateway whose deployment doesn't enable the tier refuses the target,
	// and a control replica refuses to activate a revision pinning it.
	disabled.refresh()
	if status, reply, _ := disabled.gateway("POST", "/v1/chat/completions", key, chat); status != 503 {
		t.Fatalf("a gateway without the tier served the unconfined plugin's target: %d %v", status, reply)
	}
	if served := upstream.verified.Load() - certified; served != 2 {
		t.Fatalf("a gateway without the tier reached the upstream: %d", served)
	}
	detail := disabled.want(owner, "GET", path, nil, nil, 200)
	if code := problemCode(t, disabled.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 422)); code != "plugin_unconfined_disabled" {
		t.Fatalf("activating without the tier: %s", code)
	}
}
