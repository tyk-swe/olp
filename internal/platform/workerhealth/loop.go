package workerhealth

import (
	"context"
	"log/slog"
	"time"
)

type Loop struct {
	Task     Task
	Interval time.Duration
	Budget   time.Duration
	Health   Execer
	Log      *slog.Logger
	Pass     func(context.Context) (progress bool, err error)
}

const checkpointWindow = 2 * time.Second

func Run(ctx context.Context, loop Loop) {
	reporter := New(loop.Task, "")
	for {
		pass, cancel := context.WithTimeout(ctx, loop.Budget)
		progress, err := loop.Pass(pass)
		cancel()
		outcome := OutcomeSuccess
		if err != nil {
			outcome = OutcomeFailure
			loop.Log.Warn("worker pass failed", "task", string(loop.Task), "code", "database")
		}
		checkpoint, done := context.WithTimeout(context.WithoutCancel(ctx), checkpointWindow)
		if err := reporter.Checkpoint(checkpoint, loop.Health, outcome, progress); err != nil {
			loop.Log.Warn("worker checkpoint failed", "task", string(loop.Task), "code", "database")
		}
		done()
		select {
		case <-ctx.Done():
			return
		case <-time.After(loop.Interval):
		}
	}
}
