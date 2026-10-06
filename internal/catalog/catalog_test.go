package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/tyk-swe/olp/internal/signing"
)

func compileSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", document); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	return schema
}

func schemaValid(t *testing.T, schema *jsonschema.Schema, raw []byte) error {
	t.Helper()
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	return schema.Validate(instance)
}

func goValid(raw []byte) error {
	c, err := Decode(raw)
	if err != nil {
		return err
	}
	return c.Validate()
}

// TestSchemaAndValidatorAgree keeps the published schema and the validator
// the binary applies in step. Fixtures named semantic-* break a rule only the
// validator can state, such as uniqueness or the order of dates. Fixtures
// named schema-* break a rule only the schema states: a binary still loads a
// newer catalog that names an unrepresentable component it does not know.
func TestSchemaAndValidatorAgree(t *testing.T) {
	schema := compileSchema(t)
	valid, _ := filepath.Glob("testdata/valid/*.json")
	invalid, _ := filepath.Glob("testdata/invalid/*.json")
	if len(valid) == 0 || len(invalid) == 0 {
		t.Fatal("the validation corpus is missing")
	}
	for _, path := range valid {
		raw, _ := os.ReadFile(path)
		if err := schemaValid(t, schema, raw); err != nil {
			t.Fatalf("%s fails the schema: %v", path, err)
		}
		if err := goValid(raw); err != nil {
			t.Fatalf("%s fails validation: %v", path, err)
		}
	}
	for _, path := range invalid {
		raw, _ := os.ReadFile(path)
		name := filepath.Base(path)
		if err := goValid(raw); (err == nil) != strings.HasPrefix(name, "schema-") {
			t.Fatalf("%s: validation verdict %v disagrees with its kind", path, err)
		}
		if err := schemaValid(t, schema, raw); (err == nil) != strings.HasPrefix(name, "semantic-") {
			t.Fatalf("%s: schema verdict %v disagrees with its kind", path, err)
		}
	}
}

func TestEmbeddedCatalogMatchesTheSchema(t *testing.T) {
	if err := schemaValid(t, compileSchema(t), document); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedCatalogIsCanonical(t *testing.T) {
	canonical, err := Canonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, document) {
		t.Fatal("catalog.json is not canonical; run make catalog")
	}
}

func TestEmbeddedCatalogVerifies(t *testing.T) {
	signed, err := Embedded()
	if err != nil {
		t.Fatalf("%v; after editing catalog.json run make catalog-sign", err)
	}
	if signed.Source() != "catalog@"+signed.SHA256 || len(signed.SHA256) != 64 {
		t.Fatalf("source = %s", signed.Source())
	}
	// Counters read the estimation section alone; it is the validated one.
	estimation, err := EmbeddedEstimation()
	if err != nil || !reflect.DeepEqual(estimation, signed.Catalog.Estimation) {
		t.Fatalf("estimation = %+v %v, want %+v", estimation, err, signed.Catalog.Estimation)
	}
}

// TestTamperedCatalogsAreRefused exercises the start-up path with altered
// bytes: an edited document, an edited signature, and a signature by a key
// the build does not trust.
func TestTamperedCatalogsAreRefused(t *testing.T) {
	tampered := bytes.Replace(document, []byte(`"USD"`), []byte(`"EUR"`), 1)
	if _, err := Load(tampered, signature, signing.Trusted()); !errors.Is(err, signing.ErrInvalidSignature) {
		t.Fatalf("an edited catalog loaded: %v", err)
	}
	var file signing.File
	if err := json.Unmarshal(signature, &file); err != nil {
		t.Fatal(err)
	}
	file.Signatures[0].Signature[0] ^= 1
	forged, _ := json.Marshal(file)
	if _, err := Load(document, forged, signing.Trusted()); !errors.Is(err, signing.ErrInvalidSignature) {
		t.Fatalf("a forged signature loaded: %v", err)
	}
	if _, err := Load(document, signature, signing.MustKeyring()); !errors.Is(err, signing.ErrUnknownKey) {
		t.Fatalf("a catalog loaded without a trusted key: %v", err)
	}
}

func TestCanonicalFormSortsAndNormalizes(t *testing.T) {
	raw, _ := os.ReadFile("testdata/valid/complete.json")
	c, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	models := c.Vendors[0].Models
	models[0], models[1] = models[1], models[0]
	models[1].InputModalities = []string{"text", "image", "text"}
	once, err := Encode(c)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Canonical(once)
	if err != nil || !bytes.Equal(once, twice) {
		t.Fatalf("canonical form is not a fixed point: %v", err)
	}
	if !bytes.Equal(once, raw) {
		t.Fatalf("reordered catalog encodes differently:\n%s", once)
	}
}

func TestLookupMatchesIdentifiersAliasesAndCanonicalModels(t *testing.T) {
	raw, _ := os.ReadFile("testdata/valid/complete.json")
	signed := loadUnsigned(t, raw)
	for _, test := range []struct {
		names []string
		match Match
	}{
		{[]string{"example-large"}, MatchID},
		{[]string{"unknown", "example-large-2026-09-01"}, MatchAlias},
		{[]string{"example/example-large"}, MatchCanonical},
	} {
		model, match, ok := signed.Lookup("example", test.names...)
		if !ok || model.ID != "example-large" || match != test.match {
			t.Fatalf("%v matched %v by %s", test.names, model, match)
		}
	}
	if _, _, ok := signed.Lookup("other", "example-large"); ok {
		t.Fatal("a model matched under another vendor")
	}
	if lifecycle := signed.Lifecycle("example", "example-large"); lifecycle == nil || *lifecycle.RetiresAt != "2027-03-01" || *lifecycle.Replacement != "example-xl" {
		t.Fatalf("lifecycle = %+v", lifecycle)
	}
}

// loadUnsigned signs a test catalog with a throwaway key.
func loadUnsigned(t *testing.T, raw []byte) *Signed {
	t.Helper()
	key, private := testKey(t)
	signatures, err := signing.Sign(nil, key.ID, private, raw)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := Load(raw, signatures, signing.MustKeyring(key))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}
