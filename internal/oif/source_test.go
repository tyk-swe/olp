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
