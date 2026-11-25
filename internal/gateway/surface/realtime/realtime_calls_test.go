package realtime

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/iotest"
	"time"

	"github.com/tyk-swe/olp/internal/gateway/kernel"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage/routinginputs"
)

func TestParseWebRTCCallRequestRequiresCompleteMultipart(t *testing.T) {
	t.Parallel()
	const sdp = "v=0\r\n"
	const session = `{"type":"realtime"}`
	body, contentType, err := buildWebRTCCallsBody([]byte(sdp), []byte(session))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		body  io.Reader
		valid bool
	}{
		{"clean end of form", bytes.NewReader(body), true},
		{"EOF before any multipart data", iotest.ErrReader(io.EOF), false},
		{"body read failure", iotest.ErrReader(io.ErrClosedPipe), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/realtime/calls", test.body)
			request.Header.Set("Content-Type", contentType)
			parsed, failure := parseWebRTCCallRequest(request)
			if !test.valid {
				if parsed != nil || failure == nil || failure.Status != http.StatusBadRequest || failure.Code != "invalid_value" || failure.Param != nil {
					t.Fatalf("got request %+v and error %+v, want a form-level bad request", parsed, failure)
				}
				return
			}
			if failure != nil || parsed == nil || string(parsed.sdp) != sdp || string(parsed.session) != session {
				t.Fatalf("got request %+v and error %+v, want both form parts unchanged", parsed, failure)
			}
		})
	}
}

type realtimeTestRuntime struct {
	kernel.Runtime
	authority keyauthority.Authority
}

func (r realtimeTestRuntime) Authenticate(string) (*keyauthority.Authority, error) {
	return &r.authority, nil
}

func (r realtimeTestRuntime) RoutingInputs() *routinginputs.Inputs { return nil }

func TestRealtimeReauthorizationKeepsAdmittedBudget(t *testing.T) {
	t.Parallel()
	authority := keyauthority.Authority{
		ID: "key",
		Policy: keyauthority.KeyPolicy{
			Scopes:         []string{"inference"},
			DailyCostLimit: new("5"),
		},
	}
	for _, tc := range []struct {
		name string
		now  keyauthority.Authority
		want error
	}{
		{"unchanged budget", authority, nil},
		{"revoked key", keyauthority.Authority{ID: authority.ID, Policy: authority.Policy, RevokedAt: new(time.Now())}, errRealtimeAuthorityRevoked},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &kernel.Server{Deps: kernel.Dependencies{Runtime: realtimeTestRuntime{authority: tc.now}}, Now: time.Now}
			x := &kernel.Call{}
			x.Route = &routing.Route{Slug: "realtime"}
			server.BindAuthority(x, authority)
			st := relayState{surface: surface{kernel: server}, x: x, p: &kernel.Pin{}, keyID: authority.ID}
			if reason, err := st.reauthorize(t.Context()); !errors.Is(err, tc.want) {
				t.Fatalf("reauthorize = %q, %v, want %v", reason, err, tc.want)
			}
		})
	}
}
