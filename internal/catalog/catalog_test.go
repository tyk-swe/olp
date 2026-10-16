package catalog

import (
	"bytes"
	"os"
	"path/filepath"
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

func TestEmbeddedCatalogIsCanonical(t *testing.T) {
	canonical, err := Canonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, document) {
		t.Fatal("catalog.json is not canonical; run make catalog")
	}
}

// Counters read the estimation section alone; it is the validated one.

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
