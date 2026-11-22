package release

import (
	"errors"
	"testing"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

func TestPausedKeyRefusesCurrentAndOverlapSecrets(t *testing.T) {
	now := time.Now()
	overlapUntil := now.Add(time.Hour)
	auth := secrets.NewAuthKey(make([]byte, 32), "installation")
	record := func(lookup, secret string, expiresAt *time.Time) *keyRecord {
		return &keyRecord{
			authority: keyauthority.Authority{ID: "key", LookupID: lookup, Policy: keyauthority.KeyPolicy{Scopes: []string{"inference"}}, PausedAt: &now},
			digest:    auth.Digest(secrets.APIKeyDigest, secret),
			expiresAt: expiresAt,
		}
	}
	replica := &Manager{auth: auth, authority: authorityState{loaded: true, readAt: now, keys: map[string]*keyRecord{
		"cur": record("cur", "olp_cur_currentsecret", nil),
		"old": record("old", "olp_old_overlapsecret", &overlapUntil),
	}}}
	for _, secret := range []string{"olp_cur_currentsecret", "olp_old_overlapsecret"} {
		if _, err := replica.Authenticate(secret); !errors.Is(err, ErrPausedKey) {
			t.Fatalf("paused key accepted %s with %v, want paused refusal", secret, err)
		}
	}
	if _, err := replica.Authenticate("olp_cur_wrongsecret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("wrong secret for paused key error = %v, want invalid key", err)
	}
}
