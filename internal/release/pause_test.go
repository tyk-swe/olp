package release

import (
	"errors"
	"testing"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type pauseFixture struct {
	auth    *secrets.AuthKey
	now     time.Time
	paused  time.Time
	revoked time.Time
}

func (f pauseFixture) record(lookup, secret string, pausedAt, revokedAt, expiresAt *time.Time) *keyRecord {
	return &keyRecord{
		authority: keyauthority.Authority{
			ID: "key", LookupID: lookup,
			Policy:    keyauthority.KeyPolicy{Scopes: []string{"inference"}},
			PausedAt:  pausedAt,
			RevokedAt: revokedAt,
		},
		digest:    f.auth.Digest(secrets.APIKeyDigest, secret),
		expiresAt: expiresAt,
	}
}

func (f pauseFixture) replica(readAt time.Time, records map[string]*keyRecord) *Manager {
	return &Manager{auth: f.auth, authority: authorityState{loaded: true, readAt: readAt, keys: records}}
}

func (f pauseFixture) currentAndOverlap(pausedAt *time.Time, overlapUntil time.Time) map[string]*keyRecord {
	return map[string]*keyRecord{
		"cur": f.record("cur", "olp_cur_currentsecret", pausedAt, nil, nil),
		"old": f.record("old", "olp_old_overlapsecret", pausedAt, nil, &overlapUntil),
	}
}

func TestPausedKeyReplicaRollout(t *testing.T) {
	now := time.Now()
	fixture := pauseFixture{auth: secrets.NewAuthKey(make([]byte, 32), "installation"), now: now, paused: now, revoked: now}
	overlapUntil := now.Add(time.Hour)

	refreshed := fixture.replica(now, fixture.currentAndOverlap(&fixture.paused, overlapUntil))
	for _, secret := range []string{"olp_cur_currentsecret", "olp_old_overlapsecret"} {
		if _, err := refreshed.Authenticate(secret); !errors.Is(err, ErrPausedKey) {
			t.Fatalf("refreshed replica accepted %s with %v, want paused refusal", secret, err)
		}
	}

	lagging := fixture.replica(now, fixture.currentAndOverlap(nil, overlapUntil))
	for _, secret := range []string{"olp_cur_currentsecret", "olp_old_overlapsecret"} {
		authority, err := lagging.Authenticate(secret)
		if err != nil || authority == nil {
			t.Fatalf("lagging fresh replica refused %s with %v", secret, err)
		}
		if !authority.Allows("inference", "route", nil, now) {
			t.Fatalf("lagging fresh replica did not allow %s", secret)
		}
	}

	stale := fixture.replica(now.Add(-AuthorityStaleAfter-time.Second), fixture.currentAndOverlap(nil, overlapUntil))
	if _, err := stale.Authenticate("olp_cur_currentsecret"); !errors.Is(err, ErrStaleAuthority) {
		t.Fatalf("stale lagging replica error = %v, want stale authority", err)
	}
}

func TestPausedKeyRotationAndResume(t *testing.T) {
	now := time.Now()
	fixture := pauseFixture{auth: secrets.NewAuthKey(make([]byte, 32), "installation"), now: now, paused: now, revoked: now}

	rotatedPaused := map[string]*keyRecord{
		"new": fixture.record("new", "olp_new_newsecret", &fixture.paused, nil, nil),
	}
	replica := fixture.replica(now, rotatedPaused)
	if _, err := replica.Authenticate("olp_new_newsecret"); !errors.Is(err, ErrPausedKey) {
		t.Fatalf("rotated secret while paused error = %v, want paused refusal", err)
	}
	if _, err := replica.Authenticate("olp_cur_currentsecret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("zero-overlap previous secret error = %v, want invalid key", err)
	}

	resumed := fixture.replica(now, map[string]*keyRecord{
		"new": fixture.record("new", "olp_new_newsecret", nil, nil, nil),
	})
	if _, err := resumed.Authenticate("olp_new_newsecret"); err != nil {
		t.Fatalf("resumed new secret refused with %v", err)
	}
	if _, err := resumed.Authenticate("olp_cur_currentsecret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("resumed previous secret error = %v, want invalid key", err)
	}
}

func TestResumeNeverRestoresExpiredOverlapOrRevokedKey(t *testing.T) {
	now := time.Now()
	fixture := pauseFixture{auth: secrets.NewAuthKey(make([]byte, 32), "installation"), now: now, paused: now, revoked: now}
	expiredOverlap := now.Add(-time.Minute)

	resumed := fixture.replica(now, fixture.currentAndOverlap(nil, expiredOverlap))
	if _, err := resumed.Authenticate("olp_old_overlapsecret"); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("expired overlap after resume error = %v, want invalid key", err)
	}
	if _, err := resumed.Authenticate("olp_cur_currentsecret"); err != nil {
		t.Fatalf("current secret after resume refused with %v", err)
	}

	revoked := fixture.record("cur", "olp_cur_currentsecret", nil, &fixture.revoked, nil)
	if revoked.authority.Allows("inference", "route", nil, now) {
		t.Fatal("revoked key allowed after pause was cleared")
	}
}
