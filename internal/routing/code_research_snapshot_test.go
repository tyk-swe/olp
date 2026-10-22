package routing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/codemode"
)

func TestResearchAbsentPreservesLegacySnapshotDigest(t *testing.T) {
	// This is the canonical digest input written before research bindings
	// existed. Its field order and absent optional fields are persisted data.
	const legacy = `{"code_routes":{"coding":{"id":"route","project_id":"project","slug":"coding","pool_id":"pool","models":["model"],"enabled":true,"revision_id":"revision","revision":1,"etag":"etag","published_at":null}},"providers":{},"routes":{}}`
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(legacy), &snapshot); err != nil {
		t.Fatal(err)
	}
	digest, err := snapshot.Digest()
	want := sha256.Sum256([]byte(legacy))
	if err != nil || digest != hex.EncodeToString(want[:]) {
		t.Fatalf("legacy release digest changed: %s %v", digest, err)
	}
}

func TestResearchSnapshotCloneOwnsBindingAndServices(t *testing.T) {
	tools := &codemode.ResearchTools{AccountID: "account", Services: []string{"search", "reader"}}
	input := &Snapshot{CodeRoutes: map[string]codemode.Route{"coding": {Tools: tools}}}
	frozen := input.Clone()
	before, err := frozen.Digest()
	if err != nil {
		t.Fatal(err)
	}
	tools.AccountID = "replacement"
	tools.Services[0] = "zread"
	after, err := frozen.Digest()
	if err != nil || after != before || frozen.CodeRoutes["coding"].Tools.AccountID != "account" || frozen.CodeRoutes["coding"].Tools.Services[0] != "search" {
		t.Fatalf("caller mutated frozen research binding: %+v %v", frozen.CodeRoutes["coding"].Tools, err)
	}
}
