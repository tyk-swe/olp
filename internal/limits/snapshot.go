package limits

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Windows struct {
	DailyStart   time.Time
	DailyEnd     time.Time
	DailyID      int64
	WeeklyStart  time.Time
	WeeklyEnd    time.Time
	WeeklyID     int64
	MonthlyStart time.Time
	MonthlyEnd   time.Time
	MonthlyID    int64
}

func (w Windows) bounded() bool {
	return !w.DailyStart.IsZero() || !w.MonthlyStart.IsZero() || !w.WeeklyStart.IsZero()
}

func (w Windows) validate() error {
	for _, id := range [...]int64{w.DailyID, w.WeeklyID, w.MonthlyID} {
		if id < 0 || id > MaxCounter {
			return errors.New("cost snapshot window IDs exceed the Valkey Lua integer range")
		}
	}
	if !w.bounded() {
		return nil
	}
	for _, period := range [...][2]time.Time{{w.DailyStart, w.DailyEnd}, {w.MonthlyStart, w.MonthlyEnd}, {w.WeeklyStart, w.WeeklyEnd}} {
		if period[0].IsZero() || !period[1].After(period[0]) || period[0].UnixMilli() < 0 || period[1].UnixMilli() > MaxCounter {
			return errors.New("invalid budget calendar bounds")
		}
	}
	return nil
}

type Snapshot struct {
	OwnerID          string
	Basis            Basis
	Windows          Windows
	DailyAccrued     string
	WeeklyAccrued    string
	MonthlyAccrued   string
	UnpricedAttempts int64
	RequestID        string
}

func (s Snapshot) Validate() error {
	if !s.Basis.Valid() {
		return fmt.Errorf("cost snapshot basis %q is not valid", s.Basis)
	}
	if _, err := uuid.Parse(s.OwnerID); err != nil {
		return fmt.Errorf("cost snapshot owner ID %q is not a UUID", s.OwnerID)
	}
	if err := s.Windows.validate(); err != nil {
		return err
	}
	if s.WeeklyAccrued != "" && !ValidDecimal(s.WeeklyAccrued) {
		return errors.New("invalid weekly cost snapshot")
	}
	if !ValidDecimal(s.DailyAccrued) {
		return fmt.Errorf("daily accrued cost %q is not a non-negative decimal with at most 12 fractional digits", s.DailyAccrued)
	}
	if !ValidDecimal(s.MonthlyAccrued) {
		return fmt.Errorf("monthly accrued cost %q is not a non-negative decimal with at most 12 fractional digits", s.MonthlyAccrued)
	}
	if s.UnpricedAttempts < 0 || s.UnpricedAttempts > MaxCounter {
		return fmt.Errorf("unpriced attempt count %d exceeds the Valkey Lua integer range", s.UnpricedAttempts)
	}
	if s.RequestID == "" {
		return nil
	}
	if _, err := uuid.Parse(s.RequestID); err != nil {
		return fmt.Errorf("cost snapshot request ID %q is not a UUID", s.RequestID)
	}
	return nil
}

type Installed struct{ Daily, Monthly bool }
