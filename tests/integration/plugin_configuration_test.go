//go:build integration

package integration_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// Configuration exports reference the plugin build each plugin provider pins
// and never carry grant material. Importing them waits for that build to be
// installed and approved, binds a static plugin credential like any secret,
// and leaves each credential slot a grant backs for a new grant enrollment,
// which activation then requires.
func TestConfigurationPromotionOfPluginProviders(t *testing.T) {
	source := newAccessHarness(t)
	owner := source.owner()
	authority := testutil.NewOAuthServer(t)
	upstream := newGrantUpstream(t, authority)
	module := testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.upstream="+upstream.URL+"/v1", "-X=main.version=0.1.0", "-X=main.authority="+authority.URL)
	digest := installPlugin(t, source, owner, module)

	// A draft with a static plugin credential, and a published provider
	// whose grant was enrolled.
	source.want(owner, "POST", "/api/v1/providers", map[string]any{"name": "Reference key", "credential": pluginCredential, "model": vendorModel,
		"configuration": map[string]any{"kind": "plugin", "auth_mode": "static_credential", "profile_id": "reference-chat", "profile_revision": digest}}, idem(uuid.NewString()), 201)
	account := grantProvider(t, source, owner, digest, nil)
	enrollment := startGrantEnrollment(t, source, owner, account)
	completed := continueGrantEnrollment(source, owner, account, enrollment, signIn(t, enrollment).String(), 201)
	certifyPluginProvider(t, source, owner, account)
	accountName := source.want(owner, "GET", account, nil, nil, 200)["name"].(string)
	grantRef, staticRef := accountName+"/default", "Reference key/default"

	document := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)["document"].(map[string]any)
	encoded, _ := json.Marshal(document)
	for _, material := range append(authority.Issued(), pluginCredential, completed["credential_id"].(string), completed["principal"].(string), "acct-reference") {
		if strings.Contains(string(encoded), material) {
			t.Fatalf("the export carries %q: %s", material, encoded)
		}
	}
	for _, entry := range document["providers"].([]any) {
		provider := entry.(map[string]any)
		configuration := provider["configuration"].(map[string]any)
		ref := provider["slots"].([]any)[0].(map[string]any)["credential_ref"]
		if configuration["profile_revision"] != digest || ref != provider["name"].(string)+"/default" {
			t.Fatalf("the export of %s references %v and %v", provider["name"], configuration["profile_revision"], ref)
		}
	}

	destination := newAccessHarness(t)
	operator := destination.owner()
	plan := func(bindings map[string]any) map[string]any {
		t.Helper()
		body := map[string]any{"document": document}
		if bindings != nil {
			body["secret_bindings"] = bindings
		}
		return destination.want(operator, "POST", "/api/v1/configuration/plan", body, nil, 200)
	}
	blocked := func(planned map[string]any, detail string) bool {
		return slices.ContainsFunc(planned["blockers"].([]any), func(item any) bool {
			blocker := item.(map[string]any)
			return blocker["kind"] == "plugin" && blocker["key"] == digest && blocker["detail"] == detail
		})
	}
	if planned := plan(nil); !blocked(planned, "plugin_not_installed") {
		t.Fatalf("the plan does not wait for the plugin build: %v", planned)
	}
	if status, out, _ := destination.request(operator, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem(uuid.NewString())); status != 409 || problemCode(t, out) != "configuration_not_applicable" {
		t.Fatalf("applied an artifact whose plugin is not installed: %d %v", status, out)
	}
	installed := destination.want(operator, "POST", "/api/v1/plugins", module, wasm, 201)
	if planned := plan(nil); !blocked(planned, "plugin_not_approved") {
		t.Fatalf("the plan does not wait for approval: %v", planned)
	}
	destination.want(operator, "POST", "/api/v1/plugins/"+digest+"/approve", map[string]any{"origins": installed["manifest"].(map[string]any)["origins"]}, etagHeader(installed), 200)

	// The static credential binds like any other secret; a grant can't.
	planned := plan(nil)
	if blockers := planned["blockers"].([]any); len(blockers) != 1 || blockers[0].(map[string]any)["key"] != staticRef || blockers[0].(map[string]any)["detail"] != "secret_binding_required" {
		t.Fatalf("plan blockers %v", blockers)
	}
	enroll := func(planned map[string]any) bool {
		return slices.ContainsFunc(planned["actions"].([]any), func(item any) bool {
			action := item.(map[string]any)
			return action["key"] == grantRef && action["action"] == "enroll" && action["detail"] == "grant_enrollment_required"
		})
	}
	if !enroll(planned) {
		t.Fatalf("the plan does not leave the grant slot for grant enrollment: %v", planned)
	}
	requireFieldError(t, destination.want(operator, "POST", "/api/v1/configuration/plan", map[string]any{"document": document, "secret_bindings": map[string]any{grantRef: "pasted"}}, nil, 422), "secret_bindings."+grantRef)
	bindings := map[string]any{staticRef: pluginCredential}
	applied := destination.want(operator, "POST", "/api/v1/configuration/apply", map[string]any{"document": document, "secret_bindings": bindings}, idem(uuid.NewString()), 200)
	if !enroll(applied) || len(applied["blockers"].([]any)) != 0 {
		t.Fatalf("apply %v", applied)
	}

	paths := map[string]string{}
	for _, item := range destination.want(operator, "GET", "/api/v1/providers", nil, nil, 200)["items"].([]any) {
		provider := item.(map[string]any)
		paths[provider["name"].(string)] = "/api/v1/providers/" + provider["id"].(string)
	}
	if versions := destination.want(operator, "GET", paths["Reference key"]+"/credentials", nil, nil, 200)["items"].([]any); len(versions) != 1 || versions[0].(map[string]any)["draft_selected"] != true {
		t.Fatalf("the static plugin credential did not bind: %v", versions)
	}
	imported := paths[accountName]
	if versions := destination.want(operator, "GET", imported+"/credentials", nil, nil, 200)["items"].([]any); len(versions) != 0 {
		t.Fatalf("the grant provider imported a credential: %v", versions)
	}
	detail := destination.want(operator, "GET", imported, nil, nil, 200)
	if refusal := destination.want(operator, "POST", imported+"/activate", nil, withMatch(detail, idem(uuid.NewString())), 422); problemCode(t, refusal) != "credential_required" {
		t.Fatalf("activated an imported grant provider without a grant: %v", refusal)
	}

	// Once enrolled, the grant is reused and the imported provider serves.
	enrollment = startGrantEnrollment(t, destination, operator, imported)
	continueGrantEnrollment(destination, operator, imported, enrollment, signIn(t, enrollment).String(), 201)
	planned = plan(bindings)
	for _, item := range planned["actions"].([]any) {
		action := item.(map[string]any)
		if action["kind"] == "provider" && action["action"] != "noop" || action["key"] == grantRef && action["action"] != "reuse" {
			t.Fatalf("re-planning the applied artifact changes %v: %v", action, planned)
		}
	}
	certifyPluginProvider(t, destination, operator, imported)
}
