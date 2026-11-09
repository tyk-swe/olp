package batches

import "time"

type Decision struct {
	State State
	Gap   int64
}

func Gap(expected *int64, counts ResultCounts) int64 {
	if expected == nil {
		return 1
	}
	return max(*expected-counts.Observed(), 1)
}

func CloseDecision(expected *int64, counts ResultCounts, conflict, terminal bool, now, closeAfter time.Time) Decision {
	expired := !now.Before(closeAfter)
	switch {
	case conflict && expired:
		return Decision{State: StateUncertain, Gap: Gap(expected, counts)}
	case conflict:
		return Decision{State: StateConflict}
	case expected != nil && counts.Observed() >= *expected && (terminal || *expected > 0):
		return Decision{State: StateSettled}
	case terminal || expired:
		return Decision{State: StateUncertain, Gap: Gap(expected, counts)}
	default:
		return Decision{State: StateActive}
	}
}
