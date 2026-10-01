package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

type stubRows struct {
	values [][]any
	index  int
}

func (r *stubRows) Close()                                       {}
func (r *stubRows) Err() error                                   { return nil }
func (r *stubRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *stubRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *stubRows) Next() bool {
	r.index++
	return r.index <= len(r.values)
}
func (r *stubRows) Scan(dest ...any) error {
	for i, d := range dest {
		assign(d, r.values[r.index-1][i])
	}
	return nil
}
func (r *stubRows) Values() ([]any, error) { return nil, nil }
func (r *stubRows) RawValues() [][]byte    { return nil }
func (r *stubRows) Conn() *pgx.Conn        { return nil }
func (r *stubRows) TypeMap() *pgtype.Map   { return nil }

func assign(dest, src any) {
	dv := reflect.ValueOf(dest).Elem()
	if src == nil {
		dv.SetZero()
		return
	}
	sv := reflect.ValueOf(src)
	if dv.Kind() == reflect.Pointer {
		p := reflect.New(dv.Type().Elem())
		assign(p.Interface(), src)
		dv.Set(p)
		return
	}
	if sv.Type() != dv.Type() {
		sv = sv.Convert(dv.Type())
	}
	dv.Set(sv)
}

type singleRow struct {
	values []any
	err    error
}

func (r singleRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		assign(d, r.values[i])
	}
	return nil
}

type queryStub struct {
	match string
	rows  [][]any
	row   []any
	err   error
}

type mapQueryer struct {
	t    *testing.T
	stub []queryStub
}

func (q mapQueryer) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	for _, s := range q.stub {
		if strings.Contains(sql, s.match) {
			return singleRow{values: s.row, err: s.err}
		}
	}
	return singleRow{err: pgx.ErrNoRows}
}
func (q mapQueryer) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	for _, s := range q.stub {
		if strings.Contains(sql, s.match) {
			return &stubRows{values: s.rows}, nil
		}
	}
	return &stubRows{}, nil
}

func testServer() *Server {
	return &Server{Egress: &egress.Policy{}}
}

func testDocument() *Document {
	slotName := "primary"
	ref := CredentialRef("acme", slotName)
	return &Document{
		APIVersion: APIVersion,
		Projects:   []ProjectEntry{{Name: "Edge"}},
		Providers: []ProviderEntry{{
			Name:    "acme",
			Project: ptr("Edge"),
			Configuration: providers.Configuration{
				Kind:     "openai_compatible",
				AuthMode: "api_key",
				Endpoint: ptr("https://vendor.example.com/v1/"),
			},
			Models: []ModelEntry{{
				UpstreamModel: "gpt-x",
				DisplayName:   "gpt-x",
				Enabled:       true,
				Capabilities: []CapabilityEntry{
					{Operation: "generation", Surface: "openai", Mode: "unary"},
					{Operation: "generation", Surface: "openai", Mode: "streaming"},
				},
			}},
			Slots: []SlotEntry{{
				Name: slotName, IsDefault: true, Position: 0, Enabled: true, Priority: 0, Weight: 1,
				Restrictions:  Restrictions{},
				Limits:        providers.Limits{},
				CredentialRef: &ref,
			}},
		}},
		Routes: []RouteEntry{{
			Slug:             "main",
			Project:          ptr("Edge"),
			Operations:       []string{"generation"},
			OverallTimeoutMS: 30000,
			MaxAttempts:      2,
			Targets:          []TargetEntry{{Provider: "acme", ProviderModel: "gpt-x", Priority: 0, Weight: 1, TimeoutMS: 30000}},
			RoutingPolicy:    &runtime.Policy{AllowedStrategies: []string{"weighted"}},
		}},
		Pricing: &PricingEntry{
			EffectiveAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			Prices:      []PriceEntry{{ProviderKind: "openai_compatible", Model: "gpt-x", Operation: "generation", InputPerMillion: ptr("1.5"), Currency: "USD"}},
		},
	}
}

func ptr[T any](v T) *T { return &v }

func TestDigestDeterministicAndExcludesExportedAt(t *testing.T) {
	doc := testDocument()
	first, err := Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.ExportedAt = "2026-01-01T00:00:00Z"
	second, err := Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("digest changed with exported_at: %s != %s", first, second)
	}
	if len(first) != 64 || first != strings.ToLower(first) {
		t.Fatalf("unexpected digest %q", first)
	}
	reversed := testDocument()
	// The fixtures must differ only in ordering, not in a pricing activation
	// time that can cross a wall-clock second between construction calls.
	reversed.Pricing.EffectiveAt = doc.Pricing.EffectiveAt
	reversed.Providers[0].Models[0].Capabilities = slices.Clone(reversed.Providers[0].Models[0].Capabilities)
	slices.Reverse(reversed.Providers[0].Models[0].Capabilities)
	third, err := Digest(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if third != first {
		t.Fatalf("canonical ordering changed digest: %s != %s", third, first)
	}
	activation, err := time.Parse(time.RFC3339, reversed.Pricing.EffectiveAt)
	if err != nil {
		t.Fatal(err)
	}
	reversed.Pricing.EffectiveAt = activation.Add(time.Second).Format(time.RFC3339)
	changed, err := Digest(reversed)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("pricing activation time must remain part of the digest")
	}
}

func TestValidateDocumentRejectsDuplicatesAndReferences(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
	}{
		{"api_version", func(d *Document) { d.APIVersion = "v2" }},
		{"duplicate project", func(d *Document) { d.Projects = append(d.Projects, ProjectEntry{Name: "edge"}) }},
		{"duplicate provider", func(d *Document) {
			dup := d.Providers[0]
			dup.Name = "ACME"
			d.Providers = append(d.Providers, dup)
		}},
		{"target provider missing", func(d *Document) { d.Routes[0].Targets[0].Provider = "other" }},
		{"target model missing", func(d *Document) { d.Routes[0].Targets[0].ProviderModel = "absent" }},
		{"cross-project target", func(d *Document) { d.Routes[0].Project = nil }},
		{"credential ref mismatch", func(d *Document) { d.Providers[0].Slots[0].CredentialRef = ptr("other/ref") }},
		{"credentialless auth with ref", func(d *Document) { d.Providers[0].Configuration.AuthMode = "none" }},
		{"no default slot", func(d *Document) { d.Providers[0].Slots[0].IsDefault = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := testDocument()
			tc.mutate(doc)
			if _, err := testServer().validateDocument(t.Context(), nil, doc); err == nil {
				t.Fatalf("expected validation failure for %s", tc.name)
			}
		})
	}
	doc := testDocument()
	if _, err := testServer().validateDocument(t.Context(), nil, doc); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
}

func TestRouteFidelityOmissionIsStrictInDocuments(t *testing.T) {
	omitted := testDocument()
	first, err := Digest(omitted)
	if err != nil {
		t.Fatal(err)
	}
	if string(omitted.Routes[0].Fidelity) != `{"mode":"strict"}` {
		t.Fatalf("canonical route did not state strict fidelity: %s", omitted.Routes[0].Fidelity)
	}
	for _, raw := range []string{`{"mode":"strict"}`, `{}`, `null`} {
		explicit := testDocument()
		explicit.Pricing.EffectiveAt = omitted.Pricing.EffectiveAt
		explicit.Routes[0].Fidelity = json.RawMessage(raw)
		digest, err := Digest(explicit)
		if err != nil || digest != first {
			t.Fatalf("%s differs from an omitted fidelity: %v", raw, err)
		}
	}
	transformed := testDocument()
	transformed.Pricing.EffectiveAt = omitted.Pricing.EffectiveAt
	transformed.Routes[0].Fidelity = json.RawMessage(`{"mode":"transformed"}`)
	if digest, _ := Digest(transformed); digest == first {
		t.Fatal("transformed fidelity did not change the digest")
	}
	legacy := testDocument()
	legacy.Routes[0].Fidelity = json.RawMessage(`{"mode":"legacy"}`)
	var problem *access.Problem
	if _, err := testServer().validateDocument(t.Context(), nil, legacy); !errors.As(err, &problem) || problem.Field != "routes.0.fidelity" {
		t.Fatalf("legacy fidelity was not a typed field error: %v", err)
	}
}

func TestValidateBindingsRejectsForeignRef(t *testing.T) {
	doc := testDocument()
	if err := validateBindings(doc, map[string]string{"acme/other": "secret"}); err == nil {
		t.Fatal("binding outside document refs accepted")
	}
	if err := validateBindings(doc, map[string]string{"acme/primary": "secret"}); err != nil {
		t.Fatalf("binding rejected: %v", err)
	}
}

func TestPlanCreatesAndRequiresBindings(t *testing.T) {
	s := testServer()
	doc := testDocument()
	result, err := s.plan(context.Background(), mapQueryer{t: t}, doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", result.Conflicts)
	}
	var blocker *planItem
	for i := range result.Blockers {
		if result.Blockers[i].Detail == "secret_binding_required" {
			blocker = &result.Blockers[i]
		}
	}
	if blocker == nil || blocker.Key != "acme/primary" {
		t.Fatalf("expected secret_binding_required for acme/primary: %+v", result.Blockers)
	}
	result, err = s.plan(context.Background(), mapQueryer{t: t}, doc, map[string]string{"acme/primary": "vendor-secret"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Blockers) != 0 || len(result.Conflicts) != 0 {
		t.Fatalf("supplied binding still blocked: %+v %+v", result.Blockers, result.Conflicts)
	}
}

func TestPlanProviderKindConflict(t *testing.T) {
	s := testServer()
	doc := testDocument()
	stubs := []queryStub{
		{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "openai", "active", nil, nil}}},
	}
	result, err := s.plan(context.Background(), mapQueryer{t: t, stub: stubs}, doc, map[string]string{"acme/primary": "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range result.Conflicts {
		found = found || c.Detail == "provider_kind_changed"
	}
	if !found {
		t.Fatalf("expected provider_kind_changed conflict: %+v", result.Conflicts)
	}
}
func TestPlanProviderProjectMismatch(t *testing.T) {
	s := testServer()
	doc := testDocument()
	stubs := []queryStub{
		{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "openai_compatible", "active", "other-id", nil}}},
		{match: "FROM olp.projects", rows: [][]any{{"other-id", "Core"}, {"edge-id", "Edge"}}},
	}
	result, err := s.plan(context.Background(), mapQueryer{t: t, stub: stubs}, doc, map[string]string{"acme/primary": "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range result.Conflicts {
		found = found || c.Detail == "provider_project_mismatch"
	}
	if !found {
		t.Fatalf("expected provider_project_mismatch conflict: %+v", result.Conflicts)
	}
}

func TestValidateRejectsNonPortableAPIKeys(t *testing.T) {
	s := testServer()
	doc := testDocument()
	doc.Providers[0].Slots[0].Restrictions.AllowedAPIKeys = []string{"key-id"}
	_, err := s.validateDocument(t.Context(), nil, doc)
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != 422 || problem.Code != "non_portable_reference" {
		t.Fatalf("expected 422 non_portable_reference, got %v", err)
	}
}

func TestDigestTrimsNaturalIdentities(t *testing.T) {
	doc := testDocument()
	padded := testDocument()
	padded.Projects[0].Name = "  Edge  "
	padded.Providers[0].Name = " acme "
	*padded.Providers[0].Project = " Edge "
	padded.Providers[0].Models[0].UpstreamModel = " gpt-x "
	padded.Providers[0].Models[0].DisplayName = " gpt-x "
	padded.Providers[0].Slots[0].Name = " primary "
	*padded.Providers[0].Slots[0].CredentialRef = " acme/primary "
	padded.Routes[0].Slug = " main "
	padded.Routes[0].Targets[0].Provider = " acme "
	padded.Routes[0].Targets[0].ProviderModel = " gpt-x "
	padded.Pricing.Prices[0].Model = " gpt-x "
	first, err := Digest(doc)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Digest(padded)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("digest must ignore surrounding whitespace in natural identities")
	}
}

func TestValidateDuplicateWhitespaceAlias(t *testing.T) {
	s := testServer()
	doc := testDocument()
	alias := doc.Providers[0]
	alias.Name = " ACME "
	doc.Providers = append(doc.Providers, alias)
	_, err := s.validateDocument(t.Context(), nil, doc)
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != 422 {
		t.Fatalf("expected duplicate identity rejection, got %v", err)
	}
	doc = testDocument()
	doc.Projects = append(doc.Projects, ProjectEntry{Name: " EDGE "})
	if _, err = s.validateDocument(t.Context(), nil, doc); err == nil {
		t.Fatal("expected duplicate project identity rejection")
	}
}

func TestPlanPricingRebasedDetail(t *testing.T) {
	s := testServer()
	doc := testDocument()
	doc.Pricing.EffectiveAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	result, err := s.plan(context.Background(), mapQueryer{t: t}, doc, map[string]string{"acme/primary": "s"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range result.Blockers {
		if b.Detail == "pricing_effective_at_past" {
			t.Fatalf("past effective time must not block: %+v", result.Blockers)
		}
	}
	found := false
	for _, a := range result.Actions {
		found = found || a.Kind == "pricing" && a.Detail == "effective_at_rebased"
	}
	if !found {
		t.Fatalf("expected effective_at_rebased pricing action: %+v", result.Actions)
	}
}

func TestPlanNoopProviderAndRoute(t *testing.T) {
	s := testServer()
	doc := testDocument()
	configuration, _ := json.Marshal(doc.Providers[0].Configuration)
	capabilities, _ := json.Marshal([]map[string]any{
		{"operation": "generation", "surface": "openai", "mode": "unary", "source": "declared"},
		{"operation": "generation", "surface": "openai", "mode": "streaming", "source": "declared"},
	})
	targets, _ := json.Marshal([]map[string]any{
		{"provider_id": "provider-id", "provider_name": "acme", "provider_model": "gpt-x", "provider_model_id": "model-id", "priority": 0, "weight": 1, "timeout_ms": 30000},
	})
	stubs := []queryStub{
		{match: "FROM olp.providers WHERE", row: []any{configuration}},
		{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "openai_compatible", "draft", "edge-id", nil}}},
		{match: "provider_slots s LEFT JOIN", rows: [][]any{{"provider-id", "primary", "slot-id", "cred-id", ""}}},
		{match: "provider_slots WHERE", rows: [][]any{{"primary", true, 0, true, 0, 1, "cred-id", []byte(`{"allowed_api_keys":[],"allowed_models":[],"allowed_routes":[]}`), []byte(`{}`)}}},
		{match: "FROM olp.provider_models", rows: [][]any{{"gpt-x", "gpt-x", true, capabilities}}},
		{match: "route_drafts WHERE id", row: []any{[]byte(`["generation"]`), 30000, 2, targets, nil, []byte(`{"mode":"strict"}`)}},
		{match: "routing_policies WHERE", row: []any{[]byte(`{"allowed_strategies":["weighted"]}`)}},
		{match: "FROM olp.route_drafts", rows: [][]any{{"draft-id", "main", "edge-id"}}},
		{match: "FROM olp.projects", rows: [][]any{{"edge-id", "Edge"}}},
	}
	result, err := s.plan(context.Background(), mapQueryer{t: t, stub: stubs}, doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	providerNoop, routeNoop := false, false
	for _, a := range result.Actions {
		providerNoop = providerNoop || a.Kind == "provider" && a.Key == "acme" && a.Action == "noop"
		routeNoop = routeNoop || a.Kind == "route" && a.Key == "main" && a.Action == "noop"
	}
	if !providerNoop || !routeNoop {
		t.Fatalf("expected provider and route noop actions: %+v", result.Actions)
	}
}

const pluginDigest = "5f2b7c0e9a41d3b6c8e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6"

// pluginDocument declares acme as a provider of a plugin profile that
// authenticates with authMode, with a second credential slot.
func pluginDocument(authMode string) *Document {
	doc := testDocument()
	acme := &doc.Providers[0]
	acme.Configuration = providers.Configuration{Kind: providers.KindPlugin, AuthMode: authMode, ProfileID: "reference-chat", ProfileRevision: pluginDigest}
	acme.Slots = append(acme.Slots, SlotEntry{Name: "backup", Position: 1, Enabled: true, Weight: 1, CredentialRef: ptr(CredentialRef("acme", "backup"))})
	doc.Pricing = nil
	return doc
}

func TestPlanBlocksPluginProvidersUntilTheirPluginIsInstalled(t *testing.T) {
	doc := pluginDocument(connectors.AuthStaticCredential)
	other := doc.Providers[0]
	other.Name = "acme-eu"
	other.Slots = []SlotEntry{{Name: "default", IsDefault: true, Enabled: true, Weight: 1, CredentialRef: ptr(CredentialRef("acme-eu", "default"))}}
	doc.Providers = append(doc.Providers, other)
	result, err := testServer().plan(t.Context(), mapQueryer{t: t}, doc, nil, nil)
	if err != nil {
		t.Fatalf("an artifact pinning a plugin this installation lacks was refused: %v", err)
	}
	var blockers []planItem
	for _, blocker := range result.Blockers {
		if blocker.Kind == "plugin" {
			blockers = append(blockers, blocker)
		}
	}
	if want := []planItem{{Kind: "plugin", Key: pluginDigest, Action: "blocker", Detail: "plugin_not_installed"}}; !reflect.DeepEqual(blockers, want) {
		t.Fatalf("plugin blockers %+v, want one for the digest both providers pin", blockers)
	}

	// Only the plugin waits for this installation: a malformed reference to
	// it is invalid anywhere.
	doc = pluginDocument(connectors.AuthStaticCredential)
	doc.Providers[0].Configuration.ProfileRevision = "not-a-digest"
	var problem *access.Problem
	if _, err = testServer().plan(t.Context(), mapQueryer{t: t}, doc, nil, nil); !errors.As(err, &problem) || problem.Field != "configuration.profile_revision" {
		t.Fatalf("a malformed plugin reference was not refused: %v", err)
	}
}

// An unconfined plugin build is usable only where the deployment enables the
// unconfined tier: elsewhere it blocks the plan as a build the installation
// lacks does, until the operator enables the tier.
func TestPlanBlocksUnconfinedPluginProvidersWhileTheTierIsDisabled(t *testing.T) {
	manifest, _ := json.Marshal(abi.Manifest{Name: "reference", Version: "1.0.0", Origins: []string{"https://api.reference.example"}, Profiles: []abi.Profile{{
		ID: "reference-chat", Label: "Reference Chat", Dialect: "openai-chat",
		Hosting: abi.Hosting{Address: "https://api.reference.example/v1", Headers: map[string]string{"Authorization": "Bearer {credential}"}},
	}}})
	q := mapQueryer{t: t, stub: []queryStub{{match: "FROM olp.plugins WHERE", row: []any{manifest, true, "reference", nil}}}}
	server := testServer()
	result, err := server.plan(t.Context(), q, pluginDocument(connectors.AuthStaticCredential), nil, nil)
	if err != nil {
		t.Fatalf("an artifact pinning an unconfined plugin was refused with the tier disabled: %v", err)
	}
	if !slices.Contains(result.Blockers, planItem{Kind: "plugin", Key: pluginDigest, Action: "blocker", Detail: "plugin_unconfined_disabled"}) {
		t.Fatalf("an unconfined plugin with the tier disabled did not block the plan: %+v", result.Blockers)
	}

	server.Unconfined = plugins.NewUnconfined(t.TempDir(), plugins.DefaultLimits, slog.New(slog.DiscardHandler))
	if result, err = server.plan(t.Context(), q, pluginDocument(connectors.AuthStaticCredential), nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, blocker := range result.Blockers {
		if blocker.Kind == "plugin" {
			t.Fatalf("a permitted unconfined plugin blocked the plan with the tier enabled: %+v", blocker)
		}
	}
}

func TestPlanMarksGrantSlotsForGrantEnrollment(t *testing.T) {
	doc := pluginDocument(connectors.AuthGrant)
	result, err := testServer().plan(t.Context(), mapQueryer{t: t}, doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"acme/primary", "acme/backup"} {
		if !slices.Contains(result.Actions, planItem{Kind: "credential", Key: ref, Action: "enroll", Detail: "grant_enrollment_required"}) {
			t.Fatalf("%s is not marked for grant enrollment: %+v", ref, result.Actions)
		}
	}
	for _, blocker := range result.Blockers {
		if blocker.Kind == "credential" {
			t.Fatalf("a grant slot needs a secret binding: %+v", blocker)
		}
	}

	// A grant the destination slot already holds serves on, if the plugin
	// build the artifact pins enrolled it.
	configuration, _ := json.Marshal(doc.Providers[0].Configuration)
	held := func(backup string) []queryStub {
		return []queryStub{
			{match: "FROM olp.providers WHERE", row: []any{configuration}},
			{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "plugin", "draft", "edge-id", nil}}},
			{match: "provider_slots s LEFT JOIN", rows: [][]any{{"provider-id", "primary", "slot-id", "cred-id", pluginDigest}, {"provider-id", "backup", "backup-id", "backup-cred-id", backup}}},
			{match: "FROM olp.projects", rows: [][]any{{"edge-id", "Edge"}}},
		}
	}
	for _, backup := range []string{"", strings.Repeat("0", 64)} {
		result, err = testServer().plan(t.Context(), mapQueryer{t: t, stub: held(backup)}, doc, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(result.Actions, planItem{Kind: "credential", Key: "acme/primary", Action: "reuse"}) ||
			!slices.Contains(result.Actions, planItem{Kind: "credential", Key: "acme/backup", Action: "enroll", Detail: "grant_enrollment_required"}) {
			t.Fatalf("a slot holding %q was reused: %+v", backup, result.Actions)
		}
	}

	// Grant enrollment is the only source of a grant slot's credential.
	var problem *access.Problem
	if _, err = testServer().plan(t.Context(), mapQueryer{t: t}, doc, map[string]string{"acme/backup": "pasted"}, nil); !errors.As(err, &problem) || problem.Field != "secret_bindings.acme/backup" {
		t.Fatalf("a grant slot took a secret binding: %v", err)
	}
	doc.Providers[0].Slots[1].CredentialRef = nil
	if _, err = testServer().plan(t.Context(), mapQueryer{t: t}, doc, nil, nil); !errors.As(err, &problem) || problem.Field != "providers.0.slots.1.credential_ref" {
		t.Fatalf("a grant slot without its credential reference was accepted: %v", err)
	}
}

func TestPlanRequiresABindingForAStaticSlotHoldingAGrant(t *testing.T) {
	doc := pluginDocument(connectors.AuthStaticCredential)
	configuration, _ := json.Marshal(doc.Providers[0].Configuration)
	stubs := []queryStub{
		{match: "FROM olp.providers WHERE", row: []any{configuration}},
		{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "plugin", "draft", "edge-id", nil}}},
		{match: "provider_slots s LEFT JOIN", rows: [][]any{{"provider-id", "primary", "slot-id", "cred-id", pluginDigest}}},
		{match: "FROM olp.projects", rows: [][]any{{"edge-id", "Edge"}}},
	}
	result, err := testServer().plan(t.Context(), mapQueryer{t: t, stub: stubs}, doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Blockers, planItem{Kind: "credential", Key: "acme/primary", Action: "blocker", Detail: "secret_binding_required"}) {
		t.Fatalf("a static credential slot reused a grant: %+v", result.Blockers)
	}
}

// Exports never carry a grant, so they reference every grant slot's
// credential: importing and exporting again before any grant enrollment
// reproduces the artifact.
func TestExportsReferenceEveryCredentialSlotAGrantBacks(t *testing.T) {
	doc := pluginDocument(connectors.AuthGrant)
	configuration, _ := json.Marshal(doc.Providers[0].Configuration)
	capabilities, _ := json.Marshal([]map[string]any{
		{"operation": "generation", "surface": "openai", "mode": "unary", "source": "declared"},
		{"operation": "generation", "surface": "openai", "mode": "streaming", "source": "declared"},
	})
	restrictions := []byte(`{"allowed_api_keys":[],"allowed_models":[],"allowed_routes":[]}`)
	stubs := []queryStub{
		{match: "FROM olp.providers WHERE", row: []any{configuration}},
		{match: "FROM olp.providers", rows: [][]any{{"provider-id", "acme", "plugin", "draft", "edge-id", nil}}},
		{match: "provider_slots s LEFT JOIN", rows: [][]any{{"provider-id", "primary", "slot-id", nil, ""}, {"provider-id", "backup", "backup-id", nil, ""}}},
		{match: "provider_slots WHERE", rows: [][]any{{"primary", true, 0, true, 0, 1, nil, restrictions, []byte(`{}`)}, {"backup", false, 1, true, 0, 1, nil, restrictions, []byte(`{}`)}}},
		{match: "FROM olp.provider_models", rows: [][]any{{"gpt-x", "gpt-x", true, capabilities}}},
		{match: "FROM olp.projects", rows: [][]any{{"edge-id", "Edge"}}},
	}
	result, err := testServer().plan(t.Context(), mapQueryer{t: t, stub: stubs}, doc, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(result.Actions, planItem{Kind: "provider", Key: "acme", Action: "noop"}) ||
		!slices.Contains(result.Actions, planItem{Kind: "credential", Key: "acme/backup", Action: "enroll", Detail: "grant_enrollment_required"}) {
		t.Fatalf("an imported grant provider awaiting enrollment changed: %+v", result.Actions)
	}
}

// Apply keeps a model a document omits as a disabled row without
// capabilities; re-planning the same document must not see it as a change.
func TestCanonicalEqualProviderIgnoresTombstonedModels(t *testing.T) {
	desired := testDocument().Providers[0]
	current := testDocument().Providers[0]
	current.Models = append(current.Models, ModelEntry{UpstreamModel: "retired", DisplayName: "retired"})
	if !canonicalEqualProvider(&desired, &current) {
		t.Fatal("a tombstoned model made the provider differ")
	}
	if len(current.Models) != 2 {
		t.Fatalf("comparison mutated the current entry: %+v", current.Models)
	}
	enabled := testDocument().Providers[0]
	enabled.Models = append(enabled.Models, ModelEntry{UpstreamModel: "retired", Enabled: true})
	if canonicalEqualProvider(&desired, &enabled) {
		t.Fatal("an enabled undeclared model must differ")
	}
	capable := testDocument().Providers[0]
	capable.Models = append(capable.Models, ModelEntry{UpstreamModel: "retired", Capabilities: []CapabilityEntry{{Operation: "generation", Surface: "openai", Mode: "unary"}}})
	if canonicalEqualProvider(&desired, &capable) {
		t.Fatal("an undeclared model with capabilities must differ")
	}
}

// A price naming an unknown provider would otherwise apply to every provider
// of its kind.
func TestValidateDocumentRejectsPricesForUnknownProviders(t *testing.T) {
	s := testServer()
	q := mapQueryer{t: t, stub: []queryStub{{match: "FROM olp.providers", rows: [][]any{{"other-id", "Other"}}}}}
	doc := testDocument()
	doc.Pricing.Prices[0].Provider = ptr("acme-typo")
	var problem *access.Problem
	if _, err := s.validateDocument(t.Context(), q, doc); !errors.As(err, &problem) || problem.Field != "pricing.prices.0.provider" {
		t.Fatalf("expected an invalid price provider, got %v", err)
	}
	for _, name := range []string{"ACME", "other"} {
		doc = testDocument()
		doc.Pricing.Prices[0].Provider = ptr(name)
		if _, err := s.validateDocument(t.Context(), q, doc); err != nil {
			t.Fatalf("price for %s: %v", name, err)
		}
	}
}
