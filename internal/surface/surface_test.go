package surface

import (
	"testing"
)

func TestReservedPrefixesAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, prefix := range Reserved() {
		if seen[prefix.Path] {
			t.Fatalf("%s is reserved twice", prefix.Path)
		}
		seen[prefix.Path] = true
		if prefix.GatewayCatchAll && !prefix.Surface.Inference {
			t.Fatalf("%s is a gateway catch-all outside inference", prefix.Path)
		}
		if got := Of(prefix.Path); got != prefix.Surface {
			t.Fatalf("%s classifies as %+v, want %+v", prefix.Path, got, prefix.Surface)
		}
	}
}
