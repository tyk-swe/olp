package providers

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
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
		"denied":                      {grants.Standing{Status: grants.Denied, Ended: true}, `{"status":"denied"}`},
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

func TestEnrollmentStandingReplyAndAuditEvent(t *testing.T) {
	t.Parallel()
	current := &record{ID: "provider-1", ETag: "etag-1"}
	terminal := httpkit.Fail(422, "grant_enrollment_failed", "upstream rejected device code")
	wantFailedEvent := access.Event{Action: grantEnrollAction, Resource: "provider", ID: "provider-1", Failed: true}

	t.Run("terminal Standing.Failure returns error and audits failure", func(t *testing.T) {
		t.Parallel()
		standing := grants.Standing{Ended: true, Failure: terminal}
		reply, err := enrollmentStandingReply(current, standing)
		if !errors.Is(err, terminal) {
			t.Fatalf("err = %v, want %v", err, terminal)
		}
		if reply.Status != 0 {
			t.Fatalf("reply status = %d, want 0", reply.Status)
		}
		if got := enrollmentAuditEvent(current.ID, standing); got != wantFailedEvent {
			t.Fatalf("event = %+v, want %+v", got, wantFailedEvent)
		}
	})

	t.Run("ended denied and expired standings return 200 and audit failure", func(t *testing.T) {
		t.Parallel()
		for _, status := range []grants.Status{grants.Denied, grants.Expired} {
			standing := grants.Standing{Status: status, Ended: true}
			reply, err := enrollmentStandingReply(current, standing)
			if err != nil {
				t.Fatalf("status %s returned unexpected error: %v", status, err)
			}
			if reply.Status != http.StatusOK {
				t.Fatalf("status %s reply status = %d, want 200", status, reply.Status)
			}
			if got := enrollmentAuditEvent(current.ID, standing); got != wantFailedEvent {
				t.Fatalf("status %s event = %+v, want %+v", status, got, wantFailedEvent)
			}
		}
	})

	t.Run("pending standing returns 200 and writes no audit event", func(t *testing.T) {
		t.Parallel()
		standing := grants.Standing{Status: grants.Pending, Interval: 5 * time.Second}
		reply, err := enrollmentStandingReply(current, standing)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if reply.Status != http.StatusOK {
			t.Fatalf("reply status = %d, want 200", reply.Status)
		}
		if got := enrollmentAuditEvent(current.ID, standing); got != (access.Event{}) {
			t.Fatalf("event = %+v, want zero event", got)
		}
	})
}
