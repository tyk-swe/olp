package resources

import (
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/retainedapi"
)

func TestRetainedLocalIdentity(t *testing.T) {
	id := uuid.New()
	for _, kind := range retainedapi.Kinds() {
		local := LocalID(kind, id)
		got, err := parseLocal(local)
		if err != nil || got != id {
			t.Errorf("%s: %v %v", kind, got, err)
		}
	}
	for _, local := range []string{"unknown_" + id.String(), "agent_resource_" + id.String(), "agent_resource_00000000000000000000000000000000"} {
		if _, err := parseLocal(local); err == nil {
			t.Errorf("invalid identity admitted: %s", local)
		}
	}
}
