package resources

import (
	"context"
	"errors"
)

var ErrPendingUsage = errors.New("cost owner has unsettled background generation usage")

// CheckPendingCostOwner checks current budget scope against the transactionally
// projected subjects of accepted work. It never guesses dollars or creates a
// second event. No unsettled subject becomes a permanently accrued counter.
func (s *Store) CheckPendingCostOwner(ctx context.Context, owner string) error {
	var pending bool
	err := s.pool.QueryRow(ctx, pendingCostOwnerSQL, owner).Scan(&pending)
	if err != nil {
		return err
	}
	if pending {
		return ErrPendingUsage
	}
	return nil
}

const pendingCostOwnerSQL = `SELECT olp.has_pending_resource_usage($1::uuid)`
