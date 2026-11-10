package providerinvoke

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/release"
	"github.com/tyk-swe/olp/internal/routing"
)

type bindingCredentials struct {
	secret  func(context.Context, *release.Release, string) ([]byte, int64, error)
	network func(context.Context, *release.Release, string, string) ([]byte, error)
}

func (b bindingCredentials) Eligibility(string) routing.Eligibility { return routing.Eligible }
func (b bindingCredentials) Secret(ctx context.Context, pinned *release.Release, id string) ([]byte, int64, error) {
	return b.secret(ctx, pinned, id)
}
func (b bindingCredentials) NetworkSecret(ctx context.Context, pinned *release.Release, provider, id string) ([]byte, error) {
	return b.network(ctx, pinned, provider, id)
}

func TestBindingResolvesPinnedCredentialsBeforeNetwork(t *testing.T) {
	pinned := &release.Release{}
	credentialID := "api-version"
	events := make(chan string, 4)
	refusal := errors.New("network credential revoked")
	resolver := Resolver{Policy: &egress.Policy{}, HeaderTimeout: time.Second, Credentials: bindingCredentials{
		secret: func(_ context.Context, got *release.Release, id string) ([]byte, int64, error) {
			if got != pinned || id != credentialID {
				t.Fatal("lost pinned credential identity")
			}
			events <- "api"
			return []byte("owned-secret"), 7, nil
		},
		network: func(_ context.Context, got *release.Release, provider, id string) ([]byte, error) {
			if got != pinned || provider != "provider" || id != "network-version" {
				t.Fatal("lost pinned network identity")
			}
			events <- "network"
			return nil, refusal
		},
	}}
	bound := resolver.Bind(pinned, Target{Provider: routing.Provider{ID: "provider", Network: &egress.ConnectionOptions{CredentialID: "network-version"}}, Slot: routing.Slot{CredentialID: &credentialID}}, nil)
	resolved, err := bound.Resolve(t.Context())
	var resolution *ResolutionError
	if !errors.As(err, &resolution) || resolution.Stage != NetworkResolution {
		t.Fatalf("network refusal lost its resolution stage: %v", err)
	}
	if !errors.Is(err, refusal) || resolved.Credential.Generation != 7 || string(resolved.Credential.Secret) != "owned-secret" || resolved.Client != nil {
		t.Fatalf("resolution lost credentials or network refusal: %+v %v", resolved, err)
	}
	if <-events != "api" || <-events != "network" || len(events) != 0 {
		t.Fatal("credential resolution order changed")
	}
}

func TestBindingCallerCredentialDoesNotUseOwnedSecret(t *testing.T) {
	credentialID := "owned-version"
	resolver := Resolver{Policy: &egress.Policy{}, HeaderTimeout: time.Second, DefaultClient: &http.Client{}, Credentials: bindingCredentials{
		secret: func(context.Context, *release.Release, string) ([]byte, int64, error) {
			t.Fatal("resolved an owned credential for the caller")
			return nil, 0, nil
		},
		network: func(context.Context, *release.Release, string, string) ([]byte, error) {
			t.Fatal("resolved an undeclared network credential")
			return nil, nil
		},
	}}
	bound := resolver.Bind(nil, Target{Provider: routing.Provider{ID: "provider", CredentialSource: "caller"}, Slot: routing.Slot{CredentialID: &credentialID}, Model: "served-model"}, func(provider, model string) ([]byte, error) {
		if provider != "provider" || model != "served-model" {
			t.Fatal("caller credential scope changed")
		}
		return []byte("caller-secret"), nil
	})
	resolved, err := bound.Resolve(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer resolved.Client.CloseIdleConnections()
	if string(resolved.Credential.Secret) != "caller-secret" || resolved.Credential.Generation != 0 {
		t.Fatal("caller credential was replaced")
	}
}

func TestBindingExplicitScopeRetainsConnectionValidation(t *testing.T) {
	resolver := Resolver{Policy: &egress.Policy{}, Connections: &egress.ConnectionClientCache{}, DefaultClient: &http.Client{}, Scope: "invalid\nscope"}
	defer resolver.Connections.CloseIdleConnections()
	_, err := resolver.Bind(nil, Target{Provider: routing.Provider{ID: "provider"}}, nil).Resolve(t.Context())
	var resolution *ResolutionError
	if !errors.As(err, &resolution) || resolution.Stage != ConnectionResolution {
		t.Fatalf("explicit scope bypassed connection construction: %v", err)
	}
}
