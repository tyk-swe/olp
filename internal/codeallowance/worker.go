package codeallowance

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/usage"
)

const ReadFreshFor = 5 * time.Minute

const (
	readEvery       = 15 * time.Second
	readTimeout     = 30 * time.Second
	readsPerPass    = 50
	allowanceLockID = int64(0x4f4c505f414c5752)
)

type DueAccount struct {
	Account       codemode.Account
	Configuration routing.Configuration
}

type Store interface {
	DueAllowanceReads(ctx context.Context, now time.Time, limit int) ([]DueAccount, error)
	ObserveAllowance(ctx context.Context, account string, allowance codemode.Allowance) error
	CompleteAllowanceRead(ctx context.Context, account string, readAt time.Time) error
}

type Authorizer interface {
	AuthorizeCode(ctx context.Context, cfg routing.Configuration, account codemode.Account, dispatch codemode.Dispatch) (codemode.Authorization, error)
}

type Locker interface {
	TryLock(ctx context.Context, key string, run func() (bool, error)) (bool, bool, error)
}

type Reader struct {
	Pool       *pgxpool.Pool
	Store      Store
	Authorizer Authorizer
	Locker     Locker
	Client     *http.Client
	Log        *slog.Logger
	Now        func() time.Time
}

func (r *Reader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Reader) locker() Locker {
	if r.Locker != nil {
		return r.Locker
	}
	return poolLocker{pool: r.Pool}
}

func Due(account codemode.Account, now time.Time) bool {
	if account.AllowanceReadRequestedAt != nil &&
		(account.AllowanceReadAt == nil || account.AllowanceReadRequestedAt.After(*account.AllowanceReadAt)) {
		return true
	}
	if account.AllowanceReadAt != nil && account.AllowanceReadAt.After(now.Add(-ReadFreshFor)) {
		return false
	}
	if account.Allowance == nil {
		return true
	}
	for _, window := range account.Allowance.Windows {
		if window.EffectiveEvidence() == codemode.AllowanceEvidenceActive && window.ObservedAt.After(now.Add(-ReadFreshFor)) {
			return false
		}
	}
	return true
}

func (r *Reader) Run(ctx context.Context) {
	ticker := time.NewTicker(readEvery)
	defer ticker.Stop()
	for ctx.Err() == nil {
		outcome, progress := r.Pass(ctx)
		if ctx.Err() != nil {
			return
		}
		if r.Pool != nil {
			if err := usage.CheckpointTask(ctx, r.Pool, usage.TaskCodeAllowanceRead, outcome, progress); err != nil {
				r.Log.Warn("code allowance read checkpoint failed", "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (r *Reader) Pass(ctx context.Context) (usage.Outcome, bool) {
	due, err := r.Store.DueAllowanceReads(ctx, r.now(), readsPerPass)
	if err != nil {
		r.Log.Warn("code accounts due an allowance read could not be read", "error", err)
		return usage.OutcomeFailure, false
	}
	progress := false
	for _, entry := range due {
		ran, wrote, err := r.locker().TryLock(ctx, entry.Account.ID, func() (bool, error) {
			return r.read(ctx, entry)
		})
		if err != nil {
			if ctx.Err() == nil {
				r.Log.Warn("code allowance read could not be recorded", "account_id", entry.Account.ID, "error", err)
			}
			return usage.OutcomeFailure, progress
		}
		progress = progress || ran && wrote
	}
	return usage.OutcomeSuccess, progress
}

func (r *Reader) read(ctx context.Context, entry DueAccount) (bool, error) {
	now := r.now()
	if !Due(entry.Account, now) {
		return false, nil
	}
	step, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	allowance, parsed := r.fetch(step, entry, now)
	if parsed {
		if err := r.Store.ObserveAllowance(ctx, entry.Account.ID, allowance); err != nil {
			return false, err
		}
	}
	if err := r.Store.CompleteAllowanceRead(ctx, entry.Account.ID, now); err != nil {
		return false, err
	}
	return parsed, nil
}

func (r *Reader) fetch(ctx context.Context, entry DueAccount, now time.Time) (codemode.Allowance, bool) {
	auth, err := r.Authorizer.AuthorizeCode(ctx, entry.Configuration, entry.Account, codemode.Dispatch{
		Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses, ClientPath: "responses",
	})
	if err != nil {
		r.Log.Warn("code allowance read authorization unavailable", "account_id", entry.Account.ID, "error", err)
		return codemode.Allowance{}, false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, UsageURL, nil)
	if err != nil {
		r.Log.Warn("code allowance read request could not be built", "account_id", entry.Account.ID, "error", err)
		return codemode.Allowance{}, false
	}
	request.Header = auth.Headers.Clone()
	request.Header.Set("Accept", "application/json")
	response, err := r.Client.Do(request)
	if err != nil {
		r.Log.Warn("code allowance read failed", "account_id", entry.Account.ID, "error", err)
		return codemode.Allowance{}, false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxUsageBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || len(body) > maxUsageBytes {
		r.Log.Warn("code allowance read returned no usable usage", "account_id", entry.Account.ID, "status", response.StatusCode)
		return codemode.Allowance{}, false
	}
	allowance, ok := ParseUsage(body, now)
	if !ok {
		r.Log.Warn("code allowance read response did not match the pinned usage schema", "account_id", entry.Account.ID)
		return codemode.Allowance{}, false
	}
	return allowance, true
}

type poolLocker struct{ pool *pgxpool.Pool }

func (l poolLocker) TryLock(ctx context.Context, key string, run func() (bool, error)) (bool, bool, error) {
	if l.pool == nil {
		return false, false, nil
	}
	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, false, err
	}
	defer conn.Release()
	var held bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,$2))`, key, allowanceLockID).Scan(&held); err != nil || !held {
		return false, false, err
	}
	progress, runErr := run()
	_, unlockErr := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1,$2))`, key, allowanceLockID)
	if runErr != nil {
		return true, progress, runErr
	}
	return true, progress, unlockErr
}
