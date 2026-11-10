package billing

import (
	"errors"
	"slices"
	"time"
)

var errCloudBudget = errors.New("cloud import pass budget exceeded")

type cloudContinuation struct {
	next      time.Time
	remaining int
}

func (c *cloudContinuation) Error() string { return "cloud import continues in the next pass" }

func firstOfMonth(day time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func pendingMonths(earliest, today time.Time, completed []time.Time) []time.Time {
	first := firstOfMonth(earliest)
	months := []time.Time{}
	for month := firstOfMonth(today); !month.Before(first); month = month.AddDate(0, -1, 0) {
		if !slices.ContainsFunc(completed, month.Equal) {
			months = append(months, month)
		}
	}
	return months
}

func walkMonths(row *importRow, months []time.Time, limit int, read func(month time.Time) error) error {
	order := monthOrder(row, months)
	var failure error
	for index, month := range order {
		err := readWithin(index, limit, month, read)
		switch {
		case err == nil:
		case errors.Is(err, errCloudBudget) && index > 0 && failure == nil:
			return &cloudContinuation{next: month, remaining: len(order) - index}
		case failure == nil:
			failure = err
		}
	}
	return failure
}

func readWithin(index, limit int, month time.Time, read func(month time.Time) error) error {
	if index >= limit {
		return errCloudBudget
	}
	return read(month)
}

func monthOrder(row *importRow, months []time.Time) []time.Time {
	if len(months) == 0 {
		return nil
	}
	if row.ResumePeriod != nil && row.ResumeRemaining > 0 {
		if index := slices.IndexFunc(months, row.ResumePeriod.Equal); index >= 0 {
			return slices.Concat(months[index:], months[:index])[:min(row.ResumeRemaining, len(months))]
		}
	}
	start := row.ConsecutiveFailures % len(months)
	return slices.Concat(months[start:], months[:start])
}
