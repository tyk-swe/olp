package main

import (
	"context"
	"log/slog"
	"os"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/gateway"
	"github.com/tyk-swe/olp/internal/media/jobs"
	"github.com/tyk-swe/olp/internal/media/transport"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/staging"
)

type mediaPlane struct {
	spool     *staging.Spool
	transport *transport.Transport
	service   *jobs.Service
	closer    namedCloser
}

func newMediaPlane(c Config, pool *pgxpool.Pool, policy *egress.Policy, credentials release.Credentials, gaps *atomic.Uint64, log *slog.Logger) (mediaPlane, error) {
	spool, err := staging.NewSpool(c.spoolDirectory(), c.MediaSpoolCapacityBytes, log)
	if err != nil {
		return mediaPlane{}, err
	}
	mediaTransport := transport.New(transport.Deps{
		Client:           policy.Client(gateway.UpstreamHeaderTimeout),
		Auth:             connectors.NewAuth(policy, os.Getenv),
		Egress:           policy,
		Spool:            spool,
		MaxResponseBytes: c.ProviderMaxResponseBytes,
		Log:              log,
	})
	return mediaPlane{
		spool:     spool,
		transport: mediaTransport,
		service: jobs.New(jobs.Deps{
			Store:       jobs.NewStore(pool),
			Transport:   mediaTransport,
			Credentials: credentials,
			Gaps:        gaps,
			Log:         log,
			Now:         time.Now,
		}),
		closer: namedCloser{name: "media spool", close: func(context.Context) error { return spool.Close() }},
	}, nil
}
