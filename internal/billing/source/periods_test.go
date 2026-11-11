package source

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func TestWalkMonthsContinuesAnOverBudgetSweepWithoutRereadingMonths(t *testing.T) {
	months := []time.Time{
		time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC),
	}
	read := []time.Time{}
	sweep := func(resume *Resume) (*Resume, error) {
		return walkMonths(Cloud{Resume: resume}, months, 2, func(month time.Time) error {
			read = append(read, month)
			return nil
		})
	}

	resume, passes := (*Resume)(nil), 0
	for {
		passes++
		next, err := sweep(resume)
		if err != nil {
			t.Fatalf("pass %d: %v", passes, err)
		}
		if next == nil {
			break
		}
		resume = next
	}
	if passes != 3 || !slices.Equal(read, months) {
		t.Fatalf("read %v over %d passes, want each month once over 3 passes", read, passes)
	}
}

func TestWalkMonthsReadsPastAFailedMonthAndReportsTheFailure(t *testing.T) {
	months := []time.Time{time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	failed := errors.New("vendor down")
	read := []time.Time{}
	resume, err := walkMonths(Cloud{}, months, len(months), func(month time.Time) error {
		read = append(read, month)
		if len(read) == 1 {
			return failed
		}
		return errBudget
	})
	if resume != nil || !errors.Is(err, failed) || !slices.Equal(read, months) {
		t.Fatalf("got %v, %v after reading %v, want the first failure after reading every month", resume, err, read)
	}
}
