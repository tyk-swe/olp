package admission

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type OutagePolicy int

const (
	FailClosed OutagePolicy = iota
	FailOpen
)

func (p OutagePolicy) String() string {
	if p == FailOpen {
		return "fail_open"
	}
	return "fail_closed"
}

const outagePolicySetting = "limits.valkey_unavailable"

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
