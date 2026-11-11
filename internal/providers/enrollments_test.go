package providers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/grants"
)

func TestEnrollmentStatusReplyCarriesOnlyWhatItsStatusNeeds(t *testing.T) {
	t.Parallel()
	current := &record{ID: "provider-1", ETag: "etag-1"}
	issued := grants.Credential{ID: "credential-1", Principal: "principal-1", Version: 2}
	cases := map[string]struct {
		standing grants.Standing
		want     string
	}{
		"pending waits the interval":  {grants.Standing{Status: grants.Pending, Interval: 5 * time.Second}, `{"status":"pending","interval":5}`},
		"pending without a wait":      {grants.Standing{Status: grants.Pending}, `{"status":"pending","interval":0}`},
		"completed names the version": {grants.Standing{Status: grants.Completed, Interval: 5 * time.Second, Credential: issued}, `{"status":"completed","completion":{"provider_id":"provider-1","etag":"etag-1","credential_id":"credential-1","credential_version":2,"principal":"principal-1"}}`},
		"denied":                      {grants.Standing{Status: grants.Denied}, `{"status":"denied"}`},
		"expired":                     {grants.Standing{Status: grants.Expired, Ended: true}, `{"status":"expired"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			encoded, err := json.Marshal(grantStatus(current, c.standing))
			if err != nil {
				t.Fatal(err)
			}
			if string(encoded) != c.want {
				t.Fatalf("reply %s, want %s", encoded, c.want)
			}
		})
	}
}
