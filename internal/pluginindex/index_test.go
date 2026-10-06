package pluginindex

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/signing"
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

func TestTamperedIndexesAreRefused(t *testing.T) {
	tampered := bytes.Replace(document, []byte(`"OpenLLMProxy"`), []byte(`"Someone else"`), 1)
	if bytes.Equal(tampered, document) {
		t.Fatal("the fixture edit changed nothing")
	}
	if _, err := Load(tampered, signature, signing.Trusted()); !errors.Is(err, signing.ErrInvalidSignature) {
		t.Fatalf("an edited index loaded: %v", err)
	}
	if _, err := Load(document, signature, signing.MustKeyring()); !errors.Is(err, signing.ErrUnknownKey) {
		t.Fatalf("an index loaded without a trusted key: %v", err)
	}
}

func TestInvalidIndexesAreRefused(t *testing.T) {
	published := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	valid := func() *Index {
		return &Index{APIVersion: APIVersion, PublishedAt: published, Plugins: []Plugin{{
			Name: "example", Description: "Example.", Maintainer: "Example", DocumentationURL: "https://example.test/docs", Repository: "https://example.test/repo", Path: "plugins/example",
			Releases: []Release{{Version: "1.0.0", Digest: strings.Repeat("a", 64), ABIVersion: 1, SizeBytes: 10, Origins: []string{"https://api.example.test"}, Profiles: []string{"example-chat"}, Commit: strings.Repeat("b", 40), ReviewedAt: published}},
		}}}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Index){
		"bad digest":          func(x *Index) { x.Plugins[0].Releases[0].Digest = "abc" },
		"insecure repository": func(x *Index) { x.Plugins[0].Repository = "http://example.test/repo" },
		"no releases":         func(x *Index) { x.Plugins[0].Releases = nil },
		"no origins":          func(x *Index) { x.Plugins[0].Releases[0].Origins = nil },
		"reviewed later":      func(x *Index) { x.Plugins[0].Releases[0].ReviewedAt = published.Add(time.Hour) },
		"escaping path":       func(x *Index) { x.Plugins[0].Path = "../elsewhere" },
		"repeated plugin":     func(x *Index) { x.Plugins = append(x.Plugins, x.Plugins[0]) },
	} {
		x := valid()
		mutate(x)
		if x.Validate() == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	encoded, _ := json.Marshal(map[string]any{"api_version": "openllmproxy.dev/plugin-index/v2"})
	if _, err := Decode(encoded); !errors.Is(err, signing.ErrUnsupportedVersion) {
		t.Fatalf("another format version: %v", err)
	}
}
