package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
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

func TestEstimationFactorsRequirePositiveCanonicalDecimals(t *testing.T) {
	fixture, err := os.ReadFile("testdata/valid/complete.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		factor string
		valid  bool
	}{
		{factor: "0.000000000001", valid: true},
		{factor: "1", valid: true},
		{factor: "999999999999.999999999999", valid: true},
		{factor: "0"},
		{factor: "-1"},
		{factor: "NaN"},
		{factor: "+Inf"},
		{factor: "1e2"},
		{factor: "1.0"},
		{factor: "01"},
		{factor: "1000000000000"},
		{factor: "0.0000000000001"},
	} {
		t.Run(tc.factor, func(t *testing.T) {
			input := bytes.Replace(fixture, []byte(`"factor": "1.18"`), []byte(`"factor": "`+tc.factor+`"`), 1)
			canonical, err := Canonical(input)
			if (err == nil) != tc.valid {
				t.Fatalf("factor %q accepted = %v, want %v: %v", tc.factor, err == nil, tc.valid, err)
			}
			if tc.valid && !bytes.Contains(canonical, []byte(`"factor": "`+tc.factor+`"`)) {
				t.Fatalf("canonical output changed factor %q", tc.factor)
			}
		})
	}
}

func TestEncodeSortsTiersWithoutChangingCatalog(t *testing.T) {
	tiers := []Tier{{AboveInputTokens: 30}, {AboveInputTokens: 10}, {AboveInputTokens: 20}}
	catalog := &Catalog{APIVersion: APIVersion, Vendors: []Vendor{{Models: []Model{{Prices: []Price{{Tiers: slices.Clone(tiers)}}}}}}}

	document, err := Encode(catalog)
	if err != nil {
		t.Fatal(err)
	}
	if got := catalog.Vendors[0].Models[0].Prices[0].Tiers; !slices.Equal(got, tiers) {
		t.Fatalf("encoding changed input tiers: got %v, want %v", got, tiers)
	}
	encoded, err := Decode(document)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := encoded.Vendors[0].Models[0].Prices[0].Tiers, []Tier{tiers[1], tiers[2], tiers[0]}; !slices.Equal(got, want) {
		t.Fatalf("encoded tiers are not sorted: got %v, want %v", got, want)
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
