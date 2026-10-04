// Package grants holds grants beneath immutable credential versions and runs
// grant enrollment through provider plugins.
//
// A grant is rotating upstream authorization a provider plugin obtains for an
// operator's upstream account. Grant enrollment creates an ordinary credential
// version that records the plugin, the observed principal and the grant facts,
// and holds the grant beneath it. The version's secret, under the
// provider_credential purpose, is a connectors.GrantCredential: the current
// access token and the facts, which is all the credential source serves
// gateways. The refresh token is kept apart under secrets.ProviderGrantRefresh,
// which gateway code never reads.
package grants

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// Bounds on what a plugin may hand OLP to hold.
const (
	maxToken     = 16 << 10
	maxPrincipal = 256
	maxFact      = 2048
	maxExpiresIn = 10 * 365 * 24 * 60 * 60
)

// Store creates the next credential version of a provider, recording the
// plugin that enrolled a grant, its observed principal and its grant facts,
// and holds the grant beneath it. It returns the version's ID and number.
func Store(ctx context.Context, tx pgx.Tx, a *access.Server, providerID, digest, profileID string, grant abi.Grant) (string, int, error) {
	id := access.NewID()
	facts, err := json.Marshal(grant.Facts)
	if err != nil {
		return "", 0, err
	}
	var version int
	if err = tx.QueryRow(ctx, `INSERT INTO olp.provider_credentials(id,provider_id,version,plugin_digest,principal,grant_facts,profile_id)
		VALUES($1,$2,(SELECT coalesce(max(version),0)+1 FROM olp.provider_credentials WHERE provider_id=$2),$3,$4,$5,$6) RETURNING version`,
		id, providerID, digest, grant.Principal, facts, profileID).Scan(&version); err != nil {
		return "", 0, err
	}
	served, err := json.Marshal(connectors.GrantCredential{AccessToken: grant.AccessToken, Facts: grant.Facts})
	if err != nil {
		return "", 0, err
	}
	if err = a.Keys.Store(ctx, tx, a.Installation, id, secrets.ProviderCredential, served, nil); err != nil {
		return "", 0, err
	}
	var refresh *string
	if grant.RefreshToken != "" {
		refresh = new(access.NewID())
		if err = a.Keys.Store(ctx, tx, a.Installation, *refresh, secrets.ProviderGrantRefresh, []byte(grant.RefreshToken), nil); err != nil {
			return "", 0, err
		}
	}
	expires, refreshAt := schedule(time.Now(), grant.ExpiresIn, refresh != nil)
	_, err = tx.Exec(ctx, "INSERT INTO olp.provider_grants(credential_id,refresh_token_id,expires_at,refresh_at) VALUES($1,$2,$3,$4)", id, refresh, expires, refreshAt)
	return id, version, err
}

// validate checks that a grant a plugin reported for a profile is one OLP can
// hold and place, and gives it an empty fact set when the profile declares
// none.
func validate(declared *abi.GrantAuthentication, grant *abi.Grant) error {
	if err := validateTokens(*grant); err != nil {
		return err
	}
	if !text(grant.Principal, 1, maxPrincipal) {
		return fmt.Errorf("it names no observed principal of at most %d bytes without control characters", maxPrincipal)
	}
	if grant.Facts == nil {
		grant.Facts = map[string]string{}
	}
	for name, value := range grant.Facts {
		if !slices.Contains(declared.Facts, name) {
			return fmt.Errorf("it reports grant fact %q, which the profile does not declare", name)
		}
		if !text(value, 0, maxFact) {
			return fmt.Errorf("grant fact %q exceeds %d bytes or has control characters", name, maxFact)
		}
	}
	for _, name := range declared.Facts {
		if _, reported := grant.Facts[name]; !reported {
			return fmt.Errorf("it does not report grant fact %q, which the profile declares", name)
		}
	}
	return nil
}

// validateTokens checks that OLP can hold a grant's tokens and place its
// access token in a header.
func validateTokens(grant abi.Grant) error {
	switch {
	case grant.AccessToken == "" || len(grant.AccessToken) > maxToken || strings.ContainsAny(grant.AccessToken, "\r\n\x00"):
		return fmt.Errorf("its access token is empty, exceeds 16 KiB or can't be sent in a header")
	case len(grant.RefreshToken) > maxToken:
		return fmt.Errorf("its refresh token exceeds 16 KiB")
	case grant.ExpiresIn < 0 || grant.ExpiresIn > maxExpiresIn:
		return fmt.Errorf("its expiry is not a number of seconds up to ten years")
	}
	return nil
}

func text(value string, least, most int) bool {
	return len(value) >= least && len(value) <= most && utf8.ValidString(value) && !strings.ContainsFunc(value, unicode.IsControl)
}
