package imports

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tyk-swe/olp/internal/billing/source"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/observability/workerhealth"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/usage"
)

const (
	interval    = time.Minute
	passTimeout = 2 * time.Minute
	claimBatch  = 10
)

type Deps struct {
	Pool         *pgxpool.Pool
	Keys         *secrets.KeyRing
	Installation string
	Egress       egress.Policy
	Log          *slog.Logger
	Now          func() time.Time
	S3Endpoint   string
}

type Worker struct {
	deps        Deps
	connections *egress.ConnectionClientCache
	outcomes    *tally
}

func NewWorker(deps Deps) *Worker {
	return &Worker{deps: deps, connections: egress.NewConnectionClientCache(64), outcomes: newTally()}
}

func (w *Worker) Run(ctx context.Context) {
	for {
		passCtx, cancel := context.WithTimeout(ctx, passTimeout)
		progress, err := w.pass(passCtx)
		cancel()
		w.checkpoint(ctx, progress, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

func (w *Worker) Pass(ctx context.Context) error {
	_, err := w.pass(ctx)
	return err
}

func (w *Worker) checkpoint(ctx context.Context, progress bool, err error) {
	outcome := workerhealth.OutcomeSuccess
	if err != nil {
		outcome = workerhealth.OutcomeFailure
		w.deps.Log.Warn("billing import pass failed", "code", "database")
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	if err := workerhealth.New(workerhealth.TaskBillingImport, "").Checkpoint(checkpointCtx, w.deps.Pool, outcome, progress); err != nil {
		w.deps.Log.Warn("billing import checkpoint failed", "code", "database")
	}
}

func (w *Worker) pass(ctx context.Context) (bool, error) {
	for n := range claimBatch {
		r, found, err := claim(ctx, w.deps.Pool)
		if err != nil {
			return n > 0, err
		}
		if !found {
			return n > 0, w.purge(ctx)
		}
		if err := w.importOne(ctx, r); err != nil {
			return true, err
		}
	}
	return true, w.purge(ctx)
}

func (w *Worker) importOne(ctx context.Context, r record) error {
	resume, err := w.read(ctx, r)
	if err != nil {
		code := source.CodeOf(err)
		if err := recordFailure(ctx, w.deps.Pool, r, code); err != nil {
			return err
		}
		w.outcomes.add(r.ProviderID.String(), string(code))
		w.deps.Log.Warn("billing import failed", "provider", r.ProviderID, "code", code)
		return nil
	}
	if err := w.reschedule(ctx, r, resume); err != nil {
		return err
	}
	w.outcomes.add(r.ProviderID.String(), "success")
	return nil
}

func (w *Worker) reschedule(ctx context.Context, r record, resume *source.Resume) error {
	if resume != nil {
		return recordResume(ctx, w.deps.Pool, r, *resume, interval)
	}
	if r.ResumePeriod == nil {
		return nil
	}
	return clearResume(ctx, w.deps.Pool, r)
}

func (w *Worker) read(ctx context.Context, r record) (*source.Resume, error) {
	src, ok := source.Lookup(r.Source)
	if !ok {
		return nil, &source.Failure{Code: source.CodeResponse, Err: errors.New("unsupported billing source")}
	}
	owner, found, err := readProvider(ctx, w.deps.Pool, r.ProviderID.String())
	if err != nil || !found {
		return nil, &source.Failure{Code: source.CodeEndpoint, Err: errors.New("provider configuration could not be read")}
	}
	if r.CredentialID == nil {
		return nil, &source.Failure{Code: source.CodeCredential, Err: errors.New("no billing credential is stored")}
	}
	stored, err := w.deps.Keys.Read(ctx, w.deps.Pool, w.deps.Installation, *r.CredentialID, secrets.BillingCredential)
	if err != nil {
		return nil, &source.Failure{Code: source.CodeCredential, Err: errors.New("billing credential could not be read")}
	}
	imp, err := src.Open(r.Scope, stored)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	digest := sha256.Sum256(stored)
	in := importInputs{
		worker:          w,
		record:          r,
		configuration:   owner.Configuration,
		scope:           imp.Scope,
		connectionScope: "billing/" + r.ProviderID.String() + "/" + hex.EncodeToString(digest[:8]),
	}
	return imp.Read(ctx, in, func(period source.Period) error {
		return storePeriod(ctx, w.deps.Pool, r, imp.Scope, period)
	})
}

type importInputs struct {
	worker          *Worker
	record          record
	configuration   providers.Configuration
	scope           source.Scope
	connectionScope string
}

func (in importInputs) Polling(ctx context.Context) (source.Polling, error) {
	endpoint := in.configuration.Endpoint
	if endpoint == nil || *endpoint == "" {
		return source.Polling{}, &source.Failure{Code: source.CodeEndpoint, Err: errors.New("the provider has no endpoint")}
	}
	network := in.configuration.Options.Network
	secret, err := in.worker.networkSecret(ctx, network)
	if err != nil {
		return source.Polling{}, err
	}
	return source.Polling{
		Today:           in.worker.today(),
		ImportedThrough: in.record.ImportedThrough,
		Endpoint:        *endpoint,
		Client:          in.client(network, secret),
	}, nil
}

func (in importInputs) Cloud(ctx context.Context) (source.Cloud, error) {
	network := in.configuration.Options.Network
	secret, err := in.worker.networkSecret(ctx, network)
	if err != nil {
		return source.Cloud{}, err
	}
	authClient, err := in.worker.deps.Egress.PublicDestinationClient(network, secret, source.RequestTimeout)
	if err != nil {
		return source.Cloud{}, &source.Failure{Code: source.CodeEndpoint, Err: err}
	}
	context.AfterFunc(ctx, authClient.CloseIdleConnections)
	today := in.worker.today()
	retention, err := usage.RetentionDays(ctx, in.worker.deps.Pool)
	if err != nil {
		return source.Cloud{}, err
	}
	earliest := today.AddDate(0, 0, -retention)
	completed, err := completedMonths(ctx, in.worker.deps.Pool, in.record, in.scope, earliest, today)
	if err != nil {
		return source.Cloud{}, err
	}
	return source.Cloud{
		Today:      today,
		Earliest:   earliest,
		Completed:  completed,
		Resume:     in.record.resume(),
		Failures:   in.record.ConsecutiveFailures,
		Client:     in.client(network, secret),
		Auth:       connectors.NewBillingAuth(&in.worker.deps.Egress, authClient),
		S3Endpoint: in.worker.deps.S3Endpoint,
	}, nil
}

func (in importInputs) client(network *egress.ConnectionOptions, secret []byte) func(endpoint string) (*http.Client, error) {
	egressPolicy := in.worker.deps.Egress
	return func(endpoint string) (*http.Client, error) {
		if _, err := egressPolicy.ValidateEndpoint(endpoint); err != nil {
			return nil, err
		}
		return in.worker.connections.ClientScoped(in.connectionScope, egressPolicy, network, secret, source.RequestTimeout)
	}
}

func (w *Worker) today() time.Time { return w.deps.Now().UTC().Truncate(24 * time.Hour) }

func (w *Worker) networkSecret(ctx context.Context, options *egress.ConnectionOptions) ([]byte, error) {
	if options == nil || options.CredentialID == "" {
		return nil, nil
	}
	data, err := w.deps.Keys.Read(ctx, w.deps.Pool, w.deps.Installation, options.CredentialID, secrets.ProviderCredential)
	if err != nil {
		return nil, &source.Failure{Code: source.CodeEndpoint, Err: errors.New("network credential could not be read")}
	}
	return data, nil
}

func (w *Worker) purge(ctx context.Context) error {
	retention, err := usage.RetentionDays(ctx, w.deps.Pool)
	if err != nil {
		return err
	}
	return purge(ctx, w.deps.Pool, w.today().AddDate(0, 0, -retention))
}
