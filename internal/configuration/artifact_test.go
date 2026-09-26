package configuration

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/runtime"
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
			if err := testServer().validateDocument(doc); err == nil {
				t.Fatalf("expected validation failure for %s", tc.name)
			}
		})
	}
	doc := testDocument()
	if err := testServer().validateDocument(doc); err != nil {
		t.Fatalf("valid document rejected: %v", err)
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
	err := s.validateDocument(doc)
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
	err := s.validateDocument(doc)
	var problem *access.Problem
	if !errors.As(err, &problem) || problem.Status != 422 {
		t.Fatalf("expected duplicate identity rejection, got %v", err)
	}
	doc = testDocument()
	doc.Projects = append(doc.Projects, ProjectEntry{Name: " EDGE "})
	if err = s.validateDocument(doc); err == nil {
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
		{match: "provider_slots s LEFT JOIN", rows: [][]any{{"provider-id", "primary", "slot-id", "cred-id"}}},
		{match: "provider_slots WHERE", rows: [][]any{{"primary", true, 0, true, 0, 1, "cred-id", []byte(`{"allowed_api_keys":[],"allowed_models":[],"allowed_routes":[]}`), []byte(`{}`)}}},
		{match: "FROM olp.provider_models", rows: [][]any{{"gpt-x", "gpt-x", true, capabilities}}},
		{match: "route_drafts WHERE id", row: []any{[]byte(`["generation"]`), 30000, 2, targets, nil, nil}},
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
