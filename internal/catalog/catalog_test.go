package catalog

import (
	"bytes"
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

// Counters read the estimation section alone; it is the validated one.
