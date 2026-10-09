package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type walCheckpoint struct {
	lsn uint64
	at  time.Time
}

// replicaProgress uses local timestamps of primary WAL observations, not
// replica clocks or pg_last_xact_replay_timestamp (which grows old on a healthy
// idle database). A replay checkpoint can vouch only for primary observations
// it has reached. State is protected by Manager.authorityRefresh.
type replicaProgress struct {
	pending   []walCheckpoint
	confirmed walCheckpoint
}

func (progress *replicaProgress) observe(now time.Time, primary, replay uint64) (time.Time, error) {
	if replay < progress.confirmed.lsn || primary < progress.confirmed.lsn {
		return time.Time{}, ErrStaleAuthority
	}
	if primary <= replay {
		progress.confirmed = walCheckpoint{primary, now}
	} else if len(progress.pending) == 0 || now.Sub(progress.pending[len(progress.pending)-1].at) >= PollInterval {
		progress.pending = append(progress.pending, walCheckpoint{primary, now})
	}
	remaining := progress.pending[:0]
	for _, checkpoint := range progress.pending {
		if checkpoint.lsn <= replay {
			if checkpoint.at.After(progress.confirmed.at) {
				progress.confirmed = checkpoint
			}
		} else if now.Sub(checkpoint.at) <= AuthorityStaleAfter {
			remaining = append(remaining, checkpoint)
		}
	}
	progress.pending = remaining
	if progress.confirmed.at.IsZero() || now.Sub(progress.confirmed.at) > AuthorityStaleAfter {
		return time.Time{}, ErrStaleAuthority
	}
	return progress.confirmed.at, nil
}

func parseLSN(raw string) (uint64, error) {
	high, low, ok := strings.Cut(raw, "/")
	if !ok || high == "" || low == "" {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	h, err := strconv.ParseUint(high, 16, 32)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	l, err := strconv.ParseUint(low, 16, 32)
	if err != nil {
		return 0, errors.New("invalid PostgreSQL WAL position")
	}
	return h<<32 | l, nil
}

func (m *Manager) runtimeReadPool() *pgxpool.Pool {
	if m.ReadPool != nil {
		return m.ReadPool
	}
	return m.pool
}

// authorityReadTime is called before opening the replica's read snapshot, so
// that snapshot includes at least the proven replay position. Monitoring costs
// two constant-size queries per poll, never a query on the admission path.
func (m *Manager) authorityReadTime(ctx context.Context) (time.Time, error) {
	at := time.Now()
	if m.ReadPool == nil {
		return at, nil
	}
	var primary, replay string
	if err := m.pool.QueryRow(ctx, "SELECT pg_current_wal_lsn()::text").Scan(&primary); err != nil {
		return time.Time{}, err
	}
	if err := m.ReadPool.QueryRow(ctx, "SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END::text").Scan(&replay); err != nil {
		return time.Time{}, err
	}
	head, err := parseLSN(primary)
	if err != nil {
		return time.Time{}, err
	}
	applied, err := parseLSN(replay)
	if err != nil {
		return time.Time{}, err
	}
	return m.replica.observe(at, head, applied)
}
