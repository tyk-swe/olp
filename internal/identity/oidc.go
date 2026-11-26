package identity

import (
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/domain/ids"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"golang.org/x/oauth2"
)

// Persist only the verified inputs needed to assess future sign-in eligibility.
// These facts are private to authorization and are excluded from API responses.
type oidcRoleClaims struct {
	ClientID    string   `json:"client_id"`
	Scopes      []string `json:"scopes"`
	EmailClaim  string   `json:"email_claim"`
	GroupsClaim string   `json:"groups_claim"`
	Email       string   `json:"email"`
	Groups      []string `json:"groups"`
}

type oidcConfiguration struct {
	DiscoveryBinding string        `json:"discovery_binding,omitempty"`
	ID               string        `json:"id"`
	DiscoveryURL     string        `json:"discovery_url"`
	Issuer           string        `json:"issuer"`
	ClientID         string        `json:"client_id"`
	Enabled          bool          `json:"enabled"`
	Scopes           []string      `json:"scopes"`
	EmailClaim       string        `json:"email_claim"`
	GroupsClaim      string        `json:"groups_claim"`
	DefaultRole      *string       `json:"default_role"`
	EmailMappings    []roleMapping `json:"email_role_mappings"`
	GroupMappings    []roleMapping `json:"group_role_mappings"`
	HasClientSecret  bool          `json:"has_client_secret"`
	ETag             string        `json:"etag"`
	UpdatedByEmail   string        `json:"updated_by_email"`
}

type oidcDiscoveryMetadata struct {
	oidc.ProviderConfig
	AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
}

func loadOIDC(ctx context.Context, q database.Queryer) (oidcConfiguration, error) {
	var data []byte
	var id, etag, updatedByEmail string
	var hasClientSecret bool
	err := q.QueryRow(
		ctx,
		"SELECT c.document,c.id::text,c.etag::text,u.email,EXISTS(SELECT 1 FROM olp.secrets s WHERE s.id=c.id AND s.purpose=$1) FROM olp.oidc_configuration c JOIN olp.users u ON u.id=c.updated_by WHERE singleton",
		secrets.OIDCClientSecret,
	).Scan(&data, &id, &etag, &updatedByEmail, &hasClientSecret)
	if err != nil {
		return oidcConfiguration{}, err
	}
	var c oidcConfiguration
	if err := json.Unmarshal(data, &c); err != nil {
		return oidcConfiguration{}, err
	}
	c.ID = id
	c.ETag = etag
	c.UpdatedByEmail = updatedByEmail
	c.HasClientSecret = hasClientSecret
	return c, nil
}

func (s *oidcService) register(mux *http.ServeMux) {
	s.access.Public(mux, "GET /api/v1/oidc/login", s.beginOIDCLogin)
	s.access.Public(mux, "POST /api/v1/oidc/login", s.beginOIDCLogin)
	s.access.Public(mux, "GET /api/v1/oidc/callback", s.oidcCallback, access.CrossSiteNavigation())
	routes := map[string]access.Handler{
		"GET /api/v1/oidc/configuration":               s.oidcConfiguration,
		"PUT /api/v1/oidc/configuration":               s.putOIDCConfiguration,
		"POST /api/v1/oidc/link":                       s.beginOIDCLink,
		"POST /api/v1/oidc/reauthenticate":             s.beginOIDCReauthentication,
		"GET /api/v1/oidc/identities":                  s.oidcIdentities,
		"DELETE /api/v1/oidc/identities/{identity_id}": s.unlinkOIDCIdentity,
	}
	for pattern, fn := range routes {
		s.access.Route(mux, pattern, fn)
	}
}

func (s *oidcService) oidcConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	c, err := loadOIDC(r.Context(), s.access.Pool)
	c.DiscoveryBinding = ""
	return httpkit.Detail(c, c.ETag), err
}

func (s *oidcService) fetchOIDCMetadata(ctx context.Context, discoveryURL string) (oidcDiscoveryMetadata, error) {
	if err := access.CheckIdentityURL(discoveryURL); err != nil {
		return oidcDiscoveryMetadata{}, httpkit.Invalid("discovery_url", err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, discoveryURL, nil)
	if err != nil {
		return oidcDiscoveryMetadata{}, err
	}
	response, err := s.access.IdentityClient().Do(request)
	if err != nil {
		return oidcDiscoveryMetadata{}, httpkit.Fail(422, "oidc_discovery_failed", "The OIDC discovery endpoint is unavailable or outside the identity egress policy.")
	}
	defer response.Body.Close()
	var metadata oidcDiscoveryMetadata
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&metadata) != nil {
		return oidcDiscoveryMetadata{}, httpkit.Invalid("discovery_url", "Invalid OIDC discovery document.")
	}
	return metadata, nil
}

func validateOIDCEndpoints(ctx context.Context, metadata oidcDiscoveryMetadata) error {
	for _, endpoint := range []string{metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL} {
		if access.CheckIdentityURL(endpoint) != nil {
			return httpkit.Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return httpkit.Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
		if _, err := access.IdentityAddresses(ctx, parsed.Hostname()); err != nil {
			return httpkit.Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
	}
	return nil
}

func selectOIDCAuthStyle(authMethods []string) (oauth2.AuthStyle, error) {
	switch {
	case authMethods == nil || slices.Contains(authMethods, "client_secret_basic"):
		// OIDC discovery defaults to client_secret_basic when omitted.
		return oauth2.AuthStyleInHeader, nil
	case slices.Contains(authMethods, "client_secret_post"):
		return oauth2.AuthStyleInParams, nil
	default:
		return 0, httpkit.Invalid("discovery_url", "The token endpoint must support client_secret_basic or client_secret_post.")
	}
}

// Bind validated destinations and the selected authentication method using a
// canonical JSON array [issuer, auth, token, jwks, authStyle] SHA-256 digest.
func oidcDiscoveryBinding(issuerURL, authURL, tokenURL, jwksURL string, authStyle oauth2.AuthStyle) (string, error) {
	fields := [4]string{issuerURL, authURL, tokenURL, jwksURL}
	buf := make([]byte, 0, 256)
	buf = append(buf, '[')
	for _, field := range fields {
		encoded, err := json.Marshal(field)
		if err != nil {
			return "", err
		}
		buf = append(buf, encoded...)
		buf = append(buf, ',')
	}
	buf = strconv.AppendInt(buf, int64(authStyle), 10)
	buf = append(buf, ']')
	digest := sha256.Sum256(buf)
	return hex.EncodeToString(digest[:]), nil
}

func (s *oidcService) discover(ctx context.Context, c oidcConfiguration) (*oidc.Provider, *oauth2.Config, string, error) {
	metadata, err := s.fetchOIDCMetadata(ctx, c.DiscoveryURL)
	if err != nil {
		return nil, nil, "", err
	}
	if metadata.IssuerURL != c.Issuer {
		return nil, nil, "", httpkit.Invalid("issuer", "The issuer must exactly match discovery.")
	}
	if err := validateOIDCEndpoints(ctx, metadata); err != nil {
		return nil, nil, "", err
	}
	authStyle, err := selectOIDCAuthStyle(metadata.AuthMethods)
	if err != nil {
		return nil, nil, "", err
	}
	binding, err := oidcDiscoveryBinding(metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL, authStyle)
	if err != nil {
		return nil, nil, "", err
	}
	if c.ID != "" && c.DiscoveryBinding != binding {
		return nil, nil, "", httpkit.Fail(422, "oidc_discovery_changed", "OIDC discovery changed or is not bound. An owner must review and save the configuration again.")
	}
	// Keep the verifier's refresh context independent of this request's
	// cancellation; every fetch still uses the bounded identity HTTP client.
	provider := metadata.NewProvider(oidc.ClientContext(context.Background(), s.access.IdentityClient()))
	endpoint := provider.Endpoint()
	endpoint.AuthStyle = authStyle
	oauth := &oauth2.Config{
		ClientID:    c.ClientID,
		Endpoint:    endpoint,
		RedirectURL: s.access.Origin + "/api/v1/oidc/callback",
		Scopes:      c.Scopes,
	}
	return provider, oauth, binding, nil
}

type oidcConfigInput struct {
	DiscoveryURL  string        `json:"discovery_url"`
	Issuer        string        `json:"issuer"`
	ClientID      string        `json:"client_id"`
	ClientSecret  *string       `json:"client_secret"`
	Enabled       *bool         `json:"enabled"`
	Scopes        []string      `json:"scopes"`
	EmailClaim    string        `json:"email_claim"`
	GroupsClaim   string        `json:"groups_claim"`
	DefaultRole   *string       `json:"default_role"`
	EmailMappings []roleMapping `json:"email_role_mappings"`
	GroupMappings []roleMapping `json:"group_role_mappings"`
}

func normalizeOIDCScopes(raw []string) ([]string, error) {
	scopes := raw
	if len(scopes) == 0 {
		scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(scopes, "openid") || len(scopes) > 20 {
		return nil, httpkit.Invalid("scopes", "Include openid and at most 20 scopes.")
	}
	for _, scope := range scopes {
		if httpkit.ValidText("scopes", scope, 100) != nil || strings.ContainsAny(scope, " \t\r\n") {
			return nil, httpkit.Invalid("scopes", "Use individual valid scope names.")
		}
	}
	return scopes, nil
}

func nonNilRoleMappings(entries []roleMapping) []roleMapping {
	if entries == nil {
		return []roleMapping{}
	}
	return entries
}

func validateOIDCConfigFields(c oidcConfiguration, clientSecret *string) error {
	textFields := []struct{ field, value string }{
		{"client_id", c.ClientID},
		{"email_claim", c.EmailClaim},
		{"groups_claim", c.GroupsClaim},
	}
	for _, item := range textFields {
		if err := httpkit.ValidText(item.field, item.value, 255); err != nil {
			return err
		}
	}
	if c.DefaultRole != nil && !authz.ValidRole(*c.DefaultRole) {
		return httpkit.Invalid("default_role", "Use a valid role or leave automatic provisioning disabled.")
	}
	if err := validateRoleMappings(c.EmailMappings, c.GroupMappings, "role_mappings", "role_mappings", "Use unique claim values and valid roles."); err != nil {
		return err
	}
	if clientSecret != nil && len(*clientSecret) > 4096 {
		return httpkit.Invalid("client_secret", "Use at most 4096 bytes.")
	}
	return nil
}

func normalizeOIDCConfigInput(input oidcConfigInput) (oidcConfiguration, error) {
	scopes, err := normalizeOIDCScopes(input.Scopes)
	if err != nil {
		return oidcConfiguration{}, err
	}
	enabled := true
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	c := oidcConfiguration{
		DiscoveryURL:  input.DiscoveryURL,
		Issuer:        input.Issuer,
		ClientID:      input.ClientID,
		Enabled:       enabled,
		Scopes:        scopes,
		EmailClaim:    cmp.Or(input.EmailClaim, "email"),
		GroupsClaim:   cmp.Or(input.GroupsClaim, "groups"),
		DefaultRole:   input.DefaultRole,
		EmailMappings: nonNilRoleMappings(input.EmailMappings),
		GroupMappings: nonNilRoleMappings(input.GroupMappings),
	}
	if err := validateOIDCConfigFields(c, input.ClientSecret); err != nil {
		return oidcConfiguration{}, err
	}
	return c, nil
}

func (s *oidcService) saveOIDCClientSecret(ctx context.Context, tx pgx.Tx, configID string, hadSecret bool, secret string) (bool, error) {
	var previous []byte
	var err error
	if hadSecret {
		previous, err = s.access.Keys.Read(ctx, tx, s.access.Installation, configID, secrets.OIDCClientSecret)
		if err != nil {
			return false, err
		}
	}
	if !hmac.Equal(previous, []byte(secret)) {
		// Verified identities prove sign-in only with the old credential.
		// Clear that evidence in the same transaction so owner protection
		// requires an independent path until OIDC succeeds again.
		if _, err = tx.Exec(ctx, "UPDATE olp.oidc_identities SET role_claims=NULL WHERE role_claims IS NOT NULL"); err != nil {
			return false, err
		}
	}
	hasSecret := secret != ""
	if hasSecret {
		return true, s.access.Keys.Store(ctx, tx, s.access.Installation, configID, secrets.OIDCClientSecret, []byte(secret), nil)
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1", configID)
	return false, err
}

func (s *oidcService) putOIDCConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input oidcConfigInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	c, err := normalizeOIDCConfigInput(input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if c.Enabled {
		_, _, binding, err := s.discover(r.Context(), c)
		if err != nil {
			return httpkit.Reply{}, err
		}
		c.DiscoveryBinding = binding
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		old, err := loadOIDC(r.Context(), tx)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return httpkit.Reply{}, access.Event{}, err
		}
		next := c
		if err == nil {
			if err = httpkit.Match(r, old.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			next.ID = old.ID
			next.HasClientSecret = old.HasClientSecret
		} else {
			if r.Header.Get("If-Match") != "" {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(412, "etag_mismatch", "The OIDC configuration does not exist.")
			}
			next.ID = ids.New()
		}
		next.ETag = ids.New()
		next.UpdatedByEmail = p.Email
		if input.ClientSecret != nil {
			hasSecret, err := s.saveOIDCClientSecret(r.Context(), tx, next.ID, old.HasClientSecret, *input.ClientSecret)
			if err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			next.HasClientSecret = hasSecret
		}
		data, err := json.Marshal(next)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by", next.ID, data, next.ETag, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE purpose=$1", secrets.OIDCFlow); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		next.DiscoveryBinding = ""
		return httpkit.Detail(next, next.ETag), access.Event{Action: "oidc.configuration.update", Resource: "oidc_configuration", ID: next.ID}, nil
	})
}
