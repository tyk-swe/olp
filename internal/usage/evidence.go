package usage

type usageState uint8

const (
	unobservedComplete usageState = iota
	unobservedUncertain
	observedIncomplete
	observedIncompleteUncertain
	observedComplete
)

type ValidatedUsage struct {
	state      usageState
	quantities UsageQuantities
	deferred   bool
}

func classifyUsage(observed, complete, uncertain bool) (usageState, bool) {
	switch {
	case !observed && complete && !uncertain:
		return unobservedComplete, true
	case !observed && !complete && uncertain:
		return unobservedUncertain, true
	case observed && !complete && !uncertain:
		return observedIncomplete, true
	case observed && !complete && uncertain:
		return observedIncompleteUncertain, true
	case observed && complete && !uncertain:
		return observedComplete, true
	default:
		return 0, false
	}
}

func (u ValidatedUsage) observed() bool { return u.state >= observedIncomplete }
func (u ValidatedUsage) complete() bool {
	return u.state == unobservedComplete || u.state == observedComplete
}
func (u ValidatedUsage) uncertain() bool {
	return u.state == unobservedUncertain || u.state == observedIncompleteUncertain
}
func (u ValidatedUsage) wire() AttemptUsage {
	return AttemptUsage{UsageQuantities: u.quantities, Deferred: u.deferred, Observed: u.observed(), Complete: u.complete(), BillingUncertain: u.uncertain()}
}
