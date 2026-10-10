package protocols

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/protocols/openai"
)

func TestNonObjectRequestsDoNotAllocateASourceIndex(t *testing.T) {
	body := []byte("[" + strings.Repeat("0,", 1<<18) + "0]")
	for _, family := range []openai.Family{openai.FamilyChat, openai.FamilyResponses, openai.FamilyAnthropic, openai.FamilyGemini, openai.FamilyBedrock} {
		t.Run(string(family), func(t *testing.T) {
			var failure error
			allocations := testing.AllocsPerRun(2, func() {
				_, failure = Parse(family, body, "route")
			})
			if failure == nil || allocations > 8 {
				t.Fatalf("non-object envelope allocated an index: allocations=%v error=%v", allocations, failure)
			}
		})
	}
}
