package gateway

import (
	"context"
	"slices"
	"sync"

	"github.com/tyk-swe/olp/internal/guardrails"
)

type guardrailCall struct {
	engine   *guardrails.Engine
	ctx      context.Context
	masks    *guardrails.Masking
	input    []guardrails.Segment
	internal bool
	metadata bool
	models   map[[32]byte]guardrails.Verdict
	ledger   guardrailLedger
}

func (g *guardrailCall) record(values []guardrails.Evidence) { g.ledger.record(values) }

func (g *guardrailCall) evidence() []guardrails.Evidence { return g.ledger.request() }

type guardrailLedger struct {
	mu            sync.Mutex
	all           []guardrails.Evidence
	attemptActive bool
	attempt       []guardrails.Evidence
}

func (l *guardrailLedger) record(values []guardrails.Evidence) {
	if len(values) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.all = guardrails.MergeEvidence(l.all, values)
	if l.attemptActive {
		l.attempt = guardrails.MergeEvidence(l.attempt, values)
	}
}

func (l *guardrailLedger) request() []guardrails.Evidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.all)
}

func (l *guardrailLedger) startAttempt(input []guardrails.Evidence) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attemptActive = true
	l.attempt = slices.Clone(input)
}

func (l *guardrailLedger) finishAttempt() []guardrails.Evidence {
	l.mu.Lock()
	defer l.mu.Unlock()
	decisions := l.attempt
	l.attemptActive = false
	l.attempt = nil
	return decisions
}

func (l *guardrailLedger) duringAttempt() ([]guardrails.Evidence, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.attempt), l.attemptActive
}
