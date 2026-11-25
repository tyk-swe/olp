package media

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/protocols/sse"
	"github.com/tyk-swe/olp/internal/domain/protocols/wire"
	"github.com/tyk-swe/olp/internal/gateway/events"
	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/media"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

type stubRuntime struct {
	release.Credentials
}

func (stubRuntime) Release() *release.Release                               { return nil }
func (stubRuntime) RoutingInputs() *routinginputs.Inputs                    { return nil }
func (stubRuntime) Authenticate(string) (*keyauthority.Authority, error)    { return nil, nil }
func (stubRuntime) CheckRouteFidelity(context.Context, routing.Route) error { return nil }
func (stubRuntime) GrantGeneration(string) int64                            { return 0 }
func (stubRuntime) CredentialRefused(string, int64)                         {}

type captureSink struct {
	mu   sync.Mutex
	envs []events.Envelope
}

func (c *captureSink) Terminal(env events.Envelope) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.envs = append(c.envs, env)
}

func (c *captureSink) last(t *testing.T) events.Envelope {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.envs) == 0 {
		t.Fatal("expected a terminal envelope")
	}
	return c.envs[len(c.envs)-1]
}

func TestStoredVideoResponsesKeepFailedAttemptsUncommitted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, body string
		family             wire.Format
		facts              []events.AttemptFact
	}{
		{
			name: "list after a failed poll", method: http.MethodGet, family: wire.VideoList,
			body: `{"data":[{"id":"stored-video"}]}`,
			facts: []events.AttemptFact{
				{Class: kernel.ClassSuccess, Status: http.StatusOK, Committed: true},
				{Class: kernel.ClassUpstreamServer, Status: http.StatusServiceUnavailable},
			},
		},
		{
			name: "transformed delete after upstream 404", method: http.MethodDelete, family: wire.VideoDelete,
			body:  `{"id":"stored-video","deleted":true}`,
			facts: []events.AttemptFact{{Class: kernel.ClassUpstreamClient, Status: http.StatusNotFound}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := &captureSink{}
			gw := kernel.New(kernel.Dependencies{
				Runtime: stubRuntime{},
				Egress:  &egress.Policy{},
				Log:     slog.New(slog.DiscardHandler),
				Sink:    sink,
				Getenv:  func(string) string { return "" },
			}, kernel.Config{})
			s := surface{kernel: gw}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				x := gw.SessionCall(w, r, tc.family, "unary")
				for _, fact := range tc.facts {
					x.NoteAttempt(fact)
				}
				delivered := s.deliverMediaJSON(w, x, http.StatusOK, false, []byte(tc.body))
				gw.Finish(x, &delivered.outcome, delivered.status)
			}))
			defer server.Close()
			req, err := http.NewRequestWithContext(t.Context(), tc.method, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK || string(body) != tc.body {
				t.Fatalf("response = %d, %s, %v", response.StatusCode, body, err)
			}
			env := sink.last(t)
			if !env.Committed || len(env.Attempts) != len(tc.facts) {
				t.Fatalf("response committed = %v, attempts = %+v", env.Committed, env.Attempts)
			}
			for i, want := range tc.facts {
				if got := env.Attempts[i]; got.Committed != want.Committed || got.Status != want.Status || got.Class != want.Class {
					t.Fatalf("attempt %d changed after delivery: %+v", i, got)
				}
			}
		})
	}
}

func TestFormatSSEFrameAndApplyObservedUsage(t *testing.T) {
	t.Parallel()
	id := "evt-1"
	retry := uint64(500)
	event := "speech.audio.delta"
	frame := sse.Frame{ID: &id, RetryMS: &retry, Event: &event, Data: "line1\nline2"}

	if got, want := string(formatSSEFrame(frame, false)), "event: speech.audio.delta\ndata: line1\ndata: line2\n\n"; got != want {
		t.Fatalf("non-strict frame = %q, want %q", got, want)
	}
	if got, want := string(formatSSEFrame(frame, true)), "id: evt-1\nretry: 500\nevent: speech.audio.delta\ndata: line1\ndata: line2\n\n"; got != want {
		t.Fatalf("strict frame = %q, want %q", got, want)
	}

	in, out, cached := int64(3), int64(9), int64(1)
	u := applyObservedUsage(nil, &media.MediaUsage{Input: &in, Output: &out, Cached: &cached})
	if u == nil || u.InputTokens != 3 || u.OutputTokens != 9 || u.TotalTokens != 12 || u.CachedInputTokens == nil || *u.CachedInputTokens != 1 {
		t.Fatalf("unexpected usage: %+v", u)
	}
	units := "2"
	withUnits := applyTerminalUnits(u, &units)
	if withUnits.MediaUnits == nil || *withUnits.MediaUnits != "2" || u.MediaUnits != nil {
		t.Fatalf("applyTerminalUnits did not preserve immutability: got %+v, original %+v", withUnits, u)
	}
}
