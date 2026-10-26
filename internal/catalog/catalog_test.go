package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedCatalogIsCanonical(t *testing.T) {
	canonical, err := Canonical(document)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical, document) {
		t.Fatal("catalog.json is not canonical; run make catalog")
	}
}

func TestValidFixturesDecodeAndValidate(t *testing.T) {
	paths := fixturePaths(t, "testdata/valid")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			catalog, err := decodeFixture(path)
			if err != nil {
				t.Fatalf("valid fixture does not decode: %v", err)
			}
			if err := catalog.Validate(); err != nil {
				t.Fatalf("valid fixture does not validate: %v", err)
			}
		})
	}
}

func TestInvalidFixturesAreRejected(t *testing.T) {
	paths := fixturePaths(t, "testdata/invalid")
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			catalog, err := decodeFixture(path)
			if err == nil {
				err = catalog.Validate()
			}
			if err == nil {
				t.Fatal("invalid fixture decodes and validates")
			}
		})
	}
}

func fixturePaths(t *testing.T, dir string) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no fixtures under %s", dir)
	}
	return paths
}

func decodeFixture(path string) (*Catalog, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(raw)
}

// Counters read the estimation section alone; it is the validated one.
