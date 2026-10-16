package providers

import (
	"strings"
	"testing"
)

func TestSlotUUIDsAreCanonical(t *testing.T) {
	const id = "abcdef01-2345-4678-9abc-def012345678"
	for _, value := range []string{strings.ToUpper(id), strings.ReplaceAll(id, "-", ""), "urn:uuid:" + id} {
		in := slotInput{ID: value, Name: "slot", CredentialVersionID: &value, AllowedAPIKeys: []string{value}}
		if err := validSlot(&in, id); err != nil {
			t.Fatal(err)
		}
		if in.ID != id || *in.CredentialVersionID != id || in.AllowedAPIKeys[0] != id {
			t.Fatalf("noncanonical UUIDs retained: %+v", in)
		}
	}
}
