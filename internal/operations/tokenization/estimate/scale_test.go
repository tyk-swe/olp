package estimate

import (
	"hash/fnv"
	"strings"
	"testing"
)

// The parity and oracle tests compare single-goroutine functions with their
// references on generated and corpus input, and the race detector slows them six
// to forty-six times while it has nothing to find in them: the concurrent paths
// have their own tests (TestConcurrentUseOfOneEncoding). Run whole under -race
// the package took four minutes against the five of its timeout, so a loaded CI
// machine could fail it on time alone. Under the detector they run a fixed share
// of the same input. Every generator keeps its seed, so the same cases are made
// every time and a failure reproduces; where a generator is made for each family
// the cases are the first of those the normal run makes, and the corpus is
// sampled by name, so the same bodies are read every time. Without -race nothing
// is reduced.

// scaled is how many generated cases a test makes: full as a rule, short under
// -short and race under the race detector, and the fewer of the two under both.
func scaled(full, short, race int) int {
	n := full
	if testing.Short() {
		n = min(n, short)
	}
	if raceEnabled {
		n = min(n, race)
	}
	return n
}

// corpusStride is how sparsely the race detector samples the corpus: it reads one
// body in this many of those that stand for a text of the token corpus.
const corpusStride = 16

// sampledUnderRace reports whether a body of the request corpus is read when the
// race detector is on. A request of the protocol and fidelity corpora, which are
// what clients send, is always read. The token corpus makes four bodies of each of
// its texts, so those are sampled by a hash of their names, which does not depend
// on the order of the corpus or on the order a map is walked, and the prose
// requests -short keeps are kept.
func sampledUnderRace(name string) bool {
	if !strings.HasPrefix(name, "text/") || strings.HasPrefix(name, "text/chat/prose") {
		return true
	}
	h := fnv.New32a()
	h.Write([]byte(name))
	return h.Sum32()%corpusStride == 0
}
