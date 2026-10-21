package protocols

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/wire"
)

func parse(t *testing.T, family wire.Format, body string) *wire.Request {
	t.Helper()
	r, err := Parse(family, []byte(body), "route")
	if err != nil {
		t.Fatalf("Parse(%s): %v", family, err)
	}
	return r
}

func inspectInputText(t *testing.T, r *wire.Request, fn TextSlot) *wire.Request {
	t.Helper()
	out, err := InspectInputText(r, fn)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func redactSecret(text string) (string, bool) {
	return strings.ReplaceAll(text, "s3cr3t", "[REDACTED]"), false
}

func TestInspectInputTextCompletions(t *testing.T) {
	r := parse(t, wire.Completions, `{"model":"route","prompt":["s3cr3t","safe"]}`)
	filtered, err := InspectInputText(r, redactSecret)
	if err != nil || !strings.Contains(string(filtered.Document()["prompt"]), "[REDACTED]") {
		t.Fatalf("completion prompt inspection: %v", err)
	}
	r = parse(t, wire.Completions, `{"model":"route","prompt":[1,2]}`)
	if _, err := InspectInputText(r, redactSecret); err == nil {
		t.Fatal("token IDs treated as inspected text")
	}
}
