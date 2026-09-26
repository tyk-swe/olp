package limits

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// OutagePolicy decides what happens to a request when Valkey cannot answer.
type OutagePolicy int

// The policies an operator can choose. FailClosed is the zero value, so a
// policy that was never loaded rejects rather than admits.
const (
	// FailClosed rejects the request: no replica can tell whether the budget
	// still has room.
	FailClosed OutagePolicy = iota
	// FailOpen admits the request. It only applies to rate and concurrency
	// limits; spending is never admitted against an unknown balance.
	FailOpen
)

// String names the policy as it is stored and displayed.
func (p OutagePolicy) String() string {
	if p == FailOpen {
		return "fail_open"
	}
	return "fail_closed"
}

// outagePolicySetting is the settings key an operator changes.
const outagePolicySetting = "limits.valkey_unavailable"

// LoadOutagePolicy reads the configured policy. An unreadable or unrecognised
// value is an error rather than a default, so a caller keeps the policy it
// already had instead of silently widening admission.
func LoadOutagePolicy(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (OutagePolicy, error) {
	var value string
	if err := q.QueryRow(ctx, "SELECT value FROM olp.settings WHERE key=$1", outagePolicySetting).Scan(&value); err != nil {
		return FailClosed, err
	}
	switch value {
	case "fail_open":
		return FailOpen, nil
	case "fail_closed":
		return FailClosed, nil
	default:
		return FailClosed, fmt.Errorf("limits: setting %s holds unknown value %q", outagePolicySetting, value)
	}
}
