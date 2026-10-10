package oif_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
	"github.com/tyk-swe/olp/tests/fidelity"
)

func source(t *testing.T, raw string) oif.Document {
	t.Helper()
	d, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestSourceRetainsExactRepresentationAndOwnsBytes(t *testing.T) {
	raw := []byte(` {"n":9007199254740993,"decimal":1.0000000000000001,"tiny":1e-400,"minus_zero":-0,"null":null,"zero":0,"false":false,"empty":"","arr":[],"é":"é","escaped":"\ud83d\ude00","a/b":{"~":1}} `)
	d, err := oif.ParseJSON(raw, oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), raw...)
	raw[2] = 'x'
	copyOut := d.Bytes()
	copyOut[2] = 'x'
	fields := d.Fields()
	fields["n"][0] = '0'
	delete(fields, "null")
	if !bytes.Equal(d.Bytes(), want) {
		t.Fatal("caller or returned alias mutated authoritative source")
	}
	if err := fidelity.Compare(want, d.Bytes()); err != nil {
		t.Fatal(err)
	}
	for path, expected := range map[string]string{"/n": "9007199254740993", "/decimal": "1.0000000000000001", "/tiny": "1e-400", "/minus_zero": "-0", "/a~1b/~0": "1"} {
		v, ok := d.Lookup(path)
		if !ok || v.Raw() != expected {
			t.Fatalf("source span %s changed", path)
		}
	}
	for path, presence := range map[string]oif.Presence{"/missing": oif.Missing, "/null": oif.ExplicitNull, "/zero": oif.Present, "/false": oif.Present, "/empty": oif.Present, "/arr": oif.Present} {
		v, _ := d.Lookup(path)
		if v.Presence() != presence {
			t.Fatalf("presence lost at %s", path)
		}
	}
}
func TestSourceRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	for _, raw := range []string{`{"x":1,"x":2}`, `{"outer":{"x":1,"\u0078":2}}`, `{"x":"\ud800"}`, `{"x":"\udc00"}`, `{"x":"\ud800\u0041"}`, `{"x":01}`, `{"x":1.}`, `{"x":+1}`, `{"x":1e}`, `{"x":true,}`, `[1,]`, `{} {}`, "{\"x\":\"\xff\"}"} {
		t.Run(raw, func(t *testing.T) {
			if _, err := oif.ParseJSON([]byte(raw), oif.Limits{}); err == nil {
				t.Fatal("invalid source accepted")
			}
		})
	}
	for _, raw := range []string{`{"x":"\\ud800"}`, `{"x":"\ud83d\ude00"}`, `{"a":"é","b":"é"}`, `[0,-0,1.0,1e400]`} {
		if _, err := oif.ParseJSON([]byte(raw), oif.Limits{}); err != nil {
			t.Fatal(err)
		}
	}
}
func TestSourceLimitsBoundEveryRepresentation(t *testing.T) {
	for _, test := range []struct {
		raw    string
		limits oif.Limits
	}{{`{"x":1}`, oif.Limits{MaxBytes: 3}}, {`[[[0]]]`, oif.Limits{MaxDepth: 2}}, {`[0,0,0]`, oif.Limits{MaxNodes: 3}}} {
		if _, err := oif.ParseJSON([]byte(test.raw), test.limits); err == nil {
			t.Fatal("source bound ignored")
		}
	}
}
func TestOverlaysRetainSourceAndRejectAmbiguousArrayPointers(t *testing.T) {
	d := source(t, `{"model":"route","native": { "n":1e400, "opaque":"\u0061" },"array":[0,1]}`)
	desc := oif.Descriptor{Operation: oif.Identity{ID: "fixture", Revision: "1"}, Dialect: oif.Identity{ID: "fixture-wire", Revision: "1"}}
	r, err := oif.NewRequest(desc, d)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := oif.NewRegistry([]oif.Operation{{Identity: desc.Operation}}, []oif.Binding{{Dialect: desc.Dialect, Operation: desc.Operation, IdentityRules: []oif.IdentityRule{{Pointer: "/model", Origin: oif.IdentityBinding, Kind: oif.String}}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := registry.PrepareIdentity(r, desc, []oif.Change{{Pointer: "/model", Value: `"upstream"`, Origin: oif.IdentityBinding, Reason: "configured model"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.Request().Source().Raw() != d.Raw() {
		t.Fatal("preparation mutated source")
	}
	before, _ := d.Lookup("/native")
	after, _ := p.Document().Lookup("/native")
	if before.Raw() != after.Raw() {
		t.Fatal("identity changed unknown native subtree spelling")
	}
	for _, path := range []string{"/array/+1", "/array/-0", "/array/01", "/array/1x", "/array/"} {
		if _, ok := d.Lookup(path); ok {
			t.Fatalf("noncanonical array pointer %s accepted", path)
		}
		if _, err := oif.Apply(d, []oif.Change{{Pointer: path, Value: "2", Origin: oif.IdentityBinding, Reason: "fixture"}}); err == nil {
			t.Fatalf("overlay falsely claims an unapplied change at %s", path)
		}
	}
	if _, err := oif.PrepareIdentity(r, desc, []oif.Change{{Pointer: "/model", Value: `"x"`, Origin: oif.ProviderDefault, Reason: "default"}}); err == nil {
		t.Fatal("semantic default claimed identity")
	}
	for _, origin := range []oif.Origin{oif.IdentityBinding, oif.ExplicitTransform, oif.TransformedMapping} {
		change := oif.Change{Pointer: "/native", Value: `{}`, Origin: origin, Reason: "misleading classification"}
		if _, err := registry.PrepareIdentity(r, desc, []oif.Change{change}); err == nil {
			t.Fatal("semantic change mislabeled as identity")
		}
		modified, err := r.WithChanges(change)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := registry.PrepareIdentity(modified, desc, nil); err == nil {
			t.Fatal("prior semantic mutation mislabeled as identity")
		}
	}
	if _, err := oif.Apply(d, []oif.Change{{Pointer: "/native", Value: `{}`, Origin: oif.ExplicitTransform, Reason: "fixture"}, {Pointer: "/native/n", Value: `1`, Origin: oif.ExplicitTransform, Reason: "fixture"}}); err == nil {
		t.Fatal("overlapping overlay accepted")
	}
}

func TestApplyRejectsOverlapSeparatedBySortedSibling(t *testing.T) {
	limits := oif.Limits{MaxBytes: 8192, MaxNodes: 256, MaxDepth: 16}
	change := func(pointer, value string) oif.Change {
		return oif.Change{Pointer: pointer, Value: value, Origin: oif.ExplicitTransform, Reason: "fixture"}
	}
	for _, sibling := range []string{"a!", "a-x", "a.x"} {
		d, err := oif.ParseJSON([]byte(`{"a":{"b":1},"`+sibling+`":2}`), limits)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := oif.Apply(d, []oif.Change{change("/a", `{"b":5}`), change("/"+sibling, `3`), change("/a/b", `9`)}); err == nil {
			t.Fatalf("overlap hidden by sibling %q accepted", sibling)
		}
		if _, err := oif.Apply(d, []oif.Change{change("/a", `{"b":5}`), change("/"+sibling, `3`)}); err != nil {
			t.Fatalf("disjoint overlay with sibling %q rejected: %v", sibling, err)
		}
	}
}

func FuzzSourceRetainsValidNativeLexemes(f *testing.F) {
	for _, s := range []string{`{"n":9007199254740993}`, `{"x":"\ud83d\ude00"}`, `[]`, `null`, `{"a":[false,0,""]}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 8192, MaxNodes: 256, MaxDepth: 16})
		if err != nil {
			return
		}
		if !json.Valid(d.Bytes()) || !bytes.Equal(data, d.Bytes()) {
			t.Fatal("successful source parse changed JSON")
		}
		if strings.Contains(d.Raw(), "\xff") {
			t.Fatal("invalid UTF-8 accepted")
		}
	})
}

// decoded is what encoding/json makes of a JSON string, which Text and Chars
// are held to for every string a document can hold.
func decoded(t testing.TB, raw string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return s
}

func TestStringsDecodeAsEncodingJSONDecodesThem(t *testing.T) {
	for _, raw := range []string{
		`""`, `"plain"`, `"café 日本語 😀"`, `"a\"b"`, `"\\"`, `"\/"`, `"\b\f\n\r\t"`,
		`"\u0000"`, `"\u001f"`, `"Aéあ"`, `"😀"`, `"😀😀"`, `"􏿿"`, `"  "`,
		`"line one\nline two\n"`, `"\\u0041"`, `"\\\\"`, `"trailing\\"`, `"\n"`, `"xAy\\z\"w"`,
		`"` + strings.Repeat("The report says: \\\"slow\\\"\\n", 300) + `"`,
	} {
		v, _ := source(t, `{"s":`+raw+`}`).Lookup("/s")
		want := decoded(t, raw)
		if got, ok := v.Text(); !ok || got != want {
			t.Errorf("Text of %.60s is %q, want %q", raw, got, want)
		}
		if got, ok := v.Chars(); !ok || got != want {
			t.Errorf("Chars of %.60s is %q, want %q", raw, got, want)
		}
	}
	for _, raw := range []string{`null`, `0`, `true`, `[]`, `{}`, `["a"]`} {
		v, _ := source(t, `{"s":`+raw+`}`).Lookup("/s")
		if text, ok := v.Text(); ok || text != "" {
			t.Errorf("Text of %s is %q, %v", raw, text, ok)
		}
		if text, ok := v.Chars(); ok || text != "" {
			t.Errorf("Chars of %s is %q, %v", raw, text, ok)
		}
	}
	if text, ok := (oif.Value{}).Chars(); ok || text != "" {
		t.Errorf("Chars of no value is %q, %v", text, ok)
	}
}

// Chars reads a string that has no escape without copying it, which is what lets
// the walk of a request's prompt read a 400 KB message as often as it likes at
// the price of the one pass that decodes the rest.
func TestCharsDoesNotCopyAStringWithNoEscape(t *testing.T) {
	plain, _ := source(t, `{"s":"`+strings.Repeat("a long message ", 1000)+`"}`).Lookup("/s")
	if got := testing.AllocsPerRun(20, func() { plain.Chars() }); got != 0 {
		t.Errorf("Chars of a string with no escape allocates %v times", got)
	}
	if got := testing.AllocsPerRun(20, func() { plain.Text() }); got != 1 {
		t.Errorf("Text of a string with no escape allocates %v times, want the one copy", got)
	}
	escaped, _ := source(t, `{"s":"`+strings.Repeat("a long message\\n", 1000)+`"}`).Lookup("/s")
	if got := testing.AllocsPerRun(20, func() { escaped.Text() }); got != 1 {
		t.Errorf("Text of a string with escapes allocates %v times, want its one decoded copy", got)
	}
}

func FuzzStringsDecodeAsEncodingJSONDecodesThem(f *testing.F) {
	for _, s := range []string{`{"a":"😀 x\n\"y\"\\"}`, `["\u0000\u001f","é\/","\\\\u0041"]`, `"plain"`, `{"k\n":{"k":["\t"]}}`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := oif.ParseJSON(data, oif.Limits{MaxBytes: 8192, MaxNodes: 256, MaxDepth: 16})
		if err != nil {
			return
		}
		var visit func(v oif.Value)
		visit = func(v oif.Value) {
			switch v.Kind() {
			case oif.String:
				want := decoded(t, v.Raw())
				if got, ok := v.Text(); !ok || got != want {
					t.Fatalf("Text of %q is %q, want %q", v.Raw(), got, want)
				}
				if got, ok := v.Chars(); !ok || got != want {
					t.Fatalf("Chars of %q is %q, want %q", v.Raw(), got, want)
				}
			case oif.Array:
				for _, e := range v.Elements() {
					visit(e)
				}
			case oif.Object:
				for _, m := range v.Members() {
					visit(m.Value)
				}
			}
		}
		visit(d.Root())
	})
}

func TestFieldsWithoutLeavesOutOnlyTheNamedMembers(t *testing.T) {
	d := source(t, `{"messages":[{"role":"user"}],"model":"m","":1,"a":null}`)
	all := d.Fields()
	if got := d.FieldsWithout(); len(got) != 4 || string(got["messages"]) != `[{"role":"user"}]` || string(got["a"]) != "null" {
		t.Fatalf("no names left out %v of %v", got, all)
	}
	got := d.FieldsWithout("messages", "missing")
	if len(got) != 3 || got["messages"] != nil || string(got["model"]) != `"m"` || string(got[""]) != "1" {
		t.Fatalf("messages left out: %v", got)
	}
	// Left out of a copy, and not out of the document.
	if v, ok := d.Lookup("/messages"); !ok || v.Raw() != `[{"role":"user"}]` {
		t.Fatal("the document lost a member")
	}
	if got := source(t, `[1]`).FieldsWithout("a"); got != nil {
		t.Fatalf("a document that is not an object has fields %v", got)
	}
}

func TestDefaultNodeLimitRejectsCompactScalarFlood(t *testing.T) {
	for _, raw := range []string{
		`[` + strings.Repeat(`0,`, 1<<16) + `0]`,
		`{"input":[` + strings.Repeat(`0,`, 1<<16) + `0]}`,
	} {
		_, err := oif.ParseJSON([]byte(raw), oif.Limits{})
		if err == nil || !strings.Contains(err.Error(), "node_limit") {
			t.Fatalf("compact flood should fail node budget, got %v", err)
		}
	}
}

func TestDefaultNodeLimitCountsContainersAtTheBoundary(t *testing.T) {
	for _, tc := range []struct {
		prefix, suffix string
		containers     int
	}{{"[", "]", 1}, {`{"input":[`, "]}", 2}} {
		values := (1 << 16) - tc.containers
		body := tc.prefix + strings.Repeat("0,", values-1) + "0" + tc.suffix
		if _, err := oif.ParseJSON([]byte(body), oif.Limits{}); err != nil {
			t.Fatalf("document at the node limit: %v", err)
		}
		body = tc.prefix + strings.Repeat("0,", values) + "0" + tc.suffix
		if _, err := oif.ParseJSON([]byte(body), oif.Limits{}); err == nil || !strings.Contains(err.Error(), "node_limit") {
			t.Fatalf("document one value beyond the limit: %v", err)
		}
	}
}
