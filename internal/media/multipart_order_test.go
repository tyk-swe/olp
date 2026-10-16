package media

import (
	"slices"
	"testing"
)

func TestExtraFieldsUseDeterministicKeyOrder(t *testing.T) {
	fields := extraFields(nil, map[string]any{"z": "last", "a": []any{"first", "second"}, "middle": 2})
	var values []string
	for _, field := range fields {
		if field.Text == nil {
			t.Fatal("extension field lost its text value")
		}
		values = append(values, field.Name+"="+*field.Text)
	}
	want := []string{"a[]=first", "a[]=second", "middle=2", "z=last"}
	if !slices.Equal(values, want) {
		t.Fatalf("extension field order = %v, want %v", values, want)
	}
}
