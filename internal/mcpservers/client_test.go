package mcpservers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

type fakeUpstream struct {
	pages    map[string]ToolList
	protocol string
	err      error
}

func (f fakeUpstream) Protocol() string { return f.protocol }
func (f fakeUpstream) ListTools(_ context.Context, cursor string) (ToolList, error) {
	if f.err != nil {
		return ToolList{}, f.err
	}
	page, ok := f.pages[cursor]
	if !ok {
		return ToolList{}, errors.New("unknown cursor")
	}
	return page, nil
}
func (f fakeUpstream) CallTool(context.Context, string, json.RawMessage) (ToolResult, error) {
	return ToolResult{}, errors.New("certification must not invoke tools")
}
func (f fakeUpstream) ListChanged() bool     { return false }
func (f fakeUpstream) Close(context.Context) {}

func listedTool(name string) ListedTool {
	return ListedTool{Name: name, Description: "does " + name, InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)}
}

func singlePageUpstream(tools ...ListedTool) fakeUpstream {
	return fakeUpstream{protocol: "2025-11-25", pages: map[string]ToolList{"": {Tools: tools}}}
}

func TestCertifyCatalogSortsToolsAndDigestsStably(t *testing.T) {
	ctx := context.Background()
	first, err := CertifyCatalog(ctx, singlePageUpstream(listedTool("zeta"), listedTool("alpha")), nil)
	if err != nil {
		t.Fatalf("CertifyCatalog: %v", err)
	}
	if len(first.Tools) != 2 || first.Tools[0].Name != "alpha" || first.Tools[1].Name != "zeta" {
		t.Fatalf("tools not sorted by name: %+v", first.Tools)
	}
	second, err := CertifyCatalog(ctx, singlePageUpstream(listedTool("alpha"), listedTool("zeta")), nil)
	if err != nil {
		t.Fatalf("CertifyCatalog: %v", err)
	}
	if first.Digest == "" || first.Digest != second.Digest {
		t.Fatalf("digest depends on listing order: %q vs %q", first.Digest, second.Digest)
	}
	if first.Protocol != "2025-11-25" {
		t.Fatalf("protocol = %q", first.Protocol)
	}
	changed := listedTool("alpha")
	changed.Description = "different"
	third, err := CertifyCatalog(ctx, singlePageUpstream(changed, listedTool("zeta")), nil)
	if err != nil {
		t.Fatalf("CertifyCatalog: %v", err)
	}
	if third.Digest == first.Digest {
		t.Fatal("digest did not change when a tool description changed")
	}
}

func TestCertifyCatalogFollowsPagesUntilCursorEnds(t *testing.T) {
	upstream := fakeUpstream{protocol: "2025-11-25", pages: map[string]ToolList{
		"":     {Tools: []ListedTool{listedTool("one")}, NextCursor: "next"},
		"next": {Tools: []ListedTool{listedTool("two")}},
	}}
	catalog, err := CertifyCatalog(context.Background(), upstream, nil)
	if err != nil {
		t.Fatalf("CertifyCatalog: %v", err)
	}
	if len(catalog.Tools) != 2 {
		t.Fatalf("tools = %+v", catalog.Tools)
	}
}

func TestCertifyCatalogRejections(t *testing.T) {
	credential := []byte("secret-bearer")
	many := make([]ListedTool, 0, 129)
	for i := range 129 {
		many = append(many, listedTool(fmt.Sprintf("tool-%d", i)))
	}
	reflectedInSchema := ListedTool{Name: "alpha", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","default":"secret-bearer"}}}`)}
	escapedInSchema := ListedTool{Name: "alpha", InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string","default":"secret\u002dbearer"}}}`)}
	cases := []struct {
		name       string
		upstream   fakeUpstream
		credential []byte
	}{
		{"list error", fakeUpstream{err: errors.New("down")}, nil},
		{"missing tool list", fakeUpstream{pages: map[string]ToolList{"": {}}}, nil},
		{"duplicate names", singlePageUpstream(listedTool("alpha"), listedTool("alpha")), nil},
		{"invalid tool name", singlePageUpstream(listedTool("bad name!")), nil},
		{"too many tools", singlePageUpstream(many...), nil},
		{"non-object schema", singlePageUpstream(ListedTool{Name: "alpha", InputSchema: json.RawMessage(`{"type":"array"}`)}), nil},
		{"cursor cycle", fakeUpstream{pages: map[string]ToolList{
			"":     {Tools: []ListedTool{listedTool("one")}, NextCursor: "loop"},
			"loop": {Tools: []ListedTool{}, NextCursor: "loop"},
		}}, nil},
		{"credential in description", singlePageUpstream(ListedTool{Name: "alpha", Description: "uses secret-bearer", InputSchema: json.RawMessage(`{"type":"object"}`)}), credential},
		{"credential in schema bytes", singlePageUpstream(reflectedInSchema), credential},
		{"escaped credential in schema", singlePageUpstream(escapedInSchema), credential},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CertifyCatalog(context.Background(), tc.upstream, tc.credential); !errors.Is(err, ErrCertification) {
				t.Fatalf("CertifyCatalog err = %v, want ErrCertification", err)
			}
		})
	}
}

func TestCertifyCatalogRejectsEndlessPagination(t *testing.T) {
	pages := map[string]ToolList{}
	cursor := ""
	for page := range 20 {
		next := fmt.Sprintf("page-%d", page+1)
		pages[cursor] = ToolList{Tools: []ListedTool{listedTool(fmt.Sprintf("tool-%d", page))}, NextCursor: next}
		cursor = next
	}
	pages[cursor] = ToolList{Tools: []ListedTool{}}
	if _, err := CertifyCatalog(context.Background(), fakeUpstream{pages: pages}, nil); !errors.Is(err, ErrCertification) {
		t.Fatalf("CertifyCatalog err = %v, want ErrCertification past the page bound", err)
	}
}

func TestCertifyCatalogAllowsEmptyCatalog(t *testing.T) {
	empty := fakeUpstream{protocol: "2025-11-25", pages: map[string]ToolList{"": {Tools: []ListedTool{}}}}
	catalog, err := CertifyCatalog(context.Background(), empty, nil)
	if err != nil {
		t.Fatalf("CertifyCatalog: %v", err)
	}
	if len(catalog.Tools) != 0 || catalog.Digest == "" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestValidateSchemaAcceptsObjectSchema(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	if err := validateSchema(schema); err != nil {
		t.Fatalf("validateSchema rejected a valid object schema: %v", err)
	}
}

func TestValidateSchemaRejectsNonObjectSchema(t *testing.T) {
	for _, schema := range []string{
		`{"type":"string"}`,
		`{"properties":{}}`,
		`["object"]`,
		`not json`,
	} {
		if err := validateSchema(json.RawMessage(schema)); err == nil {
			t.Errorf("validateSchema accepted %s", schema)
		}
	}
}

func TestValidateSchemaRejectsRemoteReference(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"x":{"$ref":"https://example.com/schema.json"}}}`)
	if err := validateSchema(schema); err == nil {
		t.Fatal("validateSchema accepted a schema that needs a remote reference")
	}
}

func TestValidateSchemaRejectsDeeplyNestedSchema(t *testing.T) {
	schema := `{"type":"object","properties":{"a":` + strings.Repeat(`{"type":"object","properties":{"a":`, 40) + `{"type":"string"}` + strings.Repeat(`}}`, 40) + `}}`
	if err := validateSchema(json.RawMessage(schema)); err == nil {
		t.Fatal("validateSchema accepted a schema past the depth bound")
	}
}

func TestReflectsCredentialFindsNestedSecrets(t *testing.T) {
	const credential = "bearer-token-value"
	cases := []struct {
		value any
		want  bool
	}{
		{"plain text", false},
		{"prefix " + credential + " suffix", true},
		{[]any{"safe", map[string]any{"default": credential}}, true},
		{map[string]any{credential: "safe"}, true},
		{map[string]any{"outer": []any{map[string]any{"inner": "safe"}}}, false},
		{nil, false},
		{42, false},
	}
	for _, tc := range cases {
		if got := reflectsCredential(tc.value, credential); got != tc.want {
			t.Errorf("reflectsCredential(%v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}
