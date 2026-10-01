//go:build integration

package integration_test

import (
	"testing"
	"time"
)

func TestAPIKeyPolicyEditsPreserveExpiry(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	future := now.Add(time.Hour + 123456*time.Microsecond)
	past := now.Add(-time.Hour + 654321*time.Microsecond)
	for _, tc := range []struct {
		name         string
		expires      *time.Time
		policyExpiry *time.Time
		missing      bool
	}{
		{"future expiry missing from policy", &future, nil, true},
		{"future expiry null in policy", &future, nil, false},
		{"future expiry stale in policy", &future, &past, false},
		{"expired key missing from policy", &past, nil, true},
		{"expired key stale in policy", &past, &future, false},
		{"no expiry with stale policy", nil, &future, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			owner := h.owner()
			created := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": tc.name}, idem("create-key"), 201)
			id := created["id"].(string)
			// The expiry column controls authorization and the management detail.
			// Policy JSON must not replace it when the patch omits expires_at.
			if _, err := h.Pool.Exec(t.Context(), `UPDATE olp.api_keys SET expires_at=$2,
				policy=CASE WHEN $3 THEN policy-'expires_at' ELSE jsonb_set(policy,'{expires_at}',COALESCE(to_jsonb($4::timestamptz),'null'::jsonb)) END
				WHERE id=$1`, id, tc.expires, tc.missing, tc.policyExpiry); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/api-keys/" + id
			before := h.want(owner, "GET", path, nil, nil, 200)
			h.want(owner, "PATCH", path, map[string]any{"name": "edited policy", "requests_per_minute": 7}, etagHeader(before), 200)
			after := h.want(owner, "GET", path, nil, nil, 200)
			if after["name"] != "edited policy" || after["requests_per_minute"] != float64(7) {
				t.Fatalf("policy changes were not applied: %v", after)
			}
			if after["expires_at"] != before["expires_at"] {
				t.Fatalf("omitted expiry changed: got %v, want %v", after["expires_at"], before["expires_at"])
			}
			var stored, policy *time.Time
			if err := h.Pool.QueryRow(t.Context(), `SELECT expires_at,(policy->>'expires_at')::timestamptz FROM olp.api_keys WHERE id=$1`, id).Scan(&stored, &policy); err != nil {
				t.Fatal(err)
			}
			for _, got := range []*time.Time{stored, policy} {
				if (got == nil) != (tc.expires == nil) || got != nil && !got.Equal(*tc.expires) {
					t.Fatalf("stored expiry lost precision or changed: got %v, want %v", got, tc.expires)
				}
			}
			authority, err := h.authority(created["secret"].(string))
			if err != nil {
				t.Fatal(err)
			}
			wantAllowed := tc.expires == nil || tc.expires.After(time.Now())
			if authority.Allows("inference", "", nil, time.Now()) != wantAllowed {
				t.Fatal("policy edit changed the key's expiration authority")
			}
		})
	}
}

func TestAPIKeyExpiryPatchesRemainExplicit(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	created := h.want(owner, "POST", "/api/v1/api-keys", map[string]any{"name": "expiry patches"}, idem("create-key"), 201)
	path := "/api/v1/api-keys/" + created["id"].(string)
	detail := h.want(owner, "GET", path, nil, nil, 200)
	future := time.Now().UTC().Truncate(time.Second).Add(time.Hour + 123456*time.Microsecond)
	h.want(owner, "PATCH", path, map[string]any{"expires_at": future}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	got, err := time.Parse(time.RFC3339Nano, detail["expires_at"].(string))
	if err != nil || !got.Equal(future) {
		t.Fatalf("explicit future expiry was not retained exactly: %v", detail["expires_at"])
	}
	h.want(owner, "PATCH", path, map[string]any{"expires_at": time.Now().Add(-time.Hour)}, etagHeader(detail), 422)
	afterRejected := h.want(owner, "GET", path, nil, nil, 200)
	if afterRejected["expires_at"] != detail["expires_at"] || afterRejected["etag"] != detail["etag"] {
		t.Fatal("rejected expiry changed the key")
	}
	h.want(owner, "PATCH", path, map[string]any{"expires_at": nil}, etagHeader(detail), 200)
	detail = h.want(owner, "GET", path, nil, nil, 200)
	if detail["expires_at"] != nil {
		t.Fatalf("explicit null did not clear expiry: %v", detail["expires_at"])
	}
}
