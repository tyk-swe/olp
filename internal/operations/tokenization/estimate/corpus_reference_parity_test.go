package estimate

import (
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/testutil"
)

// TestWalkerReadsWhatTheReferenceReadsOnTheCorpus is the comparison of the walker
// with the reference on the requests of the protocol and token corpora, which
// are what clients send, each read as a request of every family. Under the race
// detector it reads the sample sampledUnderRace names.
func TestWalkerReadsWhatTheReferenceReadsOnTheCorpus(t *testing.T) {
	read := 0
	for name, body := range testutil.Requests(t) {
		if testing.Short() && strings.HasPrefix(name, "text/") && !strings.HasPrefix(name, "text/chat/prose") {
			continue
		}
		if raceEnabled && !sampledUnderRace(name) {
			continue
		}
		for _, family := range walkerFamilies {
			sameReadings(t, family, body)
		}
		read++
	}
	// A sample that shrinks with the corpus must not shrink to nothing.
	if raceEnabled && !testing.Short() && read < 120 {
		t.Fatalf("the sample of the corpus holds %d bodies", read)
	}
}
