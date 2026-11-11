package source

import (
	"errors"
	"slices"
	"time"
)

var errBudget = errors.New("import pass budget exceeded")

type Resume struct {
	Period    time.Time
	Remaining int
}

func monthStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func pendingMonths(req Cloud) []time.Time {
	first := monthStart(req.Earliest)
	return slices.Collect(func(yield func(time.Time) bool) {
		for month := monthStart(req.Today); !month.Before(first); month = month.AddDate(0, -1, 0) {
			if !slices.ContainsFunc(req.Completed, month.Equal) && !yield(month) {
				return
			}
		}
	})
}

func walkMonths(req Cloud, months []time.Time, limit int, read func(month time.Time) error) (*Resume, error) {
	order := monthOrder(req, months)
	var failure error
	for index, month := range order {
		err := readWithin(index, limit, month, read)
		switch {
		case err == nil:
		case errors.Is(err, errBudget) && index > 0 && failure == nil:
			return &Resume{Period: month, Remaining: len(order) - index}, nil
		case failure == nil:
			failure = err
		}
	}
	return nil, failure
}

func readWithin(index, limit int, month time.Time, read func(month time.Time) error) error {
	if index >= limit {
		return errBudget
	}
	return read(month)
}

func monthOrder(req Cloud, months []time.Time) []time.Time {
	if len(months) == 0 {
		return nil
	}
	if req.Resume != nil {
		if index := slices.IndexFunc(months, req.Resume.Period.Equal); index >= 0 {
			return slices.Concat(months[index:], months[:index])[:min(req.Resume.Remaining, len(months))]
		}
	}
	start := req.Failures % len(months)
	return slices.Concat(months[start:], months[:start])
}
