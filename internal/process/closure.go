package process

import (
	"context"
	"log/slog"
	"time"
)

// closeDependency starts closure even when the shared drain deadline expired.
// Context-aware dependencies use that deadline to force termination; a pool
// close can still wait for an acquired connection whose owner failed to join.
// Such closure remains owned in the background, but must not extend Run's
// termination budget or be reported as proof that the task finished.
func closeDependency(deadline *time.Time, budget time.Duration, log *slog.Logger, name string, close func(context.Context) error) {
	if deadline.IsZero() {
		*deadline = time.Now().Add(budget)
	}
	ctx, cancel := context.WithDeadline(context.Background(), *deadline)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- close(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			log.Warn("dependency did not close cleanly", "dependency", name, "error", err)
		}
	case <-ctx.Done():
		log.Warn("shutdown budget reached before dependency closed", "dependency", name)
	}
}
