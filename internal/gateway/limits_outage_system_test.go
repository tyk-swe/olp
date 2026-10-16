//go:build integration

package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/tyk-swe/olp/internal/coordination"
	"github.com/tyk-swe/olp/internal/limits"
)

// lookupOutage runs every command against Valkey except the script calls that
// name one lookup, which it fails the way an outage does: refused, before
// anything ran. It is how the key's own budgets are made unreachable after the
// budget group's was reserved, since the two live under different lookups.
type lookupOutage struct {
	limits.Commander
	lookup string
}

func (c lookupOutage) Do(ctx context.Context, args ...string) (any, error) {
	if len(args) > 3 && (args[0] == "EVAL" || args[0] == "EVALSHA") && strings.Contains(args[3], "{"+c.lookup+"}") {
		return nil, &coordination.CommandError{Cause: errors.New("connection reset by peer")}
	}
	return c.Commander.Do(ctx, args...)
}

// The request ends having cost less, which is what the group then holds.
