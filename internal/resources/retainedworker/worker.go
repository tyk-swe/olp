package retainedworker

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/workerhealth"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/resources"
)

const (
	maxClaimsPerPass         = 16
	maxConcurrentClaims      = 4
	maxWavesPerPass          = maxClaimsPerPass / maxConcurrentClaims
	defaultPassInterval      = 5 * time.Second
	defaultPollTimeout       = 5 * time.Minute
	defaultHeartbeatInterval = 30 * time.Second
	defaultFinishTimeout     = 10 * time.Second
)

type settlementStore interface {
	Claim(ctx context.Context, limit int) ([]resources.RetainedClaim, error)
	Resource(ctx context.Context, c resources.RetainedClaim) (*resources.Resource, error)
	Heartbeat(ctx context.Context, c resources.RetainedClaim) error
	FinishClaim(ctx context.Context, c resources.RetainedClaim, errorClass string) error
	CommitPoll(ctx context.Context, c resources.RetainedClaim, r *resources.Resource, observation resources.RetainedObservation, adopt bool) error
	CloseUndispatched(ctx context.Context, c resources.RetainedClaim) (bool, error)
	HasUnresolvedMutations(ctx context.Context, id uuid.UUID) (bool, error)
	Checkpoint(ctx context.Context, outcome workerhealth.Outcome) error
}

type resourceStore interface {
	GetByUUID(ctx context.Context, id uuid.UUID, owner string) (*resources.Resource, error)
	DiscoverRetainedModels(ctx context.Context, parent *resources.Resource, source oif.Document) error
}

var (
	_ settlementStore = (*resources.SettlementStore)(nil)
	_ resourceStore   = (*resources.Store)(nil)
)

type Worker struct {
	Store       *resources.Store
	Resolver    *resources.Resolver
	Credentials release.Credentials
	Auth        *connectors.Auth
	Egress      *egress.Policy
	Connections *egress.ConnectionClientCache
	Log         *slog.Logger

	settlements       settlementStore
	resources         resourceStore
	heartbeatInterval time.Duration
	now               func() time.Time
}

func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(defaultPassInterval)
	defer ticker.Stop()
	settlements := w.settlementClient()
	for {
		outcome := w.runPass(ctx)
		if settlements != nil {
			_ = settlements.Checkpoint(ctx, outcome)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *Worker) runPass(ctx context.Context) workerhealth.Outcome {
	err := w.Pass(ctx)
	if err == nil {
		return workerhealth.OutcomeSuccess
	}
	if ctx.Err() == nil && w.Log != nil {
		w.Log.Warn("retained reconciliation failed", "error", err)
	}
	return workerhealth.OutcomeFailure
}

func (w *Worker) Pass(ctx context.Context) error {
	settlements := w.settlementClient()
	if settlements == nil {
		return errors.New("settlement store unavailable")
	}
	var failures []error
	for range maxWavesPerPass {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		claims, err := settlements.Claim(ctx, maxConcurrentClaims)
		if err != nil {
			failures = append(failures, err)
			break
		}
		if len(claims) == 0 {
			break
		}
		if waveErr := w.runWave(ctx, settlements, claims); waveErr != nil {
			failures = append(failures, waveErr)
		}
		if len(claims) < maxConcurrentClaims {
			break
		}
	}
	return errors.Join(failures...)
}

func (w *Worker) runWave(ctx context.Context, settlements settlementStore, claims []resources.RetainedClaim) error {
	results := make(chan error, len(claims))
	var wg sync.WaitGroup
	for _, claim := range claims {
		wg.Go(func() {
			results <- w.executeClaim(ctx, settlements, claim)
		})
	}
	wg.Wait()
	close(results)
	return collectErrors(results)
}

func (w *Worker) executeClaim(ctx context.Context, settlements settlementStore, claim resources.RetainedClaim) error {
	class := w.claimClass(ctx, settlements, claim)
	finish, stop := context.WithTimeout(context.WithoutCancel(ctx), defaultFinishTimeout)
	defer stop()
	finishErr := settlements.FinishClaim(finish, claim, class)
	if class != "" {
		return errors.Join(errors.New(class), finishErr)
	}
	return finishErr
}

func (w *Worker) claimClass(ctx context.Context, settlements settlementStore, claim resources.RetainedClaim) string {
	if ctx.Err() != nil {
		return "poll_cancelled"
	}
	return w.pollWithHeartbeat(ctx, settlements, claim)
}

func (w *Worker) pollWithHeartbeat(ctx context.Context, settlements settlementStore, claim resources.RetainedClaim) string {
	work, cancel := context.WithTimeout(ctx, defaultPollTimeout)
	defer cancel()
	done := make(chan struct{})
	go w.runHeartbeat(work, cancel, settlements, claim, done)
	class := w.poll(work, settlements, claim)
	cancel()
	<-done
	return class
}

func (w *Worker) runHeartbeat(ctx context.Context, cancel context.CancelFunc, settlements settlementStore, claim resources.RetainedClaim, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(w.heartbeatPeriod())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if settlements.Heartbeat(ctx, claim) != nil {
				cancel()
				return
			}
		}
	}
}

func (w *Worker) settlementClient() settlementStore {
	if w.settlements != nil {
		return w.settlements
	}
	if w.Store == nil {
		return nil
	}
	return w.Store.Settlements()
}

func (w *Worker) resourceClient() resourceStore {
	if w.resources != nil {
		return w.resources
	}
	if w.Store == nil {
		return nil
	}
	return w.Store
}

func (w *Worker) heartbeatPeriod() time.Duration {
	if w.heartbeatInterval > 0 {
		return w.heartbeatInterval
	}
	return defaultHeartbeatInterval
}

func (w *Worker) utcNow() time.Time {
	if w.now != nil {
		return w.now().UTC()
	}
	return time.Now().UTC()
}

func collectErrors(results <-chan error) error {
	var failures []error
	for result := range results {
		if result != nil {
			failures = append(failures, result)
		}
	}
	return errors.Join(failures...)
}
