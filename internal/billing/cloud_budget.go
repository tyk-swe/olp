package billing

import (
	"errors"
	"slices"
)

var errCloudBudget = errors.New("cloud import pass budget exceeded")

type cloudContinuation struct {
	next      string
	remaining int
}

func (c *cloudContinuation) Error() string { return "cloud import continues in the next pass" }

func cloudPeriodOrder(row *importRow, periods []string) []string {
	start := row.ConsecutiveFailures % len(periods)
	remaining := len(periods)
	if row.CloudNextPeriod != nil && row.CloudPeriodsRemaining > 0 {
		if index := slices.Index(periods, *row.CloudNextPeriod); index >= 0 {
			start = index
			remaining = min(row.CloudPeriodsRemaining, len(periods))
		}
	}
	return slices.Concat(periods[start:], periods[:start])[:remaining]
}
