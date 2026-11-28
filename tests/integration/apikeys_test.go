//go:build integration

package integration_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/apikeys"
	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routes"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestRoutingPolicyWritesMoveOnlyTheirKeysETag(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	ownerID, projectID := keyOwner(t, pool)
	client := managementHTTPAs(t, pool, ownerID, func(mux *http.ServeMux, server *access.Server) {
		apikeys.NewAPI(server, apikeys.NewOverlapRecorder(pool, slog.New(slog.DiscardHandler))).Register(mux)
		(&routes.Server{Access: server}).Register(mux)
	})
	created := func(name string) (string, string) {
		t.Helper()
		reply := expectStatus(t, "create "+name, client.call(t, "POST", "/api/v1/api-keys", "",
			fmt.Sprintf(`{"name":%q,"project_id":%q}`, name, projectID)), http.StatusCreated)
		var key struct{ ID string }
		if err := json.Unmarshal(reply.body, &key); err != nil {
			t.Fatal(err)
		}
		return key.ID, reply.etag
	}
	current := func(id string) string {
		t.Helper()
		return expectStatus(t, "read key", client.call(t, "GET", "/api/v1/api-keys/"+id, "", ""), http.StatusOK).etag
	}
	target, before := created("target")
	bystander, untouched := created("bystander")
	policy := "/api/v1/routing-policies/api-key/" + target
	written := expectStatus(t, "write key policy", client.call(t, "PUT", policy, before, `{"allowed_strategies":[]}`), http.StatusOK)
	if got := current(target); got == before || got != written.etag {
		t.Errorf("key ETag after a policy write = %q, was %q, reply %q", got, before, written.etag)
	}
	if got := current(bystander); got != untouched {
		t.Errorf("a policy write moved another key's ETag from %q to %q", untouched, got)
	}
	expectStatus(t, "write with the stale ETag", client.call(t, "PUT", policy, before, `{"allowed_strategies":[]}`), http.StatusPreconditionFailed)
	expectStatus(t, "delete key policy", client.call(t, "DELETE", policy, written.etag, ""), http.StatusNoContent)
	if got := current(target); got == written.etag {
		t.Errorf("deleting the key policy kept ETag %q", got)
	}
}

func TestWorkloadTokensEnrollOneKeyPerSubjectAcrossReplicas(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	ownerID, projectID := keyOwner(t, pool)
	identity, sign := workloadIssuerKeys(t)
	client := managementHTTPAs(t, pool, ownerID, func(mux *http.ServeMux, server *access.Server) {
		apikeys.NewAPI(server, apikeys.NewOverlapRecorder(pool, slog.New(slog.DiscardHandler))).Register(mux)
	}, func(config access.Config) access.Config {
		config.OIDCClient = identity
		return config
	})
	mustExec(t, pool, `UPDATE olp.projects SET limit_templates='{"workload":{"requests_per_minute":60}}' WHERE id=$1`, projectID)
	reply := expectStatus(t, "register workload issuer", client.call(t, "POST", "/api/v1/workload-issuers", "", fmt.Sprintf(`{"name":"ci","issuer":"https://issuer.example","jwks_url":"https://issuer.example/keys","enabled":true,"audiences":["gateway"],"algorithms":["ES256"],"max_lifetime_seconds":600,"disabled_key_ids":[],"mappings":[{"name":"worker","match":{},"project_id":%q,"limit_template":"workload","route_groups":[],"scopes":["tools"],"end_user_claim":null,"allow_provider_state":false}]}`, projectID)), http.StatusCreated)
	var issuer struct{ ID string }
	if err := json.Unmarshal(reply.body, &issuer); err != nil {
		t.Fatal(err)
	}
	replica := func() *release.Manager {
		t.Helper()
		manager := release.NewManager(release.ManagerConfig{
			Pool: pool, Installation: client.access.Installation, Auth: client.auth, Keys: client.access.Keys,
			Log: slog.New(slog.DiscardHandler), Hostname: uuid.NewString(), IdentityClient: identity,
		})
		if err := manager.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return manager
	}
	authenticate := func(manager *release.Manager, subject string) *keyauthority.Authority {
		t.Helper()
		authority, err := manager.Authenticate(sign(subject))
		if err != nil {
			t.Fatalf("authenticate %s: %v", subject, err)
		}
		return authority
	}
	enrolled := func() int {
		t.Helper()
		var count int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.api_keys WHERE workload_issuer_id=$1", issuer.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	winner, loser := replica(), replica()
	first := authenticate(winner, "builder")
	var project, mapping, creator, scopes string
	if err := pool.QueryRow(t.Context(), "SELECT project_id::text,workload_mapping,created_by::text,policy->>'scopes' FROM olp.api_keys WHERE id=$1 AND lookup_id=$2 AND workload_issuer_id=$3",
		first.ID, first.LookupID, issuer.ID).Scan(&project, &mapping, &creator, &scopes); err != nil {
		t.Fatal(err)
	}
	if project != projectID || mapping != "worker" || creator != ownerID || scopes != `["tools"]` {
		t.Errorf("enrolled key project=%s mapping=%s creator=%s scopes=%s", project, mapping, creator, scopes)
	}
	if again := authenticate(loser, "builder"); again.ID != first.ID || enrolled() != 1 {
		t.Errorf("a second replica enrolled key %s beside %s, keys=%d", again.ID, first.ID, enrolled())
	}
	if other := authenticate(loser, "deployer"); other.ID == first.ID || enrolled() != 2 {
		t.Errorf("a second subject got key %s, first subject has %s, keys=%d", other.ID, first.ID, enrolled())
	}
}

func keyOwner(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	ownerID, projectID := uuid.NewString(), uuid.NewString()
	mustExec(t, pool, `INSERT INTO olp.users(id,email,display_name,role,etag) VALUES($1::uuid,$1::text||'@example.com','API key owner','owner',$1::uuid)`, ownerID)
	mustExec(t, pool, `INSERT INTO olp.projects(id,name,etag,created_by) VALUES($1::uuid,$1::text,$1::uuid,$2)`, projectID, ownerID)
	return ownerID, projectID
}

func workloadIssuerKeys(t *testing.T) (*http.Client, func(subject string) string) {
	t.Helper()
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	published, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &private.PublicKey, KeyID: "signing", Algorithm: string(jose.ES256), Use: "sig"}}})
	if err != nil {
		t.Fatal(err)
	}
	identity := &http.Client{Transport: identityTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(published))), Request: r}, nil
	})}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: private, KeyID: "signing"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return identity, func(subject string) string {
		t.Helper()
		now := time.Now()
		claims, err := json.Marshal(map[string]any{"iss": "https://issuer.example", "sub": subject, "aud": "gateway", "iat": now.Add(-time.Second).Unix(), "exp": now.Add(5 * time.Minute).Unix()})
		if err != nil {
			t.Fatal(err)
		}
		signed, err := signer.Sign(claims)
		if err != nil {
			t.Fatal(err)
		}
		token, err := signed.CompactSerialize()
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
}
