//go:build integration

package integration_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/secrets"
)

func TestRotationAuthenticatesEveryDestinationSecretBeforeWriting(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		active, remaining     int
		replace, mix, corrupt bool
	}{
		{name: "replacement destination key", active: 2, remaining: 101, replace: true},
		{name: "mixed destination keys", active: 2, remaining: 101, mix: true},
		{name: "tampered expired destination", active: 2, remaining: 101, corrupt: true},
		{name: "completed rotation with replacement key", active: 2, replace: true},
		{name: "destination exists before version advances", active: 1, remaining: 101, mix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newAccessHarness(t)
			parse := func(key string) *secrets.KeyRing {
				t.Helper()
				ring, err := secrets.ParseRing([]byte(`{"active_version":2,"keys":[{"version":1,"key":"` + strings.Repeat("ab", 32) + `"},{"version":2,"key":"` + strings.Repeat(key, 32) + `"}]}`))
				if err != nil {
					t.Fatal(err)
				}
				return ring
			}
			ring, replacement := parse("ef"), parse("01")
			tx, err := h.Pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback(t.Context())
			if _, err = tx.Exec(t.Context(), "UPDATE olp_go.installation SET active_key_version=$1 WHERE singleton", tc.active); err != nil {
				t.Fatal(err)
			}
			// The final destination record is expired and beyond a batch boundary;
			// sampling one row or checking only live records cannot detect it.
			for i := 0; i < 101+tc.remaining; i++ {
				key := ring
				if i >= 101 {
					key = h.Server.Keys
				} else if i == 100 && tc.mix {
					key = replacement
				}
				id := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+1)
				ciphertext, err := key.Seal(h.Server.Installation, "oidc_flow", id, []byte("private rotation fixture"))
				if err != nil {
					t.Fatal(err)
				}
				var expires *time.Time
				if i == 100 {
					expired := time.Now().Add(-time.Hour)
					expires = &expired
					if tc.corrupt {
						ciphertext[len(ciphertext)-1] ^= 1
					}
				}
				if _, err = tx.Exec(t.Context(), "INSERT INTO olp_go.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'oidc_flow',$2,$3,$4)", id, key.Active, ciphertext, expires); err != nil {
					t.Fatal(err)
				}
			}
			if err = tx.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			snapshot := func() string {
				t.Helper()
				var state string
				if err := h.Pool.QueryRow(t.Context(), `SELECT jsonb_build_object(
					'secrets',(SELECT jsonb_agg(s ORDER BY id) FROM olp_go.secrets s),
					'installation',(SELECT to_jsonb(i) FROM olp_go.installation i),
					'audit',(SELECT jsonb_agg(a ORDER BY id) FROM olp_go.audit a))::text`).Scan(&state); err != nil {
					t.Fatal(err)
				}
				return state
			}
			before := snapshot()
			if tc.replace {
				ring = replacement
			}
			count, err := ring.Rotate(t.Context(), h.Pool, h.Server.Installation)
			if err == nil || count != 0 {
				t.Fatalf("expected failure before writing: count=%d error=%v", count, err)
			}
			if strings.Contains(err.Error(), "private rotation fixture") {
				t.Fatal("rotation error exposed plaintext")
			}
			if snapshot() != before {
				t.Fatal("failed destination authentication changed persistent state")
			}
		})
	}
}
