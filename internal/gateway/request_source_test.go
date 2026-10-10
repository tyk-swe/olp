package gateway

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestAuxiliaryIngressRejectsNonObjectBeforeIndexing(t *testing.T) {
	body := []byte(" \n[" + strings.Repeat("0,", 1<<18) + "0]\t")
	for name, accepts := range map[string]func([]byte) bool{
		"batch source": func(body []byte) bool {
			_, err := parseObjectRequest(body, oif.Limits{MaxBytes: 1 << 20})
			return err == nil
		},
		"realtime frame": func(body []byte) bool {
			_, ok := realtimeObject(body)
			return ok
		},
		"native attribution": func(body []byte) bool {
			return nativeEndUser(body, "openai") != ""
		},
	} {
		t.Run(name, func(t *testing.T) {
			accepted := false
			allocations := testing.AllocsPerRun(2, func() { accepted = accepts(body) })
			if accepted || allocations > 8 {
				t.Fatalf("invalid envelope allocated a source index: accepted=%v allocations=%v", accepted, allocations)
			}
		})
	}
}

func TestObjectRequestEnvelopePreservesNativeBytes(t *testing.T) {
	body := []byte(" \n{\"native\":{\"number\":9007199254740993,\"zero\":-0},\"safety_identifier\":\"end-user\"}\t")
	document, err := parseObjectRequest(body, oif.Limits{MaxBytes: len(body)})
	if err != nil || document.Raw() != string(body) || nativeEndUser(body, "openai") != "end-user" {
		t.Fatalf("object request changed or lost attribution: %v", err)
	}
	if _, err := parseObjectRequest(body, oif.Limits{MaxBytes: len(body) - 1}); err == nil {
		t.Fatal("object request bypassed its byte bound")
	}
	for _, input := range []string{`{"x":0,"x":1}`, `{"x":`, `{} {}`} {
		if _, err := parseObjectRequest([]byte(input), oif.Limits{}); err == nil {
			t.Fatalf("accepted malformed object %q", input)
		}
	}
}
