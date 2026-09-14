package process

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/limits"
)

// outagePollInterval is how often a replica re-reads the installation's choice
// of what an unreachable limiter does to traffic. Admission consults the policy
// on every request, so it is polled here instead of queried there, and an
// operator's change reaches every replica within one interval.
const outagePollInterval = 15 * time.Second

// outagePolicy holds the limits.OutagePolicy in force for this process.
type outagePolicy struct {
	pool  *pgxpool.Pool
	log   *slog.Logger
	value atomic.Int32
}

// newOutagePolicy reads the stored policy once, before any listener binds, so
// no request is ever admitted or refused under a guessed policy. A setting that
// is unreadable or holds a value this build does not understand stops startup
// rather than guessing which way the operator meant admission to fail.
func newOutagePolicy(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) (*outagePolicy, error) {
	p := &outagePolicy{pool: pool, log: log}
	current, err := p.read(ctx)
	if err != nil {
		return nil, err
	}
	p.value.Store(int32(current))
	return p, nil
}

// read loads the stored policy, treating an installation that has not chosen
// one yet as the default. Setup writes the settings row, so a process that
// starts against a database nobody has set up must still hold the strict
// default rather than refuse to serve.
func (p *outagePolicy) read(ctx context.Context) (limits.OutagePolicy, error) {
	current, err := limits.LoadOutagePolicy(ctx, p.pool)
	if errors.Is(err, pgx.ErrNoRows) {
		return limits.FailClosed, nil
	}
	return current, err
}

// Policy is the policy in force right now, safe to call from every request.
func (p *outagePolicy) Policy() limits.OutagePolicy { return limits.OutagePolicy(p.value.Load()) }

// run keeps the policy current until ctx is cancelled. A poll that fails keeps
// the value already in force: an unreadable setting must never silently widen
// or narrow admission.
func (p *outagePolicy) run(ctx context.Context) {
	ticker := time.NewTicker(outagePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		current, err := p.read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.log.Warn("valkey outage policy could not be read; keeping the policy in force",
				"policy", p.Policy(), "error", err)
			continue
		}
		if previous := p.Policy(); previous != current {
			p.value.Store(int32(current))
			p.log.Info("valkey outage policy changed", "previous", previous, "policy", current)
		}
	}
}
