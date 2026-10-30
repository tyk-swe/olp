package providers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codexauth"
	"github.com/tyk-swe/olp/internal/plugins"
	"github.com/tyk-swe/olp/internal/runtime"
	"github.com/tyk-swe/olp/sdk/plugin/abi"

	"github.com/tyk-swe/olp/internal/routing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
)

// Every slot of a provider revision observes one principal: activation
// refuses slots whose credential versions observe different principals,
// naming each slot's, and publishes the principal they share. A revoked
// version serves no more, so its principal counts for neither.
func TestActivationPublishesTheOnePrincipalItsSlotsObserve(t *testing.T) {
	grant := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant}
	slot := func(name, principal string, revoked bool) slotRow {
		return slotRow{ID: name, Name: name, Enabled: true, Weight: 1, CredentialID: new(name + "-credential"), CredentialVersion: new(1),
			CredentialPlugin: "plugin-digest", CredentialPrincipal: principal, CredentialRevoked: revoked}
	}
	// A revoked version, or one whose grant lapsed, serves no more and
	// observes no principal; gateways skip a lapsed one as ineligible.
	lapsed := slot("Lapsed", "former@example.com", false)
	lapsed.CredentialLapsed = true
	pooled := []slotRow{slot("Default", "operator@example.com", false), slot("Backup", "operator@example.com", false), slot("Retired", "former@example.com", true), {ID: "Empty", Name: "Empty", Enabled: true, Weight: 1}, lapsed}
	if err := onePrincipal(pooled, grant); err != nil {
		t.Fatal(err)
	}
	var published []routing.RevisionSlot
	for i := range pooled {
		published = append(published, pooled[i].published(grant.AuthMode))
	}
	if principal := routing.ObservedPrincipal(published); principal != "operator@example.com" || published[1].ObservedPrincipal != principal ||
		published[2].ObservedPrincipal != "" || published[2].Enabled || published[3].ObservedPrincipal != "" || published[4].ObservedPrincipal != "" {
		t.Fatalf("published %+v", published)
	}

	mixed := []slotRow{slot("Default", "operator@example.com", false), slot("Backup", "other@example.com", false)}
	problem, ok := errors.AsType[*access.Problem](onePrincipal(mixed, grant))
	if !ok || problem.Status != 422 || problem.Code != "principal_mismatch" ||
		!strings.Contains(problem.Detail, "slot Default observes operator@example.com; slot Backup observes other@example.com") {
		t.Fatalf("mixed principals: %v", problem)
	}
	static := &Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthStaticCredential}
	if err := onePrincipal([]slotRow{{Name: "Default"}, {Name: "Backup"}}, static); err != nil {
		t.Fatal(err)
	}
}

type codeAuthorizationRow func(...any) error

func (row codeAuthorizationRow) Scan(destination ...any) error { return row(destination...) }

type codeAuthorizationStore struct{ row pgx.Row }

func (store codeAuthorizationStore) QueryRow(context.Context, string, ...any) pgx.Row {
	return store.row
}
func (codeAuthorizationStore) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("fixture only serves individual authority records")
}

type codeAuthorizationSecret struct {
	data        []byte
	generation  int64
	eligibility routing.Eligibility
	err         error
}

func (secret codeAuthorizationSecret) Eligibility(string) routing.Eligibility {
	return secret.eligibility
}
func (secret codeAuthorizationSecret) Secret(context.Context, *runtime.Release, string) ([]byte, int64, error) {
	return secret.data, secret.generation, secret.err
}
func (codeAuthorizationSecret) NetworkSecret(context.Context, *runtime.Release, string, string) ([]byte, error) {
	return nil, errors.New("fixture has no network credential")
}

func codeAuthorizationHost(t *testing.T, manifest abi.Manifest) *plugins.Host {
	t.Helper()
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store := codeAuthorizationStore{row: codeAuthorizationRow(func(destination ...any) error {
		*destination[0].(*[]byte) = encoded
		*destination[1].(*bool) = true
		return nil
	})}
	return plugins.NewHost(nil, nil, store)
}

func codeAuthorizationCredential(t *testing.T, identity codexauth.Identity, generation int64) ([]byte, string) {
	t.Helper()
	claims, err := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "jti": generation,
		"https://api.openai.com/auth": map[string]string{"chatgpt_account_id": identity.Account, "chatgpt_user_id": identity.User}})
	if err != nil {
		t.Fatal(err)
	}
	token := "header." + base64.RawURLEncoding.EncodeToString(claims) + ".signature"
	encoded, err := json.Marshal(connectors.GrantCredential{PluginDigest: "fixture-plugin", ProfileID: codexauth.ProfileID, AccessToken: token, Facts: identity.Facts()})
	if err != nil {
		t.Fatal(err)
	}
	return encoded, token
}

func TestCodeAuthorizationPreservesPrincipalAndRefreshedCredentialGeneration(t *testing.T) {
	identity := codexauth.Identity{Account: "fixture-account", User: "fixture-user"}
	account := codemode.Account{ID: "account", ProviderID: "provider", ProjectID: "project", CredentialID: "credential", Enabled: true, Principal: identity.Principal()}
	configuration := routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, ProfileRevision: "fixture-plugin", Endpoint: codexauth.Upstream}
	authority := codeAuthorizationStore{row: codeAuthorizationRow(func(destination ...any) error { *destination[0].(*bool) = true; return nil })}
	for _, generation := range []int64{1, 2} {
		data, token := codeAuthorizationCredential(t, identity, generation)
		authorizer := CodeAuthorizer{Pool: authority, Credentials: codeAuthorizationSecret{data: data, generation: generation}, Plugins: codeAuthorizationHost(t, codexauth.Manifest())}
		got, err := authorizer.AuthorizeCode(t.Context(), configuration, account, codemode.Dispatch{ClientPath: "responses", Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses})
		if err != nil {
			t.Fatal(err)
		}
		if got.Principal != account.Principal || got.CredentialID != account.CredentialID || got.GrantGeneration != generation || got.Headers.Get("Authorization") != "Bearer "+token || got.Headers.Get("ChatGPT-Account-ID") != identity.Account {
			t.Fatal("authorization lost the bound account or served a different credential generation")
		}
	}
}

func TestCodeAuthorizationRejectsSameNamedForeignPluginAndRequestOverrides(t *testing.T) {
	native := codexauth.Manifest()
	profile := native.Profiles[0]
	foreignOrigin := abi.Manifest{Name: "unrelated-fixture", Version: native.Version, Profiles: native.Profiles, Origins: slices.Concat(native.Origins, []string{"https://foreign.invalid"})}
	foreignProfile := abi.Manifest{Name: "unrelated-fixture", Version: native.Version, Origins: native.Origins,
		Profiles: []abi.Profile{{ID: profile.ID, Label: profile.Label, Dialect: profile.Dialect, Hosting: profile.Hosting, Signing: profile.Signing,
			Grant: &abi.GrantAuthentication{Facts: []string{"account_id"}}}}}
	identity := codexauth.Identity{Account: "fixture-account", User: "fixture-user"}
	data, _ := codeAuthorizationCredential(t, identity, 1)
	account := codemode.Account{CredentialID: "credential", Principal: identity.Principal(), Enabled: true}
	authority := codeAuthorizationStore{row: codeAuthorizationRow(func(destination ...any) error { *destination[0].(*bool) = true; return nil })}
	for name, scenario := range map[string]struct {
		manifest      abi.Manifest
		configuration string
	}{
		"foreign origin":        {foreignOrigin, `{}`},
		"altered grant profile": {foreignProfile, `{}`},
		"foreign endpoint":      {native, `{"endpoint":"https://foreign.invalid"}`},
		"injected headers":      {native, `{"options":{"semantic_headers":{"X-Fixture":"override"}}}`},
		"injected query":        {native, `{"options":{"query_settings":{"fixture":"override"}}}`},
		"plugin options":        {native, `{"options":{"plugin_options":{"fixture":"override"}}}`},
	} {
		t.Run(name, func(t *testing.T) {
			configuration := routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, ProfileRevision: "fixture-plugin", Endpoint: codexauth.Upstream}
			if err := json.Unmarshal([]byte(scenario.configuration), &configuration); err != nil {
				t.Fatal(err)
			}
			authorizer := CodeAuthorizer{Pool: authority, Credentials: codeAuthorizationSecret{data: data, generation: 1}, Plugins: codeAuthorizationHost(t, scenario.manifest)}
			authorization, err := authorizer.AuthorizeCode(t.Context(), configuration, account, codemode.Dispatch{ClientPath: "responses", Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses})
			if err == nil || len(authorization.Headers) != 0 {
				t.Fatal("a foreign plugin or changed request configuration received authorization")
			}
		})
	}
}

func TestCodeAuthorizationRefusesLapsedStaleAndUnprovenCredentials(t *testing.T) {
	identity := codexauth.Identity{Account: "fixture-account", User: "fixture-user"}
	data, _ := codeAuthorizationCredential(t, identity, 2)
	configuration := routing.Configuration{Kind: connectors.KindPlugin, AuthMode: connectors.AuthGrant, ProfileID: codexauth.ProfileID, ProfileRevision: "fixture-plugin", Endpoint: codexauth.Upstream}
	for name, scenario := range map[string]struct {
		eligible  bool
		secret    codeAuthorizationSecret
		principal string
	}{
		"lapsed persisted grant":     {false, codeAuthorizationSecret{data: data, generation: 2}, identity.Principal()},
		"revoked runtime credential": {true, codeAuthorizationSecret{data: data, generation: 2, eligibility: routing.Revoked}, identity.Principal()},
		"stale authority":            {true, codeAuthorizationSecret{data: data, generation: 2, eligibility: routing.StaleAuthority}, identity.Principal()},
		"unproven generation":        {true, codeAuthorizationSecret{data: data}, identity.Principal()},
		"unavailable secret":         {true, codeAuthorizationSecret{err: errors.New("fixture source unavailable")}, identity.Principal()},
		"changed principal":          {true, codeAuthorizationSecret{data: data, generation: 2}, "another-principal"},
	} {
		t.Run(name, func(t *testing.T) {
			authority := codeAuthorizationStore{row: codeAuthorizationRow(func(destination ...any) error { *destination[0].(*bool) = scenario.eligible; return nil })}
			authorizer := CodeAuthorizer{Pool: authority, Credentials: scenario.secret, Plugins: codeAuthorizationHost(t, codexauth.Manifest())}
			account := codemode.Account{CredentialID: "credential", Principal: scenario.principal, Enabled: true}
			authorization, err := authorizer.AuthorizeCode(t.Context(), configuration, account, codemode.Dispatch{ClientPath: "responses", Adapter: codemode.AdapterCodex, Protocol: codemode.ProtocolResponses})
			if err == nil || len(authorization.Headers) != 0 {
				t.Fatal("an unavailable or changed credential was authorized")
			}
		})
	}
}
