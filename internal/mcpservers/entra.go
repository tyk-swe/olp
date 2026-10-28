package mcpservers

import (
	"context"
	"net/url"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

func validateEntra(in *Input) error {
	enabled := in.OAuth != nil && in.OAuth.EntraOBO != nil
	if !enabled {
		if in.EntraClientSecret != nil {
			return access.Invalid("entra_client_secret", "Configure Entra on-behalf-of authorization before supplying a client secret.")
		}
		return nil
	}
	if in.Delegation != nil {
		return access.Invalid("delegation", "Entra access tokens and local delegated JWTs cannot share the downstream authorization header.")
	}
	issuer, err := url.Parse(in.OAuth.EntraOBO.Issuer)
	if err != nil || issuer.Host == "" || (issuer.Scheme != "https" && issuer.Scheme != "http") || issuer.User != nil || issuer.RawQuery != "" || issuer.Fragment != "" {
		return access.Invalid("oauth.entra_obo.issuer", "Supply the exact registered workload issuer URL.")
	}
	if in.EntraClientSecret != nil && (len(*in.EntraClientSecret) == 0 || len(*in.EntraClientSecret) > 16384) {
		return access.Invalid("entra_client_secret", "Supply a nonempty bounded client secret.")
	}
	return nil
}

func (s *Server) saveEntraClientSecret(ctx context.Context, tx pgx.Tx, in Input, previous *Definition) (*string, error) {
	if in.OAuth == nil || in.OAuth.EntraOBO == nil {
		return nil, nil
	}
	if in.EntraClientSecret != nil {
		id := access.NewID()
		material := []byte(*in.EntraClientSecret)
		defer clear(material)
		if err := s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, secrets.MCPEntraClient, material, nil); err != nil {
			return nil, err
		}
		return &id, nil
	}
	if CanRetainEntraClientSecret(previous, in.OAuth) {
		return previous.EntraClientSecretID, nil
	}
	return nil, access.Invalid("entra_client_secret", "Supply this middle-tier client's secret when enabling Entra or changing its issuer, client, or token endpoint.")
}

func CanRetainEntraClientSecret(previous *Definition, next *routing.MCPOAuth) bool {
	if previous == nil || previous.EntraClientSecretID == nil || previous.OAuth == nil || previous.OAuth.EntraOBO == nil || next == nil || next.EntraOBO == nil {
		return false
	}
	return previous.OAuth.ClientID == next.ClientID && previous.OAuth.TokenEndpoint == next.TokenEndpoint && previous.OAuth.EntraOBO.Issuer == next.EntraOBO.Issuer
}
