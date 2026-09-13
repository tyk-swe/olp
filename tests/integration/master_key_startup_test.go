//go:build integration

package integration_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

func TestStartupAuthenticatesEveryStoredSecret(t *testing.T) {
	parse := func(t *testing.T, active int, key1, key2 string) *secrets.KeyRing {
		t.Helper()
		var entries []string
		for i, key := range []string{key1, key2} {
			if key != "" {
				entries = append(entries, fmt.Sprintf(`{"version":%d,"key":%q}`, i+1, strings.Repeat(key, 32)))
			}
		}
		ring, err := secrets.ParseRing([]byte(fmt.Sprintf(`{"active_version":%d,"keys":[%s]}`, active, strings.Join(entries, ","))))
		if err != nil {
			t.Fatal(err)
		}
		return ring
	}
	for _, tc := range []struct {
		name, key1, key2 string
		mixed, corrupt   bool
		wantError        bool
	}{
		{name: "matching keys", key1: "ab", key2: "ef"},
		{name: "replaced active key", key1: "ab", key2: "01", wantError: true},
		{name: "replaced old key", key1: "01", key2: "ef", wantError: true},
		{name: "missing old key", key2: "ef", wantError: true},
		{name: "mixed material under one version", key1: "ab", key2: "ef", mixed: true, wantError: true},
		{name: "tampered expired record", key1: "ab", key2: "ef", corrupt: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			current := parse(t, 2, "ab", "ef")
			if _, err := current.Rotate(t.Context(), h.Pool, h.Server.Installation); err != nil {
				t.Fatal(err)
			}
			// Model an interrupted rotation: current and old versions coexist.
			for i, purpose := range []string{"oidc_client", "oidc_flow", "mutation_replay"} {
				ring := current
				if i == 2 {
					ring = h.Server.Keys
				} else if i == 1 && tc.mixed {
					ring = parse(t, 2, "ab", "01")
				}
				id := uuid.NewString()
				ciphertext, err := ring.Seal(h.Server.Installation, purpose, id, []byte("private stored value"))
				if err != nil {
					t.Fatal(err)
				}
				var expires *time.Time
				if i == 2 {
					expired := time.Now().Add(-time.Hour)
					expires = &expired
					if tc.corrupt {
						ciphertext[len(ciphertext)-1] ^= 1
					}
				}
				if _, err = h.Pool.Exec(t.Context(), "INSERT INTO olp_go.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,$2,$3,$4,$5)", id, purpose, ring.Active, ciphertext, expires); err != nil {
					t.Fatal(err)
				}
			}
			var before string
			if err := h.Pool.QueryRow(t.Context(), "SELECT jsonb_agg(s ORDER BY id)::text FROM olp_go.secrets s").Scan(&before); err != nil {
				t.Fatal(err)
			}
			server, err := access.New(t.Context(), h.Pool, h.Server.Installation, h.Server.Origin, h.Server.Auth, parse(t, 2, tc.key1, tc.key2), h.Bootstrap)
			if (err != nil) != tc.wantError || (tc.wantError && server != nil) {
				t.Fatalf("startup error=%v, wantError=%v", err, tc.wantError)
			}
			if err != nil && strings.Contains(err.Error(), "private stored value") {
				t.Fatal("startup error exposed plaintext")
			}
			var after string
			if err := h.Pool.QueryRow(t.Context(), "SELECT jsonb_agg(s ORDER BY id)::text FROM olp_go.secrets s").Scan(&after); err != nil {
				t.Fatal(err)
			}
			if before != after {
				t.Fatal("startup changed stored secrets")
			}
		})
	}
}
