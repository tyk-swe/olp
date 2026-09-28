//go:build integration

package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// An operator sets the options a plugin profile declares on a provider: the
// profile's hosting adaptation places the upstream address from them, and
// they belong to the provider's configuration like any other setting, in its
// revisions and in configuration exports and imports.
func TestPluginOptionsPlaceTheUpstreamAddress(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	upstream := newPluginUpstream(t, pluginCredential)
	digest := installReferencePlugin(t, h, owner, upstream, "0.1.0")

	// The catalogue describes the options as the JSON Schema of a provider's
	// option values.
	var schema map[string]any
	for _, profile := range h.want(owner, "GET", "/api/v1/provider-profiles", nil, nil, 200)["items"].([]any) {
		if profile := profile.(map[string]any); profile["id"] == "reference-workspace-chat" && profile["revision"] == digest {
			schema, _ = profile["options_schema"].(map[string]any)
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	if workspace, _ := properties["workspace"].(map[string]any); workspace["title"] != "Workspace" || workspace["type"] != "string" || fmt.Sprint(schema["required"]) != "[workspace]" {
		t.Fatalf("catalogued options %v", schema)
	}

	create := func(options map[string]any) map[string]any {
		return map[string]any{"name": "Workspace", "credential": pluginCredential, "model": vendorModel, "configuration": map[string]any{
			"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-workspace-chat", "profile_revision": digest,
			"options": map[string]any{"plugin_options": options},
		}}
	}
	for _, refused := range []struct {
		options map[string]any
		field   string
	}{
		{map[string]any{}, "configuration.options.plugin_options.workspace"},
		{map[string]any{"workspace": "Acme Corp"}, "configuration.options.plugin_options.workspace"},
		{map[string]any{"workspace": "acme", "region": "eu"}, "configuration.options.plugin_options.region"},
	} {
		refusal := h.want(owner, "POST", "/api/v1/providers", create(refused.options), idem(uuid.NewString()), 422)
		if problemCode(t, refusal) != "validation_failed" || refusal["errors"].(map[string]any)[refused.field] == nil {
			t.Fatalf("options %v: %v", refused.options, refusal)
		}
	}
	created := h.want(owner, "POST", "/api/v1/providers", create(map[string]any{"workspace": "acme"}), idem(uuid.NewString()), 201)
	path := "/api/v1/providers/" + created["id"].(string)
	if endpoint := h.want(owner, "GET", path, nil, nil, 200)["configuration"].(map[string]any)["endpoint"]; endpoint != upstream.URL+"/v1/workspaces/acme" {
		t.Fatalf("the provider's endpoint is %v", endpoint)
	}
	certifyPluginProvider(t, h, owner, path)

	draft := fidelityDraft("workspace-strict", created["id"])
	draft["fidelity"] = map[string]any{"mode": "strict"}
	key := publishRoute(t, h, owner, draft, "Workspace")
	serve := func(workspace string) {
		t.Helper()
		h.refresh()
		status, reply, _ := h.gateway("POST", "/v1/chat/completions", key, map[string]any{"model": "workspace-strict", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
		if status != 200 || !strings.Contains(fmt.Sprint(reply), "Hello from the reference upstream") {
			t.Fatalf("strict unary through the workspace profile: %d %v", status, reply)
		}
		if paths := upstream.receivedPaths(); paths[len(paths)-1] != "/v1/workspaces/"+workspace+"/chat/completions" {
			t.Fatalf("the upstream received %v", paths)
		}
	}
	serve("acme")

	// Moving the provider to another workspace is a new revision.
	detail := h.want(owner, "GET", path, nil, nil, 200)
	moved := detail["configuration"].(map[string]any)
	moved["options"].(map[string]any)["plugin_options"] = map[string]any{"workspace": "beta"}
	h.want(owner, "PATCH", path, map[string]any{"name": "Workspace", "configuration": moved}, etagHeader(detail), 200)
	certifyPluginProvider(t, h, owner, path)
	diff := h.want(owner, "GET", path+"/revisions/diff?from=1&to=2", nil, nil, 200)
	if diff["plugin_options_changed"] != true || diff["endpoint_changed"] != true || diff["plugin_changed"] != false || diff["profile_changed"] != false {
		t.Fatalf("revision diff %v", diff)
	}
	serve("beta")

	// Exports carry the options; importing another value places the address
	// anew in the provider's draft.
	document := h.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	providers := document["providers"].([]any)
	entry := providers[slices.IndexFunc(providers, func(p any) bool { return p.(map[string]any)["name"] == "Workspace" })].(map[string]any)
	options := entry["configuration"].(map[string]any)["options"].(map[string]any)
	if fmt.Sprint(options["plugin_options"]) != "map[workspace:beta]" {
		t.Fatalf("exported options %v", options)
	}
	options["plugin_options"] = map[string]any{"workspace": "gamma"}
	planned := h.want(owner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	if !slices.ContainsFunc(planned["actions"].([]any), func(action any) bool {
		item := action.(map[string]any)
		return item["kind"] == "provider" && item["key"] == "Workspace" && item["action"] == "replace"
	}) || len(planned["blockers"].([]any)) != 0 || len(planned["conflicts"].([]any)) != 0 {
		t.Fatalf("import plan %v", planned)
	}
	h.want(owner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString()), 200)
	imported := h.want(owner, "GET", path, nil, nil, 200)["configuration"].(map[string]any)
	if imported["endpoint"] != upstream.URL+"/v1/workspaces/gamma" || fmt.Sprint(imported["options"].(map[string]any)["plugin_options"]) != "map[workspace:gamma]" {
		t.Fatalf("imported %v", imported)
	}
}
