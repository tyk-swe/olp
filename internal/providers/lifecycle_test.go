package providers

import (
	"fmt"
	"testing"
)

func TestPublishedSlotsUseOnlyCredentialsRequiredByAuthMode(t *testing.T) {
	for _, authMode := range []string{AuthNone, AuthAPIKey, AuthHeaders} {
		for _, enabled := range []bool{false, true} {
			for _, revoked := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/enabled=%t/revoked=%t", authMode, enabled, revoked), func(t *testing.T) {
					credential, version := "retained-credential", 1
					row := slotRow{
						ID: "slot", Default: true, Name: "default", Enabled: enabled, Weight: 1,
						CredentialID: &credential, CredentialVersion: &version, CredentialRevoked: revoked,
					}
					slot := row.published(authMode)
					if slot.Enabled != (enabled && (authMode == AuthNone || !revoked)) {
						t.Fatalf("incorrect published eligibility: %+v", slot)
					}
					if authMode == AuthNone {
						if slot.CredentialID != nil || slot.CredentialVersion != nil {
							t.Fatalf("no-auth slot still references a credential: %+v", slot)
						}
					} else if slot.CredentialID != row.CredentialID || slot.CredentialVersion != row.CredentialVersion {
						t.Fatalf("authenticated slot lost its credential: %+v", slot)
					}
					if row.CredentialID != &credential || row.CredentialVersion != &version || row.Enabled != enabled {
						t.Fatal("publication changed the draft slot")
					}
				})
			}
		}
	}
}
