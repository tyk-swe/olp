//go:build integration

package integration_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/testutil"
)

// enrollSlot enrolls a grant for a provider's credential slot, re-enrolling
// the slot's grant if one backs it, signing in as whichever account the
// authority signs in next, and returns the completion.
func enrollSlot(t *testing.T, h *accessHarness, owner *browser, path, slot string) map[string]any {
	t.Helper()
	detail := h.want(owner, "GET", path, nil, nil, 200)
	enrollment := h.want(owner, "POST", path+"/grant-enrollments", map[string]any{"slot_id": slot}, etagHeader(detail), 201)
	if enrollment["slot_id"] != slot {
		t.Fatalf("enrollment %v backs another slot than %s", enrollment, slot)
	}
	return continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
}

// activateEnrolled validates the model access of newly enrolled slots and
// activates the provider draft, answering with the given status.
func activateEnrolled(t *testing.T, h *accessHarness, owner *browser, path string, status int, slots ...string) map[string]any {
	t.Helper()
	for _, slot := range slots {
		h.want(owner, "POST", path+"/credential-slots/"+slot+"/validate", nil, nil, 200)
	}
	detail := h.want(owner, "GET", path, nil, nil, 200)
	return h.want(owner, "POST", path+"/activate", nil, withMatch(detail, idem(uuid.NewString())), status)
}

// An operator re-enrolls a credential slot's grant. Re-enrolling the same
// upstream account activates as a credential rotation; enrolling another
// changes the provider's serving identity, whose principal the inspector
// reports observed, and activation refuses slots that observe different
// accounts until each serves the same one.
func TestReenrollingAGrantRotatesTheCredentialOrChangesTheServingPrincipal(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	authority := testutil.NewOAuthServer(t)
	digest := installReferencePlugin(t, h, owner, newGrantUpstream(t, authority), "0.1.0", "-X=main.authority="+authority.URL)
	path := grantProvider(t, h, owner, digest, nil)
	providerID := strings.TrimPrefix(path, "/api/v1/providers/")
	enrollment := startGrantEnrollment(t, h, owner, path)
	first := continueGrantEnrollment(h, owner, path, enrollment, signIn(t, enrollment).String(), 201)
	certifyPluginProvider(t, h, owner, path)
	slots := h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	primary := slots["items"].([]any)[0].(map[string]any)["id"].(string)

	detail := h.want(owner, "GET", path, nil, nil, 200)
	if refusal := h.want(owner, "POST", path+"/grant-enrollments", map[string]any{"slot_id": uuid.NewString()}, etagHeader(detail), 422); refusal["errors"].(map[string]any)["slot_id"] == nil {
		t.Fatalf("started grant enrollment for an unknown slot: %v", refusal)
	}

	// The same account: a new credential version, pending activation, that
	// activates as a rotation.
	rotated := enrollSlot(t, h, owner, path, primary)
	if rotated["principal"] != first["principal"] || rotated["credential_version"] != float64(2) {
		t.Fatalf("re-enrolled %v", rotated)
	}
	for _, credential := range h.want(owner, "GET", path+"/credentials", nil, nil, 200)["items"].([]any) {
		version := credential.(map[string]any)
		pending := version["id"] == rotated["credential_id"]
		if version["draft_selected"] != pending || version["active"] == pending || version["grant"].(map[string]any)["principal"] != first["principal"] {
			t.Fatalf("credential version %v", version)
		}
	}
	activateEnrolled(t, h, owner, path, 200, primary)
	diff := h.want(owner, "GET", path+"/revisions/diff?from=1&to=2", nil, nil, 200)
	if diff["credential_changed"] != true || diff["serving_binding_changed"] != false {
		t.Fatalf("re-enrolling the same account: %v", diff)
	}

	// Another account changes the serving identity.
	authority.SignInAs(testutil.OAuthIdentity{Subject: "colleague@reference.example", Account: "acct-colleague"})
	switched := enrollSlot(t, h, owner, path, primary)
	if switched["principal"] != "colleague@reference.example" {
		t.Fatalf("re-enrolled %v", switched)
	}
	activateEnrolled(t, h, owner, path, 200, primary)
	diff = h.want(owner, "GET", path+"/revisions/diff?from=2&to=3", nil, nil, 200)
	if diff["credential_changed"] != true || diff["serving_binding_changed"] != true {
		t.Fatalf("enrolling another account: %v", diff)
	}
	h.refresh()
	if served := h.Runtime.Release().Snapshot.Providers[providerID]; served.ObservedPrincipal != "colleague@reference.example" {
		t.Fatalf("the serving provider observes %q", served.ObservedPrincipal)
	}
	draft := fidelityDraft("reference-colleague", providerID)
	draft["fidelity"] = map[string]any{"mode": "strict"}
	route := h.want(owner, "POST", "/api/v1/route-drafts", draft, idem(uuid.NewString()), 201)
	h.want(owner, "POST", "/api/v1/route-drafts/"+route["id"].(string)+"/activate", nil, withMatch(route, idem(uuid.NewString())), 200)
	request := map[string]any{"model": "reference-colleague", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	decision := h.list(owner, "POST", "/api/v1/routing/simulate", map[string]any{"operation": map[string]any{"operation": "generation", "request": request}, "surface": "openai", "mode": "unary", "dialect": "openai-chat", "seed": "principal"}, nil, 200)[0].(map[string]any)
	if serving := decision["interaction"].(map[string]any)["serving"].(map[string]any); serving["principal_observed"] != true || serving["principal_declared"] != false {
		t.Fatalf("inspected serving identity %v", serving)
	}

	// A slot bound to the first account's version mixes principals, which
	// activation refuses, naming each slot's, until that slot re-enrolls.
	backup := uuid.NewString()
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	h.want(owner, "PUT", path+"/credential-slots/"+backup, map[string]any{"slot": map[string]any{"name": "Backup", "credential_version_id": rotated["credential_id"]}}, withMatch(slots, idem(uuid.NewString())), 200)
	refusal := activateEnrolled(t, h, owner, path, 422, backup)
	if problemCode(t, refusal) != "principal_mismatch" || !strings.Contains(refusal["detail"].(string), "slot default observes colleague@reference.example; slot Backup observes operator@reference.example") {
		t.Fatalf("activated a revision that mixes principals: %v", refusal)
	}
	if pooled := enrollSlot(t, h, owner, path, backup); pooled["principal"] != "colleague@reference.example" {
		t.Fatalf("re-enrolled %v", pooled)
	}

	// A slot with no credential version yet, such as an imported one, enrolls
	// its first grant the same way.
	standby := uuid.NewString()
	slots = h.want(owner, "GET", path+"/credential-slots", nil, nil, 200)
	h.want(owner, "PUT", path+"/credential-slots/"+standby, map[string]any{"slot": map[string]any{"name": "Standby"}}, withMatch(slots, idem(uuid.NewString())), 200)
	if enrolled := enrollSlot(t, h, owner, path, standby); enrolled["principal"] != "colleague@reference.example" {
		t.Fatalf("enrolled %v", enrolled)
	}
	activateEnrolled(t, h, owner, path, 200, backup, standby)
}
