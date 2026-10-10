package protocols

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestConverseRejectsNonObjectBeforeIndexing(t *testing.T) {
	body := []byte(" \n[" + strings.Repeat("0,", 1<<18) + "0]\t")
	for _, stream := range []bool{false, true} {
		var failure error
		allocations := testing.AllocsPerRun(2, func() {
			_, failure = ParseBedrockRequest(body, "route", stream)
		})
		var invalid *openai.RequestError
		if !errors.As(failure, &invalid) || invalid.Code != "invalid_json" || allocations > 8 {
			t.Fatalf("Converse allocated a non-object source index: stream=%v allocations=%v error=%v", stream, allocations, failure)
		}
	}
}

func TestConverseEnvelopeGuardPreservesSource(t *testing.T) {
	body := []byte(" \n{\"messages\":[{\"role\":\"user\",\"content\":[{\"text\":\"hello\"}]}],\"native\":{\"number\":9007199254740993,\"zero\":-0}}\t")
	request, err := ParseBedrockRequest(body, "route", false)
	if err != nil || request.OIF().Document().Raw() != string(body) {
		t.Fatalf("Converse object source was changed: %v", err)
	}
}
