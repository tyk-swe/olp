package plugins

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/plugin/abi"
)

func sampleManifest() abi.Manifest {
	return abi.Manifest{
		Name:        "sample-plugin",
		Version:     "1.0.0",
		Description: "Sample provider plugin",
		Origins:     []string{"https://api.example.test"},
		Profiles: []abi.Profile{{
			ID:      "sample-chat",
			Label:   "Sample chat",
			Dialect: "openai-chat",
			Hosting: abi.Hosting{
				Address: "https://api.example.test/v1",
				Headers: map[string]string{"Authorization": "Bearer {credential}"},
			},
		}},
	}
}

func mustMarshalManifest(t *testing.T, m abi.Manifest) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDecodeManifestAcceptsValidConfinedAndUnconfinedManifests(t *testing.T) {
	t.Parallel()
	confined := sampleManifest()
	if _, err := decodeManifest(mustMarshalManifest(t, confined), false); err != nil {
		t.Fatalf("confined manifest rejected: %v", err)
	}

	guardrailOnly := abi.Manifest{
		Guardrail: true,
		Name:      "guard-plugin",
		Version:   "2.0.0",
		Origins:   []string{},
	}
	if _, err := decodeManifest(mustMarshalManifest(t, guardrailOnly), false); err != nil {
		t.Fatalf("guardrail-only confined manifest rejected: %v", err)
	}

	unconfined := sampleManifest()
	unconfined.Profiles[0].CarriesTraffic = true
	if _, err := decodeManifest(mustMarshalManifest(t, unconfined), true); err != nil {
		t.Fatalf("unconfined carrying manifest rejected: %v", err)
	}

	grantFactAddress := sampleManifest()
	grantFactAddress.Profiles[0].Grant = &abi.GrantAuthentication{Facts: []string{"base_url"}}
	grantFactAddress.Profiles[0].Hosting.Address = "{grant.base_url}/v1"
	if _, err := decodeManifest(mustMarshalManifest(t, grantFactAddress), false); err != nil {
		t.Fatalf("grant-fact address manifest rejected: %v", err)
	}
}

func TestDecodeManifestRejectsSizeLimitAndUnknownFields(t *testing.T) {
	t.Parallel()
	oversized := make([]byte, maxManifestBytes+1)
	for i := range oversized {
		oversized[i] = ' '
	}
	if _, err := decodeManifest(oversized, false); err == nil {
		t.Fatal("oversized manifest accepted")
	}

	raw := `{"name":"sample-plugin","version":"1.0.0","origins":["https://api.example.test"],"profiles":[],"unexpected":true}`
	var pluginErr *Error
	if _, err := decodeManifest([]byte(raw), false); !errors.As(err, &pluginErr) || pluginErr.Code != CodeManifestInvalid {
		t.Fatalf("unknown field error = %v, want %s", err, CodeManifestInvalid)
	}
}

func TestValidateManifestEnforcesConfinementOriginsAndProfiles(t *testing.T) {
	t.Parallel()
	seventeenOrigins := make([]string, 17)
	for i := range seventeenOrigins {
		seventeenOrigins[i] = fmt.Sprintf("https://o%d.example.test", i)
	}

	cases := map[string]struct {
		unconfined bool
		mutate     func(*abi.Manifest)
		wantCode   string
		wantField  string
	}{
		"invalid plugin name": {
			mutate:    func(m *abi.Manifest) { m.Name = "-bad" },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.name",
		},
		"invalid version label": {
			mutate:    func(m *abi.Manifest) { m.Version = "1.0 0" },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.version",
		},
		"control char in description": {
			mutate:    func(m *abi.Manifest) { m.Description = "bad\ndesc" },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.description",
		},
		"too many origins": {
			mutate:    func(m *abi.Manifest) { m.Origins = seventeenOrigins },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.origins",
		},
		"duplicate origin": {
			mutate:    func(m *abi.Manifest) { m.Origins = []string{"https://api.example.test", "https://api.example.test"} },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.origins[1]",
		},
		"unconfined guardrail refused": {
			unconfined: true,
			mutate:     func(m *abi.Manifest) { m.Guardrail = true },
			wantCode:   CodeManifestInvalid,
			wantField:  "manifest.guardrail",
		},
		"non-guardrail requires at least one profile": {
			mutate:    func(m *abi.Manifest) { m.Profiles = nil },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.profiles",
		},
		"confined carries_traffic refused": {
			mutate:    func(m *abi.Manifest) { m.Profiles[0].CarriesTraffic = true },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.profiles[0].carries_traffic",
		},
		"duplicate profile id": {
			mutate:    func(m *abi.Manifest) { m.Profiles = append(m.Profiles, m.Profiles[0]) },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.profiles[1].id",
		},
		"unknown dialect": {
			mutate:    func(m *abi.Manifest) { m.Profiles[0].Dialect = "custom-wire" },
			wantCode:  CodeDialectUnknown,
			wantField: "manifest.profiles[0].dialect",
		},
		"hosting address outside declared origins": {
			mutate:    func(m *abi.Manifest) { m.Profiles[0].Hosting.Address = "https://other.example.test/v1" },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.profiles[0].hosting.address",
		},
		"description over 500 characters": {
			mutate:    func(m *abi.Manifest) { m.Description = strings.Repeat("a", 501) },
			wantCode:  CodeManifestInvalid,
			wantField: "manifest.description",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m := sampleManifest()
			tc.mutate(&m)
			var got *Error
			err := validateManifest(m, tc.unconfined)
			if !errors.As(err, &got) || got.Code != tc.wantCode || got.Field != tc.wantField {
				t.Fatalf("validateManifest = %v, want code=%s field=%s", err, tc.wantCode, tc.wantField)
			}
		})
	}
}

func TestCanonicalOrigin(t *testing.T) {
	t.Parallel()
	accepted := []string{
		"https://api.example.test",
		"http://api.example.test",
		"https://api.example.test:8443",
		"http://api.example.test:8080",
	}
	for _, origin := range accepted {
		if !canonicalOrigin(origin) {
			t.Errorf("canonicalOrigin(%q) = false, want true", origin)
		}
	}

	rejected := []string{
		"",
		"ftp://api.example.test",
		"https://API.example.test",
		"https://api.example.test/",
		"https://api.example.test/v1",
		"https://api.example.test?a=1",
		"https://user:pass@api.example.test",
		"https://api.example.test:443",
		"http://api.example.test:80",
		"https://api.example.test:0443",
		"https://api.example.test:0",
		"https://api.example.test:70000",
	}
	for _, origin := range rejected {
		if canonicalOrigin(origin) {
			t.Errorf("canonicalOrigin(%q) = true, want false", origin)
		}
	}
}
