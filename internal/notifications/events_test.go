package notifications

import (
	"encoding/json"
	"maps"
	"os"
	"slices"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
)

func TestEveryPublishedEventHasOneSpec(t *testing.T) {
	raw, err := os.ReadFile("../../management/management.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas struct {
				NotificationEvent struct {
					Enum []contract.NotificationEvent `json:"enum"`
				} `json:"NotificationEvent"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	published := spec.Components.Schemas.NotificationEvent.Enum
	handled := slices.Sorted(maps.Keys(events))
	slices.Sort(published)
	if len(published) == 0 || !slices.Equal(published, handled) {
		t.Fatalf("published events %v, handled events %v", published, handled)
	}
}
