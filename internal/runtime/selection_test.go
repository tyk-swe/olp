package runtime

import (
	"errors"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

func TestAttemptOrderCorpus(t *testing.T) {
	corpus := testutil.JSON[struct {
		Snapshot Snapshot `json:"snapshot"`
		Cases    []struct {
			Name        string   `json:"name"`
			Route       string   `json:"route"`
			Operation   string   `json:"operation"`
			Surface     string   `json:"surface"`
			Mode        string   `json:"mode"`
			Affinity    string   `json:"affinity"`
			ExpectedIDs []string `json:"expected_provider_ids"`
			Error       string   `json:"expected_error"`
		} `json:"cases"`
	}](t, fixtures.Files, "routing/attempt-order.json")
	if err := corpus.Snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	digest, err := corpus.Snapshot.Digest()
	if err != nil || len(digest) != 64 {
		t.Fatalf("digest %q %v", digest, err)
	}
	for _, c := range corpus.Cases {
		attempts, err := Select(&corpus.Snapshot, c.Route, c.Operation, c.Surface, c.Mode, []byte(c.Affinity))
		if c.Error != "" {
			var se *SelectionError
			if !errors.As(err, &se) || se.Code != c.Error {
				t.Errorf("%s: expected %s, got %v", c.Name, c.Error, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.Name, err)
			continue
		}
		var ids []string
		for _, a := range attempts {
			ids = append(ids, a.ProviderID)
		}
		if len(ids) != len(c.ExpectedIDs) {
			t.Errorf("%s: got %v want %v", c.Name, ids, c.ExpectedIDs)
			continue
		}
		for i := range ids {
			if ids[i] != c.ExpectedIDs[i] {
				t.Errorf("%s: got %v want %v", c.Name, ids, c.ExpectedIDs)
				break
			}
		}
	}
}

func TestValidateRejectsDanglingTargets(t *testing.T) {
	corpus := testutil.JSON[struct {
		Snapshot Snapshot `json:"snapshot"`
	}](t, fixtures.Files, "routing/attempt-order.json")
	route := corpus.Snapshot.Routes["team-chat"]
	route.Targets[0].ProviderID = "018f0000-0000-7000-8000-0000000000ff"
	corpus.Snapshot.Routes["team-chat"] = route
	if err := corpus.Snapshot.Validate(); err == nil {
		t.Fatal("dangling provider accepted")
	}
}
