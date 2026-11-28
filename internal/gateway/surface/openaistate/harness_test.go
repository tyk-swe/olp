package openaistate

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/fault"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/staging"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type publishedRuntime struct{ published *release.Release }

func (r publishedRuntime) Release() *release.Release                             { return r.published }
func (publishedRuntime) RoutingInputs() *routinginputs.Inputs                    { return nil }
func (publishedRuntime) Authenticate(string) (*keyauthority.Authority, error)    { return nil, nil }
func (publishedRuntime) CheckRouteFidelity(context.Context, routing.Route) error { return nil }
func (publishedRuntime) GrantGeneration(string) int64                            { return 0 }
func (publishedRuntime) CredentialRefused(string, int64)                         {}
func (publishedRuntime) Eligibility(string) routing.Eligibility                  { return routing.Eligible }

func (publishedRuntime) Secret(context.Context, *release.Release, string) ([]byte, int64, error) {
	return []byte("upstream-secret"), 0, nil
}

func (publishedRuntime) NetworkSecret(context.Context, *release.Release, string, string) ([]byte, error) {
	return nil, release.ErrCredentialUnavailable
}

type envelopes chan events.Envelope

func (e envelopes) Terminal(envelope events.Envelope) { e <- envelope }

type pinnedProvider struct {
	surface  surface
	pin      *kernel.Pin
	finished envelopes
}

func newPinnedProvider(t *testing.T, spool *staging.Spool, answer string) pinnedProvider {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(upstream.Close)
	credential, version := uuid.NewString(), 1
	provider := routing.Provider{
		ID: uuid.NewString(), Name: "pinned", Kind: "openai_compatible", Enabled: true, ActiveCredential: &credential, RevisionID: uuid.NewString(),
		Endpoint: upstream.URL + "/v1", AuthMode: "api_key",
		Slots: []routing.Slot{{ID: uuid.NewString(), Name: "default", Enabled: true, Weight: 1, CredentialID: &credential, CredentialVersion: &version}},
	}
	published, err := release.NewRelease(uuid.NewString(), 1, &routing.Snapshot{
		Generation: routing.Generation{ID: uuid.NewString(), Ordinal: 1, ActivatedAt: time.Now()},
		Providers:  map[string]routing.Provider{provider.ID: provider},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(envelopes, 1)
	server := kernel.New(kernel.Dependencies{
		Runtime: publishedRuntime{published: published},
		Egress:  &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}},
		Log:     slog.New(slog.DiscardHandler),
		Sink:    finished,
		Getenv:  func(string) string { return "" },
	}, kernel.Config{MaxResponseBytes: 4 << 20})
	target := routing.Target{ID: uuid.NewString(), ProviderID: provider.ID, ProviderModel: "gemini-model", Weight: 1, Timeout: 2000, RoutingID: uuid.NewString()}
	return pinnedProvider{
		surface:  surface{kernel: server, spool: spool},
		pin:      &kernel.Pin{Provider: provider, Target: target, Model: target.ProviderModel, Slot: provider.Slots[0]},
		finished: finished,
	}
}

func (p pinnedProvider) onlyAttempt(t *testing.T, operation func(context.Context, http.ResponseWriter, *kernel.Call) *fault.Error) (*fault.Error, events.AttemptFact) {
	t.Helper()
	recorder := httptest.NewRecorder()
	x := p.surface.kernel.SessionCall(recorder, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/files/file-local", nil), wire.File, "unary")
	failure := operation(t.Context(), recorder, x)
	if failure == nil {
		t.Fatal("expected the file operation to fail")
	}
	p.surface.kernel.Refuse(x, recorder, failure)
	envelope := <-p.finished
	if len(envelope.Attempts) != 1 {
		t.Fatalf("attempts = %+v", envelope.Attempts)
	}
	return failure, envelope.Attempts[0]
}
