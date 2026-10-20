package runtime

import (
	"fmt"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
)

func BenchmarkCodeProviders(b *testing.B) {
	for _, count := range []int{1, 10000, 100000} {
		b.Run(fmt.Sprintf("connections=%d", count), func(b *testing.B) {
			s := &routing.Snapshot{Generation: routing.Generation{ID: uuid.NewString()}, CodeConnections: make(map[string]routing.Configuration, count)}
			for i := range count {
				s.CodeConnections[fmt.Sprintf("revision-%d:provider", i)] = routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID}
			}
			if err := s.Compile(); err != nil {
				b.Fatal(err)
			}
			route := codemode.Route{RevisionID: "revision-0"}
			if got := s.CodeProviders(route, "responses", codeadapter.HTTP); !slices.Equal(got, []string{"provider"}) {
				b.Fatalf("providers %q", got)
			}
			b.ReportAllocs()
			for b.Loop() {
				s.CodeProviders(route, "responses", codeadapter.HTTP)
			}
		})
	}
}
