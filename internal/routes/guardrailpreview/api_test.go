package guardrailpreview

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/cache"
	"github.com/tyk-swe/olp/internal/domain/guardrailpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type fixture struct {
	release, route, provider, target, key, project, sibling, organization string
}

func newFixture() fixture {
	return fixture{uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()}
}

func (f fixture) api(t *testing.T, visible bool, hidden int) api {
	t.Helper()
	registry := &guardrails.Registry{
		Definitions: map[string]guardrails.Revision{}, Policies: map[string]guardrails.PolicyRevision{},
		ProjectOrganizations: map[string]string{f.project: f.organization, f.sibling: f.organization},
	}
	attach := func(id, project string, scope guardrailpolicy.Scope, scopeID string, phase guardrailpolicy.Phase, order int, monitor bool) {
		definition := guardrails.Revision{ID: id + "-check-r1", GuardrailID: id + "-check", ProjectID: project,
			Type: "builtin.keywords", Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: guardrailpolicy.FailClosed}
		registry.Definitions[definition.ID] = definition
		policy := guardrails.PolicyRevision{ID: id + "-policy-r1", PolicyID: id, ProjectID: project,
			Entries: []guardrails.Entry{{GuardrailID: definition.GuardrailID, RevisionID: definition.ID, Phase: phase, Action: guardrailpolicy.Block, Monitor: monitor}}}
		registry.Policies[policy.ID] = policy
		registry.Attachments = append(registry.Attachments, guardrails.Attachment{ID: id, Scope: scope, ScopeID: scopeID, PolicyRevisionID: policy.ID, Order: order})
	}
	if visible {
		attach("key-check", f.project, guardrailpolicy.KeyScope, f.key, guardrailpolicy.Input, 6, true)
		attach("route-check", f.project, guardrailpolicy.RouteScope, f.route, guardrailpolicy.Output, 11, false)
		attach("project-check", f.project, guardrailpolicy.ProjectScope, f.project, guardrailpolicy.Input, 5, false)
	}
	if hidden > 0 {
		attach("hidden-installation", f.project, guardrailpolicy.InstallationScope, guardrails.InstallationProject, guardrailpolicy.Output, 2, false)
	}
	if hidden > 1 {
		attach("hidden-organization", f.sibling, guardrailpolicy.OrganizationScope, f.organization, guardrailpolicy.Output, 0, false)
		attach("hidden-sibling", f.sibling, guardrailpolicy.InstallationScope, guardrails.InstallationProject, guardrailpolicy.Input, 0, true)
	}
	model := "wire-model"
	snapshot := &routing.Snapshot{
		Generation: routing.Generation{ID: f.release},
		Guardrails: registry,
		Providers: map[string]routing.Provider{f.provider: {ID: f.provider, RevisionID: uuid.NewString(), Name: "Preview provider", Kind: "openai",
			Enabled: true, VendorID: "openai", Capabilities: []routing.Capability{{Model: model, Operation: "generation", Surface: "openai", Mode: "unary"}}}},
		Routes: map[string]routing.Route{"preview": {ID: f.route, RoutingID: f.route, Slug: "preview", ProjectID: &f.project,
			OverallTimeout: 3000, MaxAttempts: 1, Operations: []string{"generation"}, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
			Targets: []routing.Target{{ID: f.target, RoutingID: f.target, ProviderID: f.provider, ProviderModel: model, Weight: 1, Timeout: 1000}}}},
	}
	installed, err := release.NewRelease(f.release, 41, snapshot, nil)
	if err != nil {
		t.Fatal(err)
	}
	return api{serving: func() *release.Release { return installed }, query: func(_ context.Context, _ string, args ...any) pgx.Row {
		switch {
		case args[0] != f.route && args[0] != f.sibling:
			return row(func(...any) error { return pgx.ErrNoRows })
		case args[1] != f.key && args[1] != f.sibling:
			return row(func(...any) error { return pgx.ErrNoRows })
		default:
			return row(func(dest ...any) error {
				*dest[0].(*string) = "preview"
				*dest[1].(**string) = &f.project
				*dest[2].(**string) = &f.project
				if args[0] == f.sibling {
					*dest[1].(**string) = &f.sibling
				}
				if args[1] == f.sibling {
					*dest[2].(**string) = &f.sibling
				}
				*dest[3].(*int64) = 42
				return nil
			})
		}
	}}
}

type row func(...any) error

func (r row) Scan(dest ...any) error { return r(dest...) }

func (f fixture) request() url.Values {
	return url.Values{"route_id": {f.route}, "api_key_id": {f.key}, "operation": {"generation"}, "surface": {"openai-chat"}, "mode": {"streaming"}}
}

func request(t *testing.T, api api, principal authz.Principal, query url.Values) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/guardrails/preview?"+query.Encode(), nil)
	reply, err := api.handle(r, principal)
	if err != nil {
		w := httptest.NewRecorder()
		httpkit.WriteProblem(w, err)
		return w.Code, w.Body.String()
	}
	b, err := json.Marshal(reply.Body)
	if err != nil {
		t.Fatal(err)
	}
	return reply.Status, string(b)
}

func TestProjectPreviewIsIdenticalWithZeroOneAndSeveralHiddenPolicies(t *testing.T) {
	for _, visible := range []bool{true, false} {
		t.Run("visible="+strconv.FormatBool(visible), func(t *testing.T) {
			f := newFixture()
			viewer := authz.Principal{User: authz.User{Role: authz.RoleViewer}, Projects: map[string]string{f.project: "viewer"},
				Organizations: map[string]string{f.organization: "viewer"}}
			global := authz.Principal{User: authz.User{Role: authz.RoleViewer}, AllProjects: true}
			wantStatus, wantBody := request(t, f.api(t, visible, 0), viewer, f.request())
			for _, hidden := range []int{0, 1, 3} {
				a := f.api(t, visible, hidden)
				status, body := request(t, a, viewer, f.request())
				if status != http.StatusOK || status != wantStatus || body != wantBody {
					t.Fatalf("scoped preview differs with %d hidden policies: %d %s vs %d %s", hidden, status, body, wantStatus, wantBody)
				}
				result := decodePreview(t, body)
				if !result.PartialVisibility || result.StaticCompatible != nil || !result.Stale ||
					result.ReleaseID != f.release || result.ReleaseSequence != 41 {
					t.Fatalf("wrong scoped metadata: %s", body)
				}
				if visible {
					assertPreviewOrder(t, result, "project-check", "route-check", "key-check")
					if !slices.Equal(result.Entries[1].Notes, []guardrails.CompatibilityNote{guardrails.StreamingUnavailable}) ||
						len(result.Entries[0].Notes) != 0 || result.Entries[2].Mode != "monitor" || len(result.Entries[2].Notes) != 0 {
						t.Fatalf("wrong visible compatibility: %s", body)
					}
				} else if result.Entries == nil || len(result.Entries) != 0 {
					t.Fatalf("expected an empty entry array: %s", body)
				}
				status, body = request(t, a, global, f.request())
				result = decodePreview(t, body)
				if status != http.StatusOK || result.PartialVisibility || result.StaticCompatible == nil ||
					*result.StaticCompatible != (!visible && hidden == 0) {
					t.Fatalf("wrong installation-wide preview: %d %s", status, body)
				}
				want := []string{}
				if hidden > 1 {
					want = append(want, "hidden-sibling")
				}
				if hidden > 0 {
					want = append(want, "hidden-installation")
				}
				if hidden > 1 {
					want = append(want, "hidden-organization")
				}
				if visible {
					want = append(want, "project-check", "route-check", "key-check")
				}
				assertPreviewOrder(t, result, want...)
			}
		})
	}
}

func TestPreviewRefusesInvisibleRouteOrKeyAndSampleContent(t *testing.T) {
	f := newFixture()
	a := f.api(t, true, 3)
	p := authz.Principal{User: authz.User{Role: authz.RoleViewer}, Projects: map[string]string{f.project: "viewer"}}
	for _, field := range []string{"route_id", "api_key_id"} {
		q := f.request()
		q.Set(field, uuid.NewString())
		wantStatus, wantBody := request(t, a, p, q)
		q.Set(field, f.sibling)
		status, body := request(t, a, p, q)
		if status != http.StatusNotFound || status != wantStatus || body != wantBody {
			t.Fatalf("invisible %s differs from missing: %d %s", field, status, body)
		}
	}
	q := f.request()
	q.Set("sample", "secret content")
	if status, body := request(t, a, p, q); status != http.StatusUnprocessableEntity || strings.Contains(body, "secret content") {
		t.Fatalf("unexpected sample response: %d %s", status, body)
	}
}

func decodePreview(t *testing.T, body string) response {
	t.Helper()
	var result response
	dec := json.NewDecoder(strings.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&result); err != nil {
		t.Fatalf("invalid preview: %s: %v", body, err)
	}
	return result
}

func assertPreviewOrder(t *testing.T, result response, want ...string) {
	t.Helper()
	got := make([]string, 0, len(result.Entries))
	for _, entry := range result.Entries {
		got = append(got, entry.PolicyID)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("preview order = %v, want %v", got, want)
	}
}

func TestPreviewExplainsStaticSurfaceLimits(t *testing.T) {
	d := guardrails.Revision{ID: "revision", GuardrailID: "check", ProjectID: "project", Type: "builtin.keywords",
		Configuration: json.RawMessage(`{"words":["private"]}`), TimeoutMS: 100, OnError: guardrailpolicy.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{
		{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block},
		{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrailpolicy.OutputStream, Action: guardrailpolicy.Redact},
	}, map[string]guardrails.Revision{d.ID: d})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		operation, surface, mode string
		input, output            guardrails.CompatibilityNote
	}{
		{"generation", "openai-chat", "streaming", "", ""},
		{"generation", "cohere-chat-v2", "unary", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"generation", "mistral-fim", "unary", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"generation", "bedrock-converse", "streaming", "", guardrails.RewriteUnavailable},
		{"generation", "gemini-interactions", "unary", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"generation", "gemini-interactions", "streaming", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"image_generation", "openai", "streaming", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"image_edit", "openai", "streaming", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"speech", "openai", "streaming", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
		{"transcription", "openai", "streaming", guardrails.SurfaceUninspected, guardrails.SurfaceUninspected},
	} {
		t.Run(tt.operation+"/"+tt.surface+"/"+tt.mode, func(t *testing.T) {
			route := routing.Route{
				Operations: []string{tt.operation}, Fidelity: routing.RouteFidelity{Mode: routing.FidelityTransformed},
				Behavior: routing.Behavior{Cache: &cache.Policy{}, ToolExecution: "gateway"},
			}
			profile, err := compatibility(route, selection{operation: tt.operation, surface: tt.surface, mode: tt.mode}, 1024)
			if err != nil {
				t.Fatal(err)
			}
			entries := engine.Preview(profile, func(guardrails.Placement) bool { return true })
			for i, want := range []guardrails.CompatibilityNote{tt.input, tt.output} {
				if want == "" && len(entries[i].Notes) == 0 {
					continue
				}
				if !slices.Equal(entries[i].Notes, []guardrails.CompatibilityNote{want}) {
					t.Errorf("%s notes = %v, want %v", entries[i].Phase, entries[i].Notes, want)
				}
			}
		})
	}
}

func TestNativeTypeSafePreviewKeepsStreamingConflicts(t *testing.T) {
	d := guardrails.Revision{ID: "revision", GuardrailID: "check", Type: "typesafe",
		Configuration: json.RawMessage(`{"route":"decision-route"}`), TimeoutMS: 100, OnError: guardrailpolicy.FailClosed}
	engine, err := guardrails.Compile([]guardrails.Entry{
		{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrailpolicy.Input, Action: guardrailpolicy.Block},
		{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrailpolicy.Output, Action: guardrailpolicy.Block},
		{GuardrailID: d.GuardrailID, RevisionID: d.ID, Phase: guardrailpolicy.OutputStream, Action: guardrailpolicy.Block},
	}, map[string]guardrails.Revision{d.ID: d})
	if err != nil {
		t.Fatal(err)
	}
	route := routing.Route{Operations: []string{"generation"}, Behavior: routing.Behavior{ToolExecution: "gateway"}}
	const window = 1024
	if err := engine.ValidateStream(window); err == nil {
		t.Fatal("serving unexpectedly accepted the stream")
	}
	for _, surface := range []string{"cohere-chat-v2", "mistral-fim"} {
		t.Run(surface, func(t *testing.T) {
			profile, err := compatibility(route, selection{operation: "generation", surface: surface, mode: "streaming"}, window)
			if err != nil {
				t.Fatal(err)
			}
			entries := engine.Preview(profile, func(guardrails.Placement) bool { return true })
			if len(entries) != 3 || len(entries[0].Notes) != 0 ||
				!slices.Equal(entries[1].Notes, []guardrails.CompatibilityNote{guardrails.StreamingUnavailable}) ||
				!slices.Equal(entries[2].Notes, []guardrails.CompatibilityNote{guardrails.StreamingUnavailable}) {
				t.Fatalf("expected an input exception and both output conflicts, got %+v", entries)
			}
		})
	}
}
