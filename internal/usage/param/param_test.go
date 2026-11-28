package param

import (
	"errors"
	"net/url"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/problem"
)

func TestRevisionCursor(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"abc", "0", "-3"} {
		_, err := RevisionCursor(url.Values{"cursor": {raw}})
		problem, ok := errors.AsType[*problem.Problem](err)
		if !ok || problem.Status != 400 || problem.Code != "invalid_cursor" {
			t.Fatalf("cursor %q error = %v, want 400 invalid_cursor", raw, err)
		}
	}
	if before, err := RevisionCursor(url.Values{"cursor": {" "}}); before != nil || err != nil {
		t.Fatalf("blank cursor = %v, %v, want absent", before, err)
	}
	if before, err := RevisionCursor(url.Values{"cursor": {"7"}}); err != nil || before == nil || *before != 7 {
		t.Fatalf("cursor 7 = %v, %v", before, err)
	}
}
