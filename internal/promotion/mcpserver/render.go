package mcpserver

import (
	"maps"
	"slices"

	"github.com/oapi-codegen/nullable"
	"github.com/tyk-swe/olp/internal/management/contract"
	"github.com/tyk-swe/olp/internal/promotion"
	"github.com/tyk-swe/olp/internal/routing"
)

type wire struct {
	transport  routing.MCPTransport
	endpoint   string
	stdio      *routing.MCPStdioBinding
	oauth      *routing.MCPOAuth
	delegation *routing.MCPDelegation
	bearer     *promotion.Ref
}

func transportOf(flat wire) Transport {
	if flat.transport == routing.MCPStdio {
		return Stdio{Binding: promotion.ValueOf(flat.stdio)}
	}
	return HTTP{Endpoint: flat.endpoint, SSE: flat.transport == routing.MCPLegacySSE, OAuth: flat.oauth, Delegation: flat.delegation, Bearer: flat.bearer}
}

func flatten(transport Transport) wire {
	switch transport := transport.(type) {
	case Stdio:
		return wire{transport: routing.MCPStdio, stdio: &transport.Binding}
	case HTTP:
		return wire{transport: transport.mode(), endpoint: transport.Endpoint, oauth: transport.OAuth, delegation: transport.Delegation, bearer: transport.Bearer}
	}
	return wire{}
}

func (h HTTP) mode() routing.MCPTransport {
	if h.SSE {
		return routing.MCPLegacySSE
	}
	return routing.MCPStreamableHTTP
}

func Render(servers []Server) *[]contract.ConfigurationMCPServer {
	rendered := slices.Collect(func(yield func(contract.ConfigurationMCPServer) bool) {
		for _, server := range slices.SortedFunc(slices.Values(servers), byKey) {
			if !yield(renderServer(server)) {
				return
			}
		}
	})
	if len(rendered) == 0 {
		return nil
	}
	return &rendered
}

func renderServer(server Server) contract.ConfigurationMCPServer {
	flat := flatten(server.Transport)
	return contract.ConfigurationMCPServer{
		Project:              server.Project,
		Name:                 server.Name,
		Transport:            contract.ConfigurationMCPServerTransport(flat.transport),
		Endpoint:             promotion.NonZero(flat.endpoint),
		Stdio:                renderStdio(flat.stdio),
		Oauth:                renderOAuth(flat.oauth),
		Delegation:           renderDelegation(flat.delegation),
		CredentialRef:        (*string)(flat.bearer),
		Enabled:              server.Enabled,
		CatalogDigest:        server.CatalogDigest,
		UnitPrice:            promotion.Nullable(server.UnitPrice),
		ToolPrices:           promotion.OptionalMap(maps.Clone(server.ToolPrices)),
		CallsPerMinute:       narrow(server.Limits.CallsPerMinute),
		MaxConcurrency:       narrow(server.Limits.MaxConcurrency),
		PerKeyCallsPerMinute: narrow(server.Limits.PerKeyCallsPerMinute),
		PerKeyMaxConcurrency: narrow(server.Limits.PerKeyMaxConcurrency),
	}
}

func renderStdio(binding *routing.MCPStdioBinding) nullable.Nullable[contract.MCPStdioBinding] {
	if binding == nil {
		return nullable.Nullable[contract.MCPStdioBinding]{}
	}
	return nullable.NewNullableWithValue(contract.MCPStdioBinding{Executable: binding.Executable, Digest: binding.Digest})
}

func renderDelegation(delegation *routing.MCPDelegation) nullable.Nullable[contract.MCPDelegationConfiguration] {
	if delegation == nil {
		return nullable.Nullable[contract.MCPDelegationConfiguration]{}
	}
	return nullable.NewNullableWithValue(contract.MCPDelegationConfiguration{Issuer: delegation.Issuer, Audience: delegation.Audience})
}

func renderOAuth(oauth *routing.MCPOAuth) nullable.Nullable[contract.MCPOAuthConfiguration] {
	if oauth == nil {
		return nullable.Nullable[contract.MCPOAuthConfiguration]{}
	}
	return nullable.NewNullableWithValue(contract.MCPOAuthConfiguration{
		ClientId:                    oauth.ClientID,
		Audience:                    oauth.Audience,
		Scopes:                      oauth.Scopes,
		AuthorizationEndpoint:       promotion.NonZero(oauth.AuthorizationEndpoint),
		TokenEndpoint:               oauth.TokenEndpoint,
		DeviceAuthorizationEndpoint: promotion.NonZero(oauth.DeviceAuthorizationEndpoint),
		EntraObo:                    renderEntra(oauth.EntraOBO),
	})
}

func renderEntra(entra *routing.MCPEntraOBO) *contract.MCPEntraOBO {
	if entra == nil {
		return nil
	}
	return &contract.MCPEntraOBO{Issuer: entra.Issuer}
}

func stdioOf(binding nullable.Nullable[contract.MCPStdioBinding]) *routing.MCPStdioBinding {
	value, ok := promotion.Present(binding)
	if !ok {
		return nil
	}
	return &routing.MCPStdioBinding{Executable: value.Executable, Digest: value.Digest}
}

func delegationOf(delegation nullable.Nullable[contract.MCPDelegationConfiguration]) *routing.MCPDelegation {
	value, ok := promotion.Present(delegation)
	if !ok {
		return nil
	}
	return &routing.MCPDelegation{Issuer: value.Issuer, Audience: value.Audience}
}

func oauthOf(oauth nullable.Nullable[contract.MCPOAuthConfiguration]) *routing.MCPOAuth {
	value, ok := promotion.Present(oauth)
	if !ok {
		return nil
	}
	return &routing.MCPOAuth{
		ClientID:                    value.ClientId,
		Audience:                    value.Audience,
		Scopes:                      value.Scopes,
		AuthorizationEndpoint:       promotion.ValueOf(value.AuthorizationEndpoint),
		TokenEndpoint:               value.TokenEndpoint,
		DeviceAuthorizationEndpoint: promotion.ValueOf(value.DeviceAuthorizationEndpoint),
		EntraOBO:                    entraOf(value.EntraObo),
	}
}

func entraOf(entra *contract.MCPEntraOBO) *routing.MCPEntraOBO {
	if entra == nil {
		return nil
	}
	return &routing.MCPEntraOBO{Issuer: entra.Issuer}
}

func widen(value *int) *int64 {
	if value == nil {
		return nil
	}
	wide := int64(*value)
	return &wide
}

func narrow(value int64) *int {
	short := int(value)
	return &short
}

func nonNil(values map[string]string) map[string]string {
	if values == nil {
		return map[string]string{}
	}
	return values
}
