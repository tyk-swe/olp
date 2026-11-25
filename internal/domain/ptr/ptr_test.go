package ptr_test

import (
	"encoding/json"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/ptr"
)

func TestOrAndValueReadThroughNil(t *testing.T) {
	t.Parallel()
	set, zero := "set", ""
	cases := []struct {
		name      string
		in        *string
		or, value string
	}{
		{"nil", nil, "fallback", ""},
		{"set", &set, "set", "set"},
		{"set to the zero value", &zero, "", ""},
	}
	for _, c := range cases {
		if got := ptr.Or(c.in, "fallback"); got != c.or {
			t.Errorf("%s: Or = %q, want %q", c.name, got, c.or)
		}
		if got := ptr.Value(c.in); got != c.value {
			t.Errorf("%s: Value = %q, want %q", c.name, got, c.value)
		}
	}
}

func TestCloneDoesNotShareTheTarget(t *testing.T) {
	t.Parallel()
	if got := ptr.Clone[int](nil); got != nil {
		t.Fatalf("Clone(nil) = %v, want nil", *got)
	}
	original := 7
	cloned := ptr.Clone(&original)
	*cloned = 8
	if original != 7 {
		t.Fatalf("writing through the clone changed the original to %d", original)
	}
}

func TestNonNilEncodesAsEmptyNotNull(t *testing.T) {
	t.Parallel()
	type named []string
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"nil slice", ptr.NonNil([]int(nil)), "[]"},
		{"nil named slice", ptr.NonNil(named(nil)), "[]"},
		{"filled slice", ptr.NonNil([]int{1, 2}), "[1,2]"},
		{"nil map", ptr.NonNilMap(map[string]int(nil)), "{}"},
		{"filled map", ptr.NonNilMap(map[string]int{"a": 1}), `{"a":1}`},
	}
	for _, c := range cases {
		got, err := json.Marshal(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if string(got) != c.want {
			t.Errorf("%s: encoded %s, want %s", c.name, got, c.want)
		}
	}
}
