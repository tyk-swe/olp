package runtime

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestCodeAdapterDerivesFromTheRevisionsFrozenConnections(t *testing.T) {
	grant := func(profile string) Configuration {
		return Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile}
	}
	s := &Snapshot{Generation: Generation{ID: uuid.NewString()}, CodeConnections: map[string]Configuration{
		"codex:p1": grant(codexauth.ProfileID), "codex:p2": grant("reference-grant-chat"),
		"zai:p3": grant(codeplans.ZAIProfile), "zai:p4": grant(codeplans.BigModelProfile),
		"mixed:p5": grant(codeplans.OpenCodeGoProfile), "mixed:p6": grant(codexauth.ProfileID),
		"other:p7": grant("reference-grant-chat"),
	}}
	digest, err := s.Digest()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Snapshot
	if err := json.Unmarshal(encoded, &loaded); err != nil {
		t.Fatal(err)
	}
	for name, snapshot := range map[string]*Snapshot{"constructed": s, "loaded": &loaded} {
		t.Run(name, func(t *testing.T) {
			release, err := NewRelease("release", 1, snapshot, nil)
			if err != nil {
				t.Fatal(err)
			}
			if release.Digest != digest {
				t.Fatal("derived adapters changed the snapshot digest")
			}
			for revision, want := range map[string]codemode.Adapter{"codex": codemode.AdapterCodex, "zai": codemode.AdapterZAICoding, "mixed": "", "other": "", "absent": ""} {
				if got := snapshot.CodeAdapter(codemode.Route{RevisionID: revision}); got != want {
					t.Fatalf("%s: %q, want %q", revision, got, want)
				}
			}
		})
	}
}

func BenchmarkCodeAdapter(b *testing.B) {
	for _, count := range []int{1, 10000, 100000} {
		b.Run(fmt.Sprintf("connections=%d", count), func(b *testing.B) {
			s := &Snapshot{Generation: Generation{ID: uuid.NewString()}, CodeConnections: make(map[string]Configuration, count)}
			for i := range count {
				s.CodeConnections[fmt.Sprintf("revision-%d:provider", i)] = Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID}
			}
			if err := s.Validate(); err != nil {
				b.Fatal(err)
			}
			route := codemode.Route{RevisionID: "revision-0"}
			if got := s.CodeAdapter(route); got != codemode.AdapterCodex {
				b.Fatalf("adapter %q, want %q", got, codemode.AdapterCodex)
			}
			b.ReportAllocs()
			for b.Loop() {
				s.CodeAdapter(route)
			}
		})
	}
}
