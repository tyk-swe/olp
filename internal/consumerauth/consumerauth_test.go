package consumerauth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
)

type authenticatorFunc func(string) (*keyauthority.Authority, error)

func (f authenticatorFunc) Authenticate(secret string) (*keyauthority.Authority, error) {
	return f(secret)
}

func TestPausedKeyRefusalYieldsToRevocation(t *testing.T) {
	now := time.Now()
	key := func(revoked, paused *time.Time) *keyauthority.Authority {
		return &keyauthority.Authority{ID: "key", RevokedAt: revoked, PausedAt: paused, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}}
	}
	for _, test := range []struct {
		name      string
		authority *keyauthority.Authority
		err       error
		status    int
		code      string
	}{
		{name: "active key", authority: key(nil, nil)},
		{name: "paused key", authority: key(nil, &now), status: http.StatusForbidden, code: "api_key_paused"},
		{name: "paused delegating agent", err: release.ErrPausedKey, status: http.StatusForbidden, code: "api_key_paused"},
		{name: "revoked while paused", authority: key(&now, &now), status: http.StatusUnauthorized, code: "invalid_api_key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := authenticatorFunc(func(string) (*keyauthority.Authority, error) { return test.authority, test.err })
			_, failure := Authenticate(auth, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), "olp_key_secret", "inference", nil, now)
			if test.status == 0 && failure != nil {
				t.Fatalf("active key refused: %+v", failure)
			}
			if test.status != 0 && (failure == nil || failure.Status != test.status || failure.Code != test.code) {
				t.Fatalf("failure = %+v, want %d %s", failure, test.status, test.code)
			}
		})
	}
}
