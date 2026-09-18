package contract_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/management/contract"
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

func TestOptionalNullAndDecimalString(t *testing.T) {
	for _, raw := range []string{`{}`, `{"unit_price":null}`, `{"unit_price":"0.00000000000000000001"}`} {
		roundTrip[contract.PriceCeiling](t, raw)
	}
	var price contract.PriceCeiling
	if err := json.Unmarshal([]byte(`{"unit_price":0.1}`), &price); err == nil {
		t.Fatal("decimal accepted as JSON number")
	}
}

func TestUnionAllOfAndExtensionMap(t *testing.T) {
	for _, raw := range []string{
		`{"strategy":"price","allow_fallbacks":null,"only":["provider-a"],"deny_data_collection":true}`,
		`{"strategy":null,"max_price":{"unit_price":"0.002"}}`,
	} {
		roundTrip[contract.RoutingPreferences](t, raw)
	}
	// A oneOf transport carries raw JSON until the owning domain validates it.
	roundTrip[contract.PlaygroundResponseFormat](t, `{"type":"json_schema","name":"result","schema":{"type":"object","additionalProperties":false}}`)
	roundTrip[contract.ConnectionOptions](t, `{"parameter_defaults":{"temperature":0.5,"nested":{"enabled":true}}}`)
}

func TestClosedObjectsNeedStrictHandlerDecoding(t *testing.T) {
	// Generated structs express fields, but encoding/json permits unknown fields
	// by default. Management handlers must explicitly enforce additionalProperties:false.
	decoder := json.NewDecoder(strings.NewReader(`{"name":"x","configuration":{"kind":"openai","auth_mode":"bearer"},"unexpected":true}`))
	decoder.DisallowUnknownFields()
	var value contract.CreateProviderRequest
	if err := decoder.Decode(&value); err == nil {
		t.Fatal("strict decoder accepted an unknown field")
	}
}
