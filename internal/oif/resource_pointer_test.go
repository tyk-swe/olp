package oif

import "testing"

func TestResourceIdentityArrayPointerBoundaries(t *testing.T) {
	doc, err := ParseJSON([]byte(`{"input":[{"content":[{"file_id":"native"}]}],"object":{"0":{"file_id":"native"},"*":{"file_id":"native"}}}`), Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pattern, pointer string
		want             bool
	}{
		{"/input/*/content/*/file_id", "/input/0/content/0/file_id", true},
		{"/input/*/content/*/file_id", "/input/00/content/0/file_id", false},
		{"/input/*/content/*/file_id", "/input/0/content/-1/file_id", false},
		{"/object/*/file_id", "/object/0/file_id", false},
		{"/object/*/file_id", "/object/*/file_id", false},
	} {
		if got := identityPointerMatches(tc.pattern, tc.pointer, doc); got != tc.want {
			t.Fatalf("%s matches %s: %v", tc.pattern, tc.pointer, got)
		}
	}
}
