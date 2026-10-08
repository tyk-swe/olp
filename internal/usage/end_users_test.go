package usage

import (
	"strings"
	"testing"
)

func TestEndUserEventValidationAndRoundTrip(t *testing.T) {
	for _, digest := range []string{"", strings.Repeat("ab", 32), "raw-customer", strings.Repeat("a", 63), strings.Repeat("A", 64)} {
		event := metadataEvent()
		event.EndUserDigest = digest
		valid := digest == "" || digest == strings.Repeat("ab", 32)
		_, err := Validate(event)
		if (err == nil) != valid {
			t.Fatalf("Validate(%q): %v", digest, err)
		}
		payload, err := Encode(event)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := Decode(payload)
		if (err == nil) != valid || (valid && decoded.EndUserDigest != digest) {
			t.Fatalf("Decode(%q): %+v, %v", digest, decoded, err)
		}
	}
}
