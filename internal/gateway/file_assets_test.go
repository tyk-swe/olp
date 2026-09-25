package gateway

import (
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
)

// Managed inference-file uploads are admitted only by a profile-declared
// purpose and option contract; batch inputs keep their own admission path.
func TestFileUploadAdmissionIsProfileOwned(t *testing.T) {
	responses, err := connectors.LookupProfile("openai-responses", "1")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := connectors.LookupProfile("openai-chat", "1")
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name    string
		profile connectors.Profile
		purpose string
		extra   map[string]string
		want    bool
	}{
		{"responses purpose", responses, "user_data", nil, true},
		{"responses expiry options", responses, "user_data", map[string]string{"expires_after[anchor]": "created_at", "expires_after[seconds]": "3600"}, true},
		{"unlisted purpose", responses, "batch", nil, false},
		{"unlisted option", responses, "user_data", map[string]string{"unqualified_option": "1"}, false},
		{"mixed options", responses, "user_data", map[string]string{"expires_after[seconds]": "3600", "extra": "1"}, false},
		{"profile without file contract", chat, "user_data", nil, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if got := fileUploadAdmitted(fixture.profile, fixture.purpose, fixture.extra); got != fixture.want {
				t.Fatalf("fileUploadAdmitted=%v, want %v", got, fixture.want)
			}
		})
	}
}
