package pluginindex

import (
	"bytes"
	"strings"
	"testing"
)

func TestEmbeddedIndexVerifiesAndIsCanonical(t *testing.T) {
	signed, err := Embedded()
	if err != nil {
		t.Fatalf("%v; after editing index.json run make catalog-sign", err)
	}
	canonical, err := Canonical(document)
	if err != nil || !bytes.Equal(canonical, document) {
		t.Fatalf("index.json is not canonical; run make catalog: %v", err)
	}
	for _, plugin := range signed.Index.Plugins {
		release := plugin.Releases[0]
		if listing, ok := signed.Find(release.Digest); !ok || listing.Plugin.Name != plugin.Name || listing.Release.Version != release.Version {
			t.Fatalf("%s %s is not found by its digest", plugin.Name, release.Version)
		}
	}
	if _, ok := signed.Find(strings.Repeat("0", 64)); ok {
		t.Fatal("an unlisted digest was found")
	}
}
