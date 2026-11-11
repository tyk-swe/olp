package usage

// UsageEvidence is what an attempt's accounting can claim. Observed means the
// provider reported usage, complete means the accounting is settled, and
// uncertain means the attempt may have been charged upstream without the
// evidence ever arriving. Only these five combinations are meaningful, and the
// zero value is refused by Validate.
type UsageEvidence uint8

const (
	EvidenceUnset UsageEvidence = iota
	EvidenceUnobservedComplete
	EvidenceUnobservedUncertain
	EvidenceObservedIncomplete
	EvidenceObservedIncompleteUncertain
	EvidenceObservedComplete
)

// EvidenceFrom maps the three wire flags to a state, and to EvidenceUnset when
// no state has that combination.
func EvidenceFrom(observed, complete, uncertain bool) UsageEvidence {
	switch {
	case !observed && complete && !uncertain:
		return EvidenceUnobservedComplete
	case !observed && !complete && uncertain:
		return EvidenceUnobservedUncertain
	case observed && !complete && !uncertain:
		return EvidenceObservedIncomplete
	case observed && !complete && uncertain:
		return EvidenceObservedIncompleteUncertain
	case observed && complete && !uncertain:
		return EvidenceObservedComplete
	default:
		return EvidenceUnset
	}
}

func (e UsageEvidence) Valid() bool { return e != EvidenceUnset }

func (e UsageEvidence) Observed() bool {
	return e == EvidenceObservedIncomplete || e == EvidenceObservedIncompleteUncertain || e == EvidenceObservedComplete
}

func (e UsageEvidence) Complete() bool {
	return e == EvidenceUnobservedComplete || e == EvidenceObservedComplete
}

func (e UsageEvidence) Uncertain() bool {
	return e == EvidenceUnobservedUncertain || e == EvidenceObservedIncompleteUncertain
}

// Unsettled is the state of evidence that a later observation may still replace.
func (e UsageEvidence) Unsettled() UsageEvidence {
	if e.Observed() {
		return EvidenceObservedIncompleteUncertain
	}
	return EvidenceUnobservedUncertain
}
