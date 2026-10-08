package runtime

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codeplans"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
)

func TestCodeAdaptersAndProvidersDeriveFromTheRevisionsFrozenConnections(t *testing.T) {
	grant := func(profile string) Configuration {
		return Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: profile}
	}
	s := &Snapshot{Generation: Generation{ID: uuid.NewString()}, CodeConnections: map[string]Configuration{
		"codex:p1": grant(codexauth.ProfileID), "codex:p2": grant("reference-grant-chat"),
		"zai:p3": grant(codeplans.ZAIProfile), "zai:p4": grant(codeplans.BigModelProfile),
		"mixed:p7": grant(codeplans.ZAIProfile), "mixed:p5": grant(codeplans.OpenCodeGoProfile), "mixed:p6": grant(codexauth.ProfileID),
		"other:p8": grant("reference-grant-chat"),
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
			for revision, want := range map[string][]codemode.Adapter{
				"codex": {codemode.AdapterCodex}, "zai": {codemode.AdapterZAICoding},
				"mixed": {codemode.AdapterCodex, codemode.AdapterOpenCodeGo, codemode.AdapterZAICoding}, "other": nil, "absent": nil,
			} {
				if got := snapshot.CodeAdapters(codemode.Route{RevisionID: revision}); !slices.Equal(got, want) {
					t.Fatalf("%s: %q, want %q", revision, got, want)
				}
			}
			for path, want := range map[string][]string{
				"responses": {"p6"}, "v1/messages": {"p5", "p7"}, "v1/chat/completions": {"p5", "p7"}, "v1/responses": {"p5"}, "v1/models": nil,
			} {
				if got := snapshot.CodeProviders(codemode.Route{RevisionID: "mixed"}, path); !slices.Equal(got, want) {
					t.Fatalf("%s: %q, want %q", path, got, want)
				}
			}
			if got := snapshot.CodeProviders(codemode.Route{RevisionID: "zai"}, "v1/messages"); !slices.Equal(got, []string{"p3", "p4"}) {
				t.Fatalf("zai providers %q", got)
			}
			for _, test := range []struct {
				path     string
				protocol codemode.Protocol
				model    string
				want     []string
			}{
				{"v1/messages", codemode.ProtocolMessages, "minimax-m3", []string{"p5", "p7"}},
				{"v1/messages", codemode.ProtocolMessages, "glm-5.3", []string{"p7"}},
				{"v1/chat/completions", codemode.ProtocolChat, "glm-5.3", []string{"p5", "p7"}},
				{"v1/responses", codemode.ProtocolResponses, "gpt-5.5", []string{"p5"}},
				{"v1/responses", codemode.ProtocolResponses, "glm-5.3", nil},
				{"responses", codemode.ProtocolResponses, "glm-5.3", []string{"p6"}},
			} {
				if got := snapshot.CodeModelProviders(codemode.Route{RevisionID: "mixed"}, test.path, test.protocol, test.model); !slices.Equal(got, test.want) {
					t.Fatalf("%s %s: %q, want %q", test.path, test.model, got, test.want)
				}
			}
			if got := snapshot.CodeProviders(codemode.Route{RevisionID: "mixed"}, "v1/messages"); !slices.Equal(got, []string{"p5", "p7"}) {
				t.Fatalf("model filtering changed the shared providers: %q", got)
			}
		})
	}
}

func BenchmarkCodeProviders(b *testing.B) {
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
			if got := s.CodeProviders(route, "responses"); !slices.Equal(got, []string{"provider"}) {
				b.Fatalf("providers %q", got)
			}
			b.ReportAllocs()
			for b.Loop() {
				s.CodeProviders(route, "responses")
			}
		})
	}
}
