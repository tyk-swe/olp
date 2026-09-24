package resources

import (
	"errors"
	"github.com/google/uuid"
	"strings"
	"testing"
	"time"
)

func TestSubmissionIdentityHasBoundedReplayWindow(t *testing.T) {
	now := time.Unix(1900000000, 0)
	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"current", SubmissionID(now, uuid.New()), true},
		{"retry", SubmissionID(now.Add(-14*time.Minute), uuid.New()), true},
		{"expired", SubmissionID(now.Add(-16*time.Minute), uuid.New()), false},
		{"future", SubmissionID(now.Add(6*time.Minute), uuid.New()), false},
		{"uuid only", uuid.NewString(), false},
		{"unbounded", strings.Repeat("x", 129), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSubmission(tc.id, now); (err == nil) != tc.valid {
				t.Fatalf("valid=%t err=%v", tc.valid, err)
			}
		})
	}
}

func TestResourceIdentifiersHaveCanonicalKinds(t *testing.T) {
	id := uuid.New()
	for _, kind := range []string{KindFile, KindBatch, KindResponse, KindContinuation, KindStrictResponse} {
		local := LocalID(kind, id)
		got, err := parseLocal(local)
		if err != nil || got != id {
			t.Fatalf("kind=%s got=%s err=%v", kind, got, err)
		}
		for _, invalid := range []string{"decorated_" + local, strings.ToUpper(local), "unknown_" + strings.ReplaceAll(id.String(), "-", "")} {
			if _, err := parseLocal(invalid); !errors.Is(err, ErrNotFound) {
				t.Fatalf("accepted noncanonical id %q", invalid)
			}
		}
	}
}
