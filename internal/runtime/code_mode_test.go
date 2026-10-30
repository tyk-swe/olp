package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"fmt"
	"github.com/jackc/pgx/v5"
	"slices"
	"testing"
	"time"

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

type codeSnapshotPublication struct {
	route    codemode.Route
	provider routing.Provider
}

type codeSnapshotTx struct {
	pgx.Tx
	publications   []codeSnapshotPublication
	publicationErr error
}

func (tx codeSnapshotTx) Query(_ context.Context, query string, _ ...any) (pgx.Rows, error) {
	if !strings.Contains(query, "olp.code_routes") {
		return &codeSnapshotRows{}, nil
	}
	if tx.publicationErr != nil {
		return nil, tx.publicationErr
	}
	return &codeSnapshotRows{publications: tx.publications}, nil
}

func (tx codeSnapshotTx) QueryRow(context.Context, string, ...any) pgx.Row {
	return codeSnapshotSettingsRow{}
}

type codeSnapshotSettingsRow struct{}

func (codeSnapshotSettingsRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("unexpected fixture row width %d", len(dest))
	}
	switch value := dest[0].(type) {
	case *bool:
		*value = false
	case *string:
		*value = "4096"
	default:
		return fmt.Errorf("unexpected fixture destination %T", dest[0])
	}

	return nil
}

type codeSnapshotRows struct {
	pgx.Rows
	publications []codeSnapshotPublication
	position     int
}

func (r *codeSnapshotRows) Next() bool {
	if r.position >= len(r.publications) {
		return false
	}
	r.position++
	return true
}
func (r *codeSnapshotRows) Close()     {}
func (r *codeSnapshotRows) Err() error { return nil }
func (r *codeSnapshotRows) Scan(dest ...any) error {
	publication := r.publications[r.position-1]
	document, err := json.Marshal(publication.route)
	if err != nil {
		return err
	}
	paid, err := json.Marshal(map[string]routing.Provider{publication.provider.ID: publication.provider})
	if err != nil {
		return err
	}
	values := []any{publication.route.Slug, document, []byte(`{}`), paid}
	if len(dest) != len(values) {
		return fmt.Errorf("unexpected fixture row width %d", len(dest))
	}
	for i, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, dest[i]); err != nil {
			return err
		}
	}
	return nil
}

func TestReadSnapshotPreservesPaidTargetPerPublication(t *testing.T) {
	project, providerID, slotID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	publications := slices.Collect(func(yield func(codeSnapshotPublication) bool) {
		for _, slug := range []string{"coding-first", "coding-second"} {
			route := codemode.Route{ID: uuid.NewString(), ProjectID: project, Slug: slug, PoolID: uuid.NewString(), Models: []string{"native-model"}, Enabled: true, RevisionID: uuid.NewString(), Revision: 1, PublishedAt: new(time.Now().UTC()), PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{"native-model": {ProviderID: providerID, SlotID: slotID}}}
			provider := routing.Provider{ID: providerID, RevisionID: uuid.NewString(), ProjectID: &project, Name: slug, Kind: "openai", Enabled: true, Endpoint: "https://" + slug + ".example/v1", AuthMode: "api_key", ProfileID: "openai-responses", ProfileRevision: connectors.ProfileRevision, VendorID: "openai", Slots: []routing.Slot{{ID: slotID, Enabled: true, CredentialID: new(uuid.NewString()), CredentialVersion: new(1), AllowedModels: route.Models}}, Capabilities: []routing.Capability{{Model: "native-model", Operation: "generation", Surface: "openai", Mode: "unary"}}}
			if !yield(codeSnapshotPublication{route: route, provider: provider}) {
				return
			}
		}
	})
	snapshot, err := ReadSnapshot(t.Context(), codeSnapshotTx{publications: publications})
	if err != nil {
		t.Fatal(err)
	}
	for _, publication := range publications {
		route := snapshot.CodeRoutes[publication.route.Slug]
		selected := snapshot.CodePaidConnections[route.RevisionID+":"+route.PaidTargets["native-model"].ProviderID]
		if selected.RevisionID != publication.provider.RevisionID || selected.Endpoint != publication.provider.Endpoint || selected.VendorID != publication.provider.VendorID || selected.ProjectID == nil || *selected.ProjectID != project {
			t.Fatalf("paid target lost publication ownership %+v", selected)
		}
		if len(selected.Slots) != 1 {
			t.Fatalf("paid target lost selected slot %+v", selected.Slots)
		}
		slot := selected.Slots[0]
		if slot.ID != slotID || slot.CredentialID == nil || *slot.CredentialID != *publication.provider.Slots[0].CredentialID || slot.CredentialVersion == nil || *slot.CredentialVersion != *publication.provider.Slots[0].CredentialVersion {
			t.Fatalf("paid target lost frozen credential authority %+v", slot)
		}
		if protocol, ok := routing.CodePaidProtocol(selected, "native-model"); !ok || protocol != codemode.ProtocolResponses {
			t.Fatalf("published paid target is not natively usable %q %v", protocol, ok)
		}
	}
}

func TestReadSnapshotRefusesUnavailableCodePublication(t *testing.T) {
	unavailable := errors.New("publication unavailable")
	_, err := ReadSnapshot(t.Context(), codeSnapshotTx{publicationErr: unavailable})
	if !errors.Is(err, unavailable) {
		t.Fatalf("publication read failure was lost %v", err)
	}
}
