package guardrailpolicy

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestMergeEvidenceFoldsRepeatsWithinLimits(t *testing.T) {
	first := []Evidence{{GuardrailID: "g", Revision: "1", Phase: Output, Action: Block, Outcome: Blocked, Categories: []string{"b"}, LatencyMS: 2}}
	merged := MergeEvidence(first, []Evidence{{GuardrailID: "g", Revision: "1", Phase: Output, Action: Block, Outcome: Blocked, Categories: []string{"a", "b"}, LatencyMS: 3}})
	if len(merged) != 1 || merged[0].LatencyMS != 5 || !slices.Equal(merged[0].Categories, []string{"a", "b"}) || first[0].LatencyMS != 2 {
		t.Fatalf("repeated evaluations were not folded into one record: %+v (input %+v)", merged, first)
	}
	categories := func(prefix string) []string {
		values := make([]string, MaxCategories)
		for i := range values {
			values[i] = prefix + strconv.Itoa(i)
		}
		return values
	}
	repeated := MergeEvidence(first, []Evidence{{GuardrailID: "g", Revision: "1", Phase: Output, Action: Block, Outcome: Blocked, Categories: categories("c")}})
	if err := ValidateEvidence(repeated); err != nil {
		t.Fatalf("folding repeats produced evidence usage would drop: %v", err)
	}
	wide := []string{strings.Repeat("c", 1024)}
	values := make([]Evidence, MaxEntries*4)
	for i := range values {
		values[i] = Evidence{GuardrailID: strconv.Itoa(i), Revision: "1", Phase: Input, Action: Block, Outcome: Blocked, Categories: wide}
	}
	bounded := MergeEvidence(nil, values)
	raw, err := json.Marshal(bounded)
	if err != nil || len(bounded) == 0 || len(bounded) == len(values) || len(raw) > MaxEvidenceBytes {
		t.Fatalf("evidence escaped the byte limit: %d records, %d bytes", len(bounded), len(raw))
	}
}
