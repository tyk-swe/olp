package identity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/platform/database"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"golang.org/x/oauth2"
)

type roleMapping struct {
	ClaimValue string `json:"claim_value"`
	Role       string `json:"role"`
}

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

func loadOIDC(ctx context.Context, q database.Queryer) (oidcConfiguration, error) {
	var c oidcConfiguration
	var data []byte
	err := q.QueryRow(ctx, "SELECT c.document||jsonb_build_object('id',c.id,'etag',c.etag,'updated_by_email',u.email,'has_client_secret',EXISTS(SELECT 1 FROM olp.secrets s WHERE s.id=c.id AND s.purpose=$1)) FROM olp.oidc_configuration c JOIN olp.users u ON u.id=c.updated_by WHERE singleton", secrets.OIDCClientSecret).Scan(&data)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

func (s *Server) oidcConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	c, err := loadOIDC(r.Context(), s.access.Pool)
	c.DiscoveryBinding = ""
	return httpkit.Detail(c, c.ETag), err
}

func (s *Server) discover(ctx context.Context, c *oidcConfiguration) (*oidc.Provider, *oauth2.Config, error) {
	if err := access.CheckIdentityURL(c.DiscoveryURL); err != nil {
		return nil, nil, httpkit.Invalid("discovery_url", err.Error())
	}
	request, err := http.NewRequestWithContext(ctx, "GET", c.DiscoveryURL, nil)
	if err != nil {
		return nil, nil, err
	}
	response, err := s.access.IdentityClient().Do(request)
	if err != nil {
		return nil, nil, httpkit.Fail(422, "oidc_discovery_failed", "The OIDC discovery endpoint is unavailable or outside the identity egress policy.")
	}
	defer response.Body.Close()
	var metadata struct {
		oidc.ProviderConfig
		AuthMethods []string `json:"token_endpoint_auth_methods_supported"`
	}
	if response.StatusCode != 200 || json.NewDecoder(response.Body).Decode(&metadata) != nil {
		return nil, nil, httpkit.Invalid("discovery_url", "Invalid OIDC discovery document.")
	}
	if metadata.IssuerURL != c.Issuer {
		return nil, nil, httpkit.Invalid("issuer", "The issuer must exactly match discovery.")
	}
	for _, endpoint := range []string{metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL} {
		if access.CheckIdentityURL(endpoint) != nil {
			return nil, nil, httpkit.Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
		u, _ := url.Parse(endpoint) // Syntax was checked by oidcURL.
		if _, err := access.IdentityAddresses(ctx, u.Hostname()); err != nil {
			return nil, nil, httpkit.Invalid("discovery_url", "The issuer advertises an unsafe endpoint.")
		}
	}
	// Keep the verifier's refresh context independent of this request's
	// cancellation; every fetch still uses the bounded identity HTTP client.
	provider := metadata.NewProvider(oidc.ClientContext(context.Background(), s.access.IdentityClient()))
	oauth := &oauth2.Config{ClientID: c.ClientID, Endpoint: provider.Endpoint(), RedirectURL: s.access.Origin + "/api/v1/oidc/callback", Scopes: c.Scopes}
	switch {
	case metadata.AuthMethods == nil || slices.Contains(metadata.AuthMethods, "client_secret_basic"):
		// OIDC discovery defaults to client_secret_basic when omitted.
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInHeader
	case slices.Contains(metadata.AuthMethods, "client_secret_post"):
		oauth.Endpoint.AuthStyle = oauth2.AuthStyleInParams
	default:
		return nil, nil, httpkit.Invalid("discovery_url", "The token endpoint must support client_secret_basic or client_secret_post.")
	}

	// Bind validated destinations and the selected authentication method to the
	// saved configuration. Remote rediscovery cannot silently authorize a new
	// recipient of the client secret, even before a public login flow starts.
	encoded, _ := json.Marshal([]any{metadata.IssuerURL, metadata.AuthURL, metadata.TokenURL, metadata.JWKSURL, oauth.Endpoint.AuthStyle})
	digest := sha256.Sum256(encoded)
	binding := hex.EncodeToString(digest[:])
	if c.ID != "" && c.DiscoveryBinding != binding {
		return nil, nil, httpkit.Fail(422, "oidc_discovery_changed", "OIDC discovery changed or is not bound. An owner must review and save the configuration again.")
	}
	c.DiscoveryBinding = binding
	return provider, oauth, nil
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

func normalizeOIDCConfigInput(input oidcConfigInput) (oidcConfiguration, error) {
	c := oidcConfiguration{DiscoveryURL: input.DiscoveryURL, Issuer: input.Issuer, ClientID: input.ClientID, Enabled: true, Scopes: input.Scopes, EmailClaim: input.EmailClaim, GroupsClaim: input.GroupsClaim, DefaultRole: input.DefaultRole, EmailMappings: input.EmailMappings, GroupMappings: input.GroupMappings}
	if input.Enabled != nil {
		c.Enabled = *input.Enabled
	}
	if len(c.Scopes) == 0 {
		c.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(c.Scopes, "openid") || len(c.Scopes) > 20 {
		return oidcConfiguration{}, httpkit.Invalid("scopes", "Include openid and at most 20 scopes.")
	}
	for _, scope := range c.Scopes {
		if httpkit.ValidText("scopes", scope, 100) != nil || strings.ContainsAny(scope, " \t\r\n") {
			return oidcConfiguration{}, httpkit.Invalid("scopes", "Use individual valid scope names.")
		}
	}
	if c.EmailClaim == "" {
		c.EmailClaim = "email"
	}
	if c.GroupsClaim == "" {
		c.GroupsClaim = "groups"
	}
	if c.EmailMappings == nil {
		c.EmailMappings = []roleMapping{}
	}
	if c.GroupMappings == nil {
		c.GroupMappings = []roleMapping{}
	}
	for field, value := range map[string]string{"client_id": c.ClientID, "email_claim": c.EmailClaim, "groups_claim": c.GroupsClaim} {
		if err := httpkit.ValidText(field, value, 255); err != nil {
			return oidcConfiguration{}, err
		}
	}
	if c.DefaultRole != nil && !authz.ValidRole(*c.DefaultRole) {
		return oidcConfiguration{}, httpkit.Invalid("default_role", "Use a valid role or leave automatic provisioning disabled.")
	}
	for field, mappings := range map[string][]roleMapping{"email_role_mappings": c.EmailMappings, "group_role_mappings": c.GroupMappings} {
		if len(mappings) > 500 {
			return oidcConfiguration{}, httpkit.Invalid("role_mappings", "Use at most 500 mappings.")
		}
		for i, m := range mappings {
			duplicate := slices.ContainsFunc(mappings[:i], func(previous roleMapping) bool {
				return previous.ClaimValue == m.ClaimValue ||
					(field == "email_role_mappings" && strings.EqualFold(previous.ClaimValue, m.ClaimValue))
			})
			if !authz.ValidRole(m.Role) || httpkit.ValidText("claim_value", m.ClaimValue, 254) != nil || duplicate {
				return oidcConfiguration{}, httpkit.Invalid("role_mappings", "Use unique claim values and valid roles.")
			}
		}
	}
	if input.ClientSecret != nil && len(*input.ClientSecret) > 4096 {
		return oidcConfiguration{}, httpkit.Invalid("client_secret", "Use at most 4096 bytes.")
	}
	return c, nil
}

func (s *Server) saveOIDCClientSecret(ctx context.Context, tx pgx.Tx, c *oidcConfiguration, old oidcConfiguration, secret string) error {
	var previous []byte
	var err error
	if old.HasClientSecret {
		previous, err = s.access.Keys.Read(ctx, tx, s.access.Installation, c.ID, secrets.OIDCClientSecret)
		if err != nil {
			return err
		}
	}
	if !hmac.Equal(previous, []byte(secret)) {
		// Verified identities prove sign-in only with the old credential.
		// Clear that evidence in the same transaction so owner protection
		// requires an independent path until OIDC succeeds again.
		if _, err = tx.Exec(ctx, "UPDATE olp.oidc_identities SET role_claims=NULL WHERE role_claims IS NOT NULL"); err != nil {
			return err
		}
	}
	c.HasClientSecret = secret != ""
	if c.HasClientSecret {
		return s.access.Keys.Store(ctx, tx, s.access.Installation, c.ID, secrets.OIDCClientSecret, []byte(secret), nil)
	}
	_, err = tx.Exec(ctx, "DELETE FROM olp.secrets WHERE id=$1", c.ID)
	return err
}

func (s *Server) putOIDCConfiguration(r *http.Request, _ authz.Principal) (httpkit.Reply, error) {
	var input oidcConfigInput
	if err := httpkit.Decode(r, &input); err != nil {
		return httpkit.Reply{}, err
	}
	c, err := normalizeOIDCConfigInput(input)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if c.Enabled {
		if _, _, err := s.discover(r.Context(), &c); err != nil {
			return httpkit.Reply{}, err
		}
	}
	return access.Transact(s.access, r, access.Unlocked, func(tx pgx.Tx, p authz.Principal, _ struct{}) (httpkit.Reply, access.Event, error) {
		old, err := loadOIDC(r.Context(), tx)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err == nil {
			if err = httpkit.Match(r, old.ETag); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
			c.ID = old.ID
			c.HasClientSecret = old.HasClientSecret
		} else {
			if r.Header.Get("If-Match") != "" {
				return httpkit.Reply{}, access.Event{}, httpkit.Fail(412, "etag_mismatch", "The OIDC configuration does not exist.")
			}
			c.ID = httpkit.NewID()
		}
		c.ETag = httpkit.NewID()
		c.UpdatedByEmail = p.Email
		if input.ClientSecret != nil {
			if err = s.saveOIDCClientSecret(r.Context(), tx, &c, old, *input.ClientSecret); err != nil {
				return httpkit.Reply{}, access.Event{}, err
			}
		}
		data, err := json.Marshal(c)
		if err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "INSERT INTO olp.oidc_configuration(singleton,id,document,etag,updated_by) VALUES(true,$1,$2,$3,$4) ON CONFLICT(singleton) DO UPDATE SET document=excluded.document,etag=excluded.etag,updated_by=excluded.updated_by", c.ID, data, c.ETag, p.UserID()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if err = EnsureUsableOwner(r.Context(), tx, s.access.LocalLoginDisabled()); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		if _, err = tx.Exec(r.Context(), "DELETE FROM olp.secrets WHERE purpose=$1", secrets.OIDCFlow); err != nil {
			return httpkit.Reply{}, access.Event{}, err
		}
		c.DiscoveryBinding = ""
		return httpkit.Detail(c, c.ETag), access.Event{Action: "oidc.configuration.update", Resource: "oidc_configuration", ID: c.ID}, nil
	})
}
