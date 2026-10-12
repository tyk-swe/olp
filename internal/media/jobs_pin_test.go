//go:build integration

package media

import (
	"testing"

	"github.com/google/uuid"
)

func TestIntegrationReservationRejectsMismatchedHistoricalPins(t *testing.T) {
	f := seedClaimFixture(t)
	valid := Reservation{ID: uuid.NewString(), RuntimeGenerationID: f.generationID, ProviderRevisionID: f.revisionID, APIKeyID: f.apiKeyID, ProviderID: f.providerID, UpstreamModel: claimVideoModel, RouteSlug: "video-default", Operation: OpVideoCreate, Surface: "openai", SlotID: f.slotID}
	for _, tc := range []struct {
		name   string
		change func(*Reservation)
	}{
		{"provider revision", func(r *Reservation) { r.ProviderRevisionID = uuid.NewString() }},
		{"route", func(r *Reservation) { r.RouteSlug = "different-route" }},
		{"slot", func(r *Reservation) { r.SlotID = uuid.NewString() }},
		{"credential", func(r *Reservation) { id := uuid.NewString(); r.CredentialVersionID = &id }},
		{"model", func(r *Reservation) { r.UpstreamModel = "another-model" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := valid
			input.ID = uuid.NewString()
			tc.change(&input)
			if _, err := ReserveJob(t.Context(), f.pool, input); err == nil {
				t.Fatal("mismatched historical identity reserved a non-idempotent create")
			}
			var exists bool
			if err := f.pool.QueryRow(t.Context(), "SELECT EXISTS(SELECT 1 FROM olp.media_jobs WHERE id=$1)", input.ID).Scan(&exists); err != nil || exists {
				t.Fatalf("refusal left durable work: %v %v", exists, err)
			}
		})
	}
	if _, err := ReserveJob(t.Context(), f.pool, valid); err != nil {
		t.Fatal("exact historical identity rejected", err)
	}
}
