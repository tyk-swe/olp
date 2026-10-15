package wire

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestTransformationsReturnErrorsWithoutChangingSource(t *testing.T) {
	source, err := FromFields(Chat, "route", false, map[string]json.RawMessage{"model": json.RawMessage(`"route"`), "messages": json.RawMessage(`[{"role":"user","content":"hello"}]`)})
	if err != nil {
		t.Fatal(err)
	}
	before := source.OIF().Document()
	for _, transform := range []func() (*Request, error){
		func() (*Request, error) { return source.WithField("messages", json.RawMessage(`[`)) },
		func() (*Request, error) {
			return source.WithFields(map[string]json.RawMessage{"model": json.RawMessage(`"route"`), "input": json.RawMessage(`{"key":1,"key":2}`)}, oif.ExplicitTransform)
		},
	} {
		next, err := transform()
		if err == nil || next != nil {
			t.Fatalf("invalid transformation produced %v, %v", next, err)
		}
		if source.OIF().Document() != before {
			t.Fatal("failed transformation changed immutable source")
		}
	}
	next, err := source.WithField("user", json.RawMessage(`"reader"`))
	if err != nil {
		t.Fatal(err)
	}
	if source.Field("user") != nil || !bytes.Equal(next.Field("user"), []byte(`"reader"`)) {
		t.Fatal("successful transformation changed its input")
	}
	if _, err := FromFields(Chat, "route", false, map[string]json.RawMessage{"input": json.RawMessage(`[`)}); err == nil {
		t.Fatal("invalid fields were deferred")
	}
	if _, err := NewRequest(Chat, "route", false, oif.Document{}); err == nil {
		t.Fatal("invalid source was deferred")
	}
}
