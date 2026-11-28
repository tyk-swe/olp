package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	limitadmission "github.com/tyk-swe/olp/internal/limits/admission"
	limitdebits "github.com/tyk-swe/olp/internal/limits/debits"
	"github.com/tyk-swe/olp/internal/platform/coordination"
	"github.com/tyk-swe/olp/internal/platform/database"
)

type datastores struct {
	pool         *pgxpool.Pool
	readPool     *pgxpool.Pool
	installation string
	prefix       string
	debitStore   *limitdebits.Store
	vk           *coordination.Client
	shared       *sharedLimits
	limiter      *limitadmission.Limiter
	outage       *outagePolicy
	closers      []namedCloser
}

func (d datastores) limitState() sharedLimits {
	if d.shared == nil {
		return sharedLimits{}
	}
	return *d.shared
}

func openDatastores(startup context.Context, c Config, pgConfig *pgxpool.Config, openValkey func(context.Context) (*coordination.Client, error), deadline *time.Time, log *slog.Logger) (datastores, error) {
	var stores datastores
	fail := func(err error) (datastores, error) {
		for i := len(stores.closers) - 1; i >= 0; i-- {
			closer := stores.closers[i]
			closeDependency(deadline, c.ShutdownTimeout, log, closer.name, closer.close)
		}
		return datastores{}, err
	}
	pool, err := database.Open(startup, pgConfig)
	if err != nil {
		return datastores{}, err
	}
	stores.pool = pool
	stores.closers = append(stores.closers, namedCloser{name: "PostgreSQL", close: func(context.Context) error { pool.Close(); return nil }})
	if c.DatabaseReadURL != "" && (c.Mode.Inference() || c.Mode == Worker) {
		readConfig, err := database.Configuration(c.DatabaseReadURL, c.DatabaseMaxConnections, c.RequestTimeout)
		if err != nil {
			return fail(errors.New("invalid OLP_DATABASE_READ_URL"))
		}
		readPool, err := database.Open(startup, readConfig)
		if err != nil {
			return fail(errors.New("runtime read database connection failed"))
		}
		stores.readPool = readPool
		stores.closers = append(stores.closers, namedCloser{name: "runtime read PostgreSQL", close: func(context.Context) error { readPool.Close(); return nil }})
	}
	installation, err := database.Installation(startup, pool)
	if err != nil {
		return fail(err)
	}
	stores.installation = installation
	if stores.readPool != nil {
		readInstallation, err := database.Installation(startup, stores.readPool)
		if err != nil {
			return fail(errors.New("runtime read database is not initialized at the current schema"))
		}
		if readInstallation != installation {
			return fail(errors.New("runtime read database belongs to another installation"))
		}
	}
	// Shared state comes up before anything that admits or accounts for
	// traffic, because both the gateway and the console are told at
	// composition time whether this installation has a limiter at all.
	stores.prefix = coordination.Namespace(installation)
	stores.debitStore = limitdebits.NewStore(pool)
	if openValkey != nil {
		vk, err := openValkey(startup)
		if err != nil {
			return fail(err)
		}
		stores.vk = vk
		stores.closers = append(stores.closers, namedCloser{name: "Valkey", close: func(context.Context) error { vk.Close(); return nil }})
		shared, err := newSharedLimits(vk, stores.prefix+"limits", stores.debitStore)
		if err != nil {
			return fail(err)
		}
		stores.shared = shared
		stores.limiter = shared.Limiter
		if c.Mode.Inference() {
			outage, err := newOutagePolicy(startup, pool, log)
			if err != nil {
				return fail(err)
			}
			stores.outage = outage
		}
	}
	return stores, nil
}
