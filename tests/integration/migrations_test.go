//go:build integration

package integration_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestMigrationsApplyConcurrentlyAndRefuseChangedHistory(t *testing.T) {
	t.Parallel()
	pool, _ := harness.EmptyDatabase(t)
	var group sync.WaitGroup
	failures := make(chan error, 2)
	for range 2 {
		group.Go(func() { failures <- database.Migrate(t.Context(), pool) })
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	first, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	second, err := database.Installation(t.Context(), pool)
	if err != nil || first != second {
		t.Fatalf("installation identity changed from %s to %s: %v", first, second, err)
	}
	if _, err = pool.Exec(t.Context(), "UPDATE olp.migrations SET checksum='changed'"); err != nil {
		t.Fatal(err)
	}
	if database.Migrate(t.Context(), pool) == nil {
		t.Fatal("migrate accepted a changed checksum")
	}
	if _, err := database.Installation(t.Context(), pool); err == nil {
		t.Fatal("installation accepted a changed checksum")
	}
	if _, err = pool.Exec(t.Context(), "DELETE FROM olp.migrations; INSERT INTO olp.migrations(version,checksum) VALUES('0168_retire_allowance_ordering.sql','old')"); err != nil {
		t.Fatal(err)
	}
	if err := database.Migrate(t.Context(), pool); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("migrate upgraded a pre-baseline history: %v", err)
	}
	if _, err := database.Installation(t.Context(), pool); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatalf("installation accepted a pre-baseline history: %v", err)
	}
}

func TestMigrationsIgnoreSessionStatementAndLockTimeouts(t *testing.T) {
	t.Parallel()
	const statementTimeout = time.Second
	const lockTimeout = 100 * time.Millisecond
	holder, address := harness.EmptyDatabase(t)
	config, err := database.Configuration(address, 2, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	config.ConnConfig.RuntimeParams["statement_timeout"] = statementTimeout.String()
	config.ConnConfig.RuntimeParams["lock_timeout"] = lockTimeout.String()
	pool, err := database.Open(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	tx, err := holder.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := tx.Exec(t.Context(), "SELECT pg_advisory_xact_lock(726419823071)"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- database.Migrate(ctx, pool) }()
	holderPID := int32(tx.Conn().PgConn().PID())
	harness.Eventually(t, "migration waiting for the advisory lock", 5*time.Second, func() (bool, string) {
		select {
		case err := <-result:
			t.Fatalf("migration returned before its lock wait was observed: %v", err)
		default:
		}
		var blocked bool
		if err := holder.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database() AND $1 = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		return blocked, "PostgreSQL has not reported the migration blocked by the lock holder"
	})
	select {
	case err := <-result:
		t.Fatalf("migration returned while the advisory lock was held: %v", err)
	case <-time.After(statementTimeout + lockTimeout):
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}
