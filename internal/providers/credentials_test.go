package providers

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
)

type unknownCredentialSource struct{}

func (unknownCredentialSource) credentialSource() {}

func TestDefaultSlotRequiresDefault(t *testing.T) {
	t.Parallel()

	t.Run("empty slots fails with 422 credential_required", func(t *testing.T) {
		t.Parallel()
		slot, err := defaultSlot(nil)
		if slot != nil {
			t.Fatalf("slot = %+v, want nil", slot)
		}
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "credential_required" {
			t.Fatalf("err = %#v, want 422 credential_required", err)
		}
	})

	t.Run("slots without default fails with 422 credential_required", func(t *testing.T) {
		t.Parallel()
		slot, err := defaultSlot([]slotRow{{ID: "slot-1", Default: false}, {ID: "slot-2", Default: false}})
		if slot != nil {
			t.Fatalf("slot = %+v, want nil", slot)
		}
		problem, ok := errors.AsType[*httpkit.Problem](err)
		if !ok || problem.Status != 422 || problem.Code != "credential_required" {
			t.Fatalf("err = %#v, want 422 credential_required", err)
		}
	})

	t.Run("returns default slot when present", func(t *testing.T) {
		t.Parallel()
		slots := []slotRow{{ID: "slot-1", Default: false}, {ID: "slot-2", Default: true}}
		slot, err := defaultSlot(slots)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if slot == nil || slot.ID != "slot-2" {
			t.Fatalf("slot = %+v, want slot-2", slot)
		}
	})
}

func TestCredentialSourceRejectsNilAndUnknown(t *testing.T) {
	t.Parallel()
	var svc credentialService

	t.Run("material returns pasted bytes", func(t *testing.T) {
		t.Parallel()
		got, err := svc.material(t.Context(), pastedCredential("sk-test"))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "sk-test" {
			t.Fatalf("material = %q, want %q", got, "sk-test")
		}
	})

	for name, source := range map[string]credentialSource{
		"nil":     nil,
		"unknown": unknownCredentialSource{},
	} {
		t.Run("material rejects "+name, func(t *testing.T) {
			t.Parallel()
			got, err := svc.material(t.Context(), source)
			if err == nil {
				t.Fatalf("expected error for %s source, got %q", name, got)
			}
		})

		t.Run("storeCredential rejects "+name, func(t *testing.T) {
			t.Parallel()
			id, version, err := svc.storeCredential(t.Context(), nil, "provider-1", source)
			if err == nil {
				t.Fatalf("expected error for %s source, got id=%q version=%d", name, id, version)
			}
		})
	}
}

func TestCredentialAndNetworkRepliesKeepWireContract(t *testing.T) {
	t.Parallel()
	createdAt := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)

	t.Run("pasted credential preserves explicit null fields", func(t *testing.T) {
		t.Parallel()
		encoded, err := json.Marshal(credentialItem{
			ID:            "cred-1",
			Version:       1,
			Active:        true,
			DraftSelected: true,
			CreatedAt:     createdAt,
		})
		if err != nil {
			t.Fatal(err)
		}
		want := `{"id":"cred-1","version":1,"active":true,"draft_selected":true,"created_at":"2026-04-01T12:00:00Z","revoked_at":null,"external_reference":null,"grant":null}`
		if string(encoded) != want {
			t.Fatalf("got %s, want %s", encoded, want)
		}
	})

	t.Run("grant credential encodes empty facts object and nullable timestamps", func(t *testing.T) {
		t.Parallel()
		encoded, err := json.Marshal(credentialItem{
			ID:        "cred-2",
			Version:   2,
			CreatedAt: createdAt,
			Grant: &credentialGrantItem{
				PluginDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
				Principal:    "acct-1",
				Facts:        map[string]string{},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		want := `{"id":"cred-2","version":2,"active":false,"draft_selected":false,"created_at":"2026-04-01T12:00:00Z","revoked_at":null,"external_reference":null,"grant":{"plugin_digest":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","principal":"acct-1","facts":{},"expires_at":null,"lapsed_at":null}}`
		if string(encoded) != want {
			t.Fatalf("got %s, want %s", encoded, want)
		}
	})

	t.Run("network credential items and mutations match contract fields", func(t *testing.T) {
		t.Parallel()
		itemJSON, err := json.Marshal(networkCredentialItem{ID: "net-1", Version: 1, CreatedAt: createdAt})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"id":"net-1","version":1,"created_at":"2026-04-01T12:00:00Z","revoked_at":null}`; string(itemJSON) != want {
			t.Fatalf("item got %s, want %s", itemJSON, want)
		}

		createdJSON, err := json.Marshal(networkCredentialMutation{ProviderID: "prov-1", CredentialID: "net-1", ETag: "etag-1"})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"provider_id":"prov-1","credential_id":"net-1","etag":"etag-1"}`; string(createdJSON) != want {
			t.Fatalf("created got %s, want %s", createdJSON, want)
		}

		revokedJSON, err := json.Marshal(revokedNetworkCredential{
			ProviderID:        "prov-1",
			CredentialID:      "net-1",
			ETag:              "etag-2",
			RuntimeGeneration: access.AuthorityGeneration{ID: "gen-1", Sequence: 7},
		})
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"provider_id":"prov-1","credential_id":"net-1","etag":"etag-2","runtime_generation":{"id":"gen-1","sequence":7}}`; string(revokedJSON) != want {
			t.Fatalf("revoked got %s, want %s", revokedJSON, want)
		}
	})
}
