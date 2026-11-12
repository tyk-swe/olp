//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"net/netip"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/providers"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/tests/harness"
)

const providerEndpoint = `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:9/v1"}`

type providerFixture struct {
	pool   *pgxpool.Pool
	server *providers.Server
	owner  string
}

type storedProvider struct {
	providerFixture
	id   string
	name string
	slot string
}

func TestProviderDraftConfigurationWritesKeepEvidenceOnlyWhileItStillApplies(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	f := providerFixture{pool: pool, owner: uuid.NewString(),
		server: providers.New(providers.Deps{Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}})}
	if _, err := pool.Exec(t.Context(), `INSERT INTO olp.users (id,email,display_name,role,etag) VALUES ($1::uuid,$1::text || '@example.com','Provider fixture','owner',$1::uuid)`, f.owner); err != nil {
		t.Fatal(err)
	}
	t.Run("transport change", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending := p.enroll(t, false)
		etag := p.etag(t)
		if err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:9/v1","options":{"parameter_defaults":{"temperature":0.2}}}`); err != nil {
			t.Fatal(err)
		}
		p.wantEvidence(t, "declared")
		if !p.enrolled(t, pending) {
			t.Fatal("a transport change inside the credential boundary cancelled a pending enrollment")
		}
		if p.etag(t) == etag {
			t.Fatal("changed configuration kept its etag")
		}
	})
	t.Run("credential boundary change", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending, continued := p.enroll(t, false), p.enroll(t, true)
		if err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:10/v1"}`); err != nil {
			t.Fatal(err)
		}
		p.wantEvidence(t, "declared")
		if p.enrolled(t, pending) {
			t.Fatal("a pending enrollment or its secret survived a credential boundary change")
		}
		if !p.enrolled(t, continued) {
			t.Fatal("a credential boundary change discarded an enrollment that had already continued")
		}
	})
	t.Run("credential boundary change with a stored credential", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		if _, err := p.pool.Exec(t.Context(), `INSERT INTO olp.provider_credentials(id,provider_id,version) VALUES($1,$2,1)`, uuid.NewString(), p.id); err != nil {
			t.Fatal(err)
		}
		err := p.store(t, p.name, `{"kind":"openai_compatible","auth_mode":"api_key","endpoint":"http://127.0.0.1:10/v1"}`)
		if problem := (*access.Problem)(nil); !errors.As(err, &problem) || problem.Code != "credential_destination_changed" {
			t.Fatalf("moving a stored credential to another endpoint = %v, want credential_destination_changed", err)
		}
	})
	t.Run("unchanged configuration", func(t *testing.T) {
		t.Parallel()
		p := f.certifiedDraft(t)
		pending := p.enroll(t, false)
		etag := p.etag(t)
		if err := p.store(t, p.name, providerEndpoint); err != nil {
			t.Fatal(err)
		}
		if p.etag(t) != etag {
			t.Fatal("writing an unchanged configuration advanced the provider etag")
		}
		if err := p.store(t, p.name+"-renamed", providerEndpoint); err != nil {
			t.Fatal(err)
		}
		if p.etag(t) == etag {
			t.Fatal("renaming the provider kept its etag")
		}
		p.wantEvidence(t, "certified")
		if !p.enrolled(t, pending) {
			t.Fatal("renaming the provider cancelled a pending enrollment")
		}
	})
}

func (f providerFixture) certifiedDraft(t *testing.T) storedProvider {
	t.Helper()
	p := storedProvider{providerFixture: f, name: "provider-" + uuid.NewString(), slot: uuid.NewString()}
	if err := p.transact(t, func(tx pgx.Tx) error {
		prepared, err := connection.Prepare(t.Context(), f.server.ConnectionPolicy(tx), providerConfiguration(t, providerEndpoint))
		if err != nil {
			return err
		}
		created, err := f.server.CreateDraft(t.Context(), tx, providers.Draft{Name: p.name, Configuration: prepared, CreatedBy: f.owner})
		if err != nil {
			return err
		}
		p.id = created.ID
		models := []providers.DraftModel{{UpstreamModel: "fixture-model", Enabled: true, Capabilities: []connection.Capability{
			{Operation: "generation", Surface: "openai", Mode: "unary"}, {Operation: "generation", Surface: "openai", Mode: "streaming"}}}}
		slots := []providers.DraftSlot{{ID: p.slot, Default: true, Name: "default", Enabled: true, Weight: 1}}
		if err := f.server.StageDraftInventory(t.Context(), tx, p.id, models, slots); err != nil {
			return err
		}
		_, err = tx.Exec(t.Context(), `UPDATE olp.provider_models SET capabilities=(SELECT jsonb_agg(c || jsonb_build_object('source','certified','certified_at',now(),'credential_fingerprint','fixture'))
			FROM jsonb_array_elements(capabilities) c) WHERE provider_id=$1`, p.id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p.wantEvidence(t, "certified")
	return p
}

func providerConfiguration(t *testing.T, raw string) connection.Configuration {
	t.Helper()
	var configuration connection.Configuration
	if err := json.Unmarshal([]byte(raw), &configuration); err != nil {
		t.Fatal(err)
	}
	return configuration
}

func (p storedProvider) transact(t *testing.T, fn func(pgx.Tx) error) error {
	t.Helper()
	tx, err := p.pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(t.Context())
}

func (p storedProvider) store(t *testing.T, name, configuration string) error {
	t.Helper()
	return p.transact(t, func(tx pgx.Tx) error {
		prepared, err := connection.Prepare(t.Context(), p.server.ConnectionPolicy(tx), providerConfiguration(t, configuration))
		if err != nil {
			return err
		}
		return p.server.StoreDraftConfiguration(t.Context(), tx, p.id, name, prepared)
	})
}

func (p storedProvider) enroll(t *testing.T, continued bool) string {
	t.Helper()
	id := uuid.NewString()
	if err := p.transact(t, func(tx pgx.Tx) error {
		if _, err := tx.Exec(t.Context(), `INSERT INTO olp.secrets(id,purpose,key_version,ciphertext,expires_at) VALUES($1,'grant_enrollment',1,'\x00',now()+interval '1 hour')`, id); err != nil {
			return err
		}
		_, err := tx.Exec(t.Context(), `INSERT INTO olp.grant_enrollments(id,provider_id,slot_id,plugin_digest,profile_id,started_by,expires_at,continued_at)
			VALUES($1,$2,$3,repeat('0',64),'fixture-profile',$4,now()+interval '1 hour',CASE WHEN $5 THEN now() END)`, id, p.id, p.slot, p.owner, continued)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (p storedProvider) enrolled(t *testing.T, enrollment string) bool {
	t.Helper()
	var rows, secrets int
	if err := p.pool.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM olp.grant_enrollments WHERE id=$1),(SELECT count(*) FROM olp.secrets WHERE id=$1 AND purpose='grant_enrollment')`, enrollment).Scan(&rows, &secrets); err != nil {
		t.Fatal(err)
	}
	if rows != secrets {
		t.Fatalf("enrollment %s has %d rows but %d secrets", enrollment, rows, secrets)
	}
	return rows == 1
}

func (p storedProvider) etag(t *testing.T) string {
	t.Helper()
	var etag string
	if err := p.pool.QueryRow(t.Context(), `SELECT etag::text FROM olp.providers WHERE id=$1`, p.id).Scan(&etag); err != nil {
		t.Fatal(err)
	}
	return etag
}

func (p storedProvider) wantEvidence(t *testing.T, source string) {
	t.Helper()
	var encoded []byte
	if err := p.pool.QueryRow(t.Context(), `SELECT capabilities FROM olp.provider_models WHERE provider_id=$1`, p.id).Scan(&encoded); err != nil {
		t.Fatal(err)
	}
	var capabilities []struct {
		Mode        string  `json:"mode"`
		Source      string  `json:"source"`
		CertifiedAt *string `json:"certified_at"`
	}
	if err := json.Unmarshal(encoded, &capabilities); err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 2 {
		t.Fatalf("capabilities %s, want both declared modes kept", encoded)
	}
	for _, capability := range capabilities {
		if capability.Source != source || (capability.CertifiedAt != nil) != (source == "certified") {
			t.Fatalf("capabilities %s, want every one %s", encoded, source)
		}
	}
}
