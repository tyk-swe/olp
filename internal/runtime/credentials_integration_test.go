//go:build integration

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/database"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func credentialPool(t *testing.T, tracers ...pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	admin := os.Getenv("OLP_TEST_DATABASE_ADMIN_URL")
	if admin == "" {
		t.Fatal("OLP_TEST_DATABASE_ADMIN_URL is required; run make integration")
	}
	cfg, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	adminPool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{"runtime_" + strings.ReplaceAll(uuid.NewString(), "-", "")}
	if _, err := adminPool.Exec(t.Context(), "CREATE DATABASE "+name.Sanitize()); err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	cfg.ConnConfig.Database, cfg.MaxConns = name[0], 1
	if len(tracers) > 0 {
		cfg.ConnConfig.Tracer = tracers[0]
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		adminPool.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE "+name.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch database: %v", err)
		}
		adminPool.Close()
	})
	if err := database.Migrate(t.Context(), pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestIntegrationHistoricalSecretReleasesPoolBeforeRemoteResolution(t *testing.T) {
	pool := credentialPool(t)
	user, provider, credential := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1,'runtime@example.test','runtime','owner',$2)`, user, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.providers(id,name,kind,state,configuration,etag,slots_etag,created_by)
        VALUES($1,'historical','openai_compatible','draft','{}',$2,$3,$4)`, provider, uuid.NewString(), uuid.NewString(), user); err != nil {
		t.Fatal(err)
	}
	reference := externalReference("1")
	raw, _ := json.Marshal(reference)
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version,external_reference) VALUES($1,$2,1,$3)`, credential, provider, raw); err != nil {
		t.Fatal(err)
	}
	m := credentialManager(t, time.Now())
	m.pool, m.keys = pool, &secrets.KeyRing{}
	installation, err := database.Installation(t.Context(), pool)
	if err != nil {
		t.Fatal(err)
	}
	m.installation = installation
	started, resume := make(chan struct{}), make(chan struct{})
	m.ExternalSecrets = externalResolveFunc(func(context.Context, secretstore.Reference) ([]byte, error) {
		close(started)
		<-resume
		return []byte("historical-secret"), nil
	})
	result := make(chan error, 1)
	go func() {
		secret, _, err := m.Secret(t.Context(), nil, credential)
		if secret != nil {
			t.Error("concurrent revocation returned a historical secret")
		}
		result <- err
	}()
	select {
	case <-started:
	case err := <-result:
		t.Fatal("historical remote resolution did not start", err)
	case <-time.After(5 * time.Second):
		t.Fatal("historical remote resolution did not start within its budget")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx, `UPDATE olp.provider_credentials SET revoked_at=now() WHERE id=$1`, credential); err != nil {
		close(resume)
		<-result
		t.Fatal("remote resolution held the only database connection", err)
	}
	if err := m.refreshAuthority(ctx); err != nil {
		close(resume)
		<-result
		t.Fatal("authority could not progress during resolution", err)
	}
	close(resume)
	if err := <-result; !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatal("remote completion ignored observed revocation", err)
	}
	if m.Eligibility(credential) != Revoked {
		t.Fatal("historical revocation was not retained")
	}
	if err := m.StopContext(t.Context()); err != nil {
		t.Fatal(err)
	}
}
