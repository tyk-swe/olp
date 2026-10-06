package providers

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/vendors"
)

var update = flag.Bool("update", false, "rewrite golden files from the current behavior")

// golden compares got with testdata/name, or rewrites it under -update.
func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed; review the difference as a change of the onboarding catalogue and rerun with -update", path)
	}
}

func indented(t *testing.T, v any) []byte {
	t.Helper()
	encoded, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(encoded, '\n')
}

// TestOnboardingCatalogueIsReviewed pins what /provider-kinds and
// /provider-vendors publish.
func TestOnboardingCatalogueIsReviewed(t *testing.T) {
	golden(t, "kinds.golden", indented(t, map[string]any{"items": kinds}))
	golden(t, "vendors.golden", indented(t, vendorCatalogue))
}

// TestCertifiableTuplesAreReviewed pins every tuple certification may probe,
// for each kind's default vendor and every reviewed vendor of its kind.
func TestCertifiableTuplesAreReviewed(t *testing.T) {
	var out strings.Builder
	for _, kind := range kinds {
		candidates := []string{vendors.DefaultFor(kind.Kind)}
		for _, v := range vendorCatalogue {
			if v.Connector == kind.Kind && v.ID != vendors.DefaultFor(kind.Kind) {
				candidates = append(candidates, v.ID)
			}
		}
		for _, vendor := range candidates {
			var tuples []string
			for _, c := range capabilitiesFor(kind.Kind, vendor) {
				tuples = append(tuples, c.Operation+"/"+c.Surface+"/"+c.Mode)
			}
			fmt.Fprintf(&out, "%s %s: %s\n", kind.Kind, vendor, strings.Join(tuples, " "))
		}
	}
	golden(t, "certifiable.golden", []byte(out.String()))
}
