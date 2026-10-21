package contract_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/usage"
)

func roundTrip[T any](t *testing.T, raw string) T {
	t.Helper()
	var value T
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var before, after any
	json.Unmarshal([]byte(raw), &before)
	json.Unmarshal(encoded, &after)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("transport round trip changed JSON:\n%s\n%s", raw, encoded)
	}
	return value
}

// A oneOf transport carries raw JSON until the owning domain validates it.

// Generated structs express fields, but encoding/json permits unknown fields
// by default. Management handlers must explicitly enforce additionalProperties:false.

func TestRequestOriginsMatchUsageEvents(t *testing.T) {
	for _, origin := range []string{usage.OriginCaller, usage.OriginShadow, usage.OriginClassifier, usage.OriginCacheEmbedding, usage.OriginProbe, usage.OriginPlayground, usage.OriginBatchSettlement} {
		t.Run(origin, func(t *testing.T) {
			value := roundTrip[contract.RequestOrigin](t, `"`+origin+`"`)
			if !value.Valid() {
				t.Fatalf("usage origin %q is missing from the management contract", origin)
			}
		})
	}
}
