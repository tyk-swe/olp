package scripted

import (
	"strings"
	"testing"
)

func TestParseDirectivesReportsMistakes(t *testing.T) {
	for text, fragment := range map[string]string{
		`[[olp:tool]]`:                  "tool name",
		`[[olp:tool x {bad}]]`:          "not JSON",
		`[[olp:tool x [1]]]`:            "JSON object",
		`[[olp:tool x {"a":1}`:          "closing",
		`[[olp:also x]]`:                "before it",
		`[[olp:reply hello]]`:           "not JSON",
		`[[olp:reply 3]]`:               "JSON string",
		`[[olp:fail 200]]`:              "error status",
		`[[olp:fail "x"]]`:              "error status",
		`[[olp:sing "x"]]`:              "unknown keyword",
		`[[olp:reply]]`:                 "missing argument",
		`[[olp:tool a {}]][[olp:oops]]`: "unknown keyword",
	} {
		if _, err := parseDirectives(text); err == nil || !strings.Contains(err.Error(), fragment) {
			t.Errorf("%s: error %v, want one about %q", text, err, fragment)
		}
	}
	if d, err := parseDirectives("no directive here, even [[1,2]] or [[olpx]]"); err != nil || len(d.rounds) != 0 {
		t.Fatalf("plain text parsed as %+v, %v", d, err)
	}
}

// one of two results: round not yet answered

// A tool-result message may also carry unrelated text, which is not a prompt.

// Once the loop produced a result, a still-forced request gets its text.
