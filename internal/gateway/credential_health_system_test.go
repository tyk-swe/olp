//go:build integration

package gateway

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/runtime"
)

type grantGenerationRuntime struct {
	*fakeRuntime
	generation int64
}

func (r *grantGenerationRuntime) GrantGeneration(string) int64 { return r.generation }

// One replica may still be recording a refusal of the old token after another
// has polled its replacement. A shared cooldown must follow the refused token.
func TestIntegrationGrantCooldownsFollowGenerationsAcrossGateways(t *testing.T) {
	limiter := mediaLimiter(t)
	log := slog.New(slog.DiscardHandler)
	provider, credential := uuid.NewString(), uuid.NewString()
	slot := &runtime.Slot{ID: uuid.NewString(), CredentialID: &credential}
	makeGateway := func(generation int64) *Server {
		return &Server{
			Runtime: &grantGenerationRuntime{fakeRuntime: &fakeRuntime{}, generation: generation},
			health:  newHealthTracker(time.Now), Admission: NewAdmission(limiter, nil, log),
		}
	}
	old, replacement := makeGateway(1), makeGateway(2)
	refused := &attemptFailure{class: classCredential, status: http.StatusUnauthorized, dispatched: true}
	old.cooldownFailure(t.Context(), provider, slot, 1, refused)
	if !old.cooling(t.Context(), provider, slot) || replacement.cooling(t.Context(), provider, slot) {
		t.Fatal("the old replica's refusal cooled the replacement token")
	}
	// A refusal of the replacement is effective, and a delayed refresh
	// notification only clears the superseded generation's cooldown.
	replacement.cooldownFailure(t.Context(), provider, slot, 2, refused)
	replacement.GrantRefreshed(provider, credential)
	if !replacement.cooling(t.Context(), provider, slot) {
		t.Fatal("a delayed refresh notification cleared the new token's refusal")
	}
}
