package process

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestDependencyClosureStartsEvenAfterShutdownBudgetExpires(t *testing.T) {
	deadline := time.Now().Add(-time.Second)
	started, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer func() { close(release); <-closed }()
	closeDependency(&deadline, time.Hour, slog.New(slog.DiscardHandler), "blocked", func(ctx context.Context) error {
		defer close(closed)
		if ctx.Err() == nil {
			t.Error("forced closure acquired a fresh shutdown budget")
		}
		close(started)
		<-release
		return nil
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("expired budget skipped dependency unblocking")
	}
}
