package estimate

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// TestWalkerReadsWhatTheReferenceReadsOnTheCorpus is the comparison of the walker
// with the reference on the requests of the protocol and token corpora, which
// are what clients send, each read as a request of every family.
func TestWalkerReadsWhatTheReferenceReadsOnTheCorpus(t *testing.T) {
	for name, body := range testutil.Requests(t) {
		if testing.Short() && strings.HasPrefix(name, "text/") && !strings.HasPrefix(name, "text/chat/prose") {
			continue
		}
		for _, family := range walkerFamilies {
			sameReadings(t, family, body)
		}
	}
}
