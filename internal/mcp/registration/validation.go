package registration

import (
	"bytes"
	"maps"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func Prepare(in Input, policy *egress.Policy) (Input, error) {
	if err := validateEntra(&in); err != nil {
		return Input{}, err
	}
	if err := validateDelegation(&in); err != nil {
		return Input{}, err
	}
	name := strings.TrimSpace(in.Name)
	if err := access.ValidText("name", name, 100); err != nil {
		return Input{}, err
	}
	if in.Enabled == nil {
		return Input{}, access.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return Input{}, access.Fail(503, "egress_policy_unavailable", "Certification requires provider egress policy.")
	}
	if in.Transport != "" && !routing.MCPTransport(in.Transport).Valid() {
		return Input{}, access.Invalid("transport", "Use streamable_http, sse or stdio.")
	}
	if in.Stdio != nil || in.Transport == string(routing.MCPStdio) {
		if !in.Stdio.Valid() || in.Endpoint != "" || in.OAuth != nil || len(in.Credential) != 0 {
			return Input{}, access.Invalid("stdio", "Pin an installed executable and digest without HTTP credentials or endpoint.")
		}
		if in.Transport != "" && in.Transport != string(routing.MCPStdio) {
			return Input{}, access.Invalid("transport", "A stdio binding requires stdio transport.")
		}
	}
	endpoint, err := prepareEndpoint(in, policy)
	if err != nil {
		return Input{}, err
	}
	if err := validateOAuth(in.OAuth, policy); err != nil {
		return Input{}, err
	}
	if in.UnitPrice != nil && !routing.ValidToolPrice(*in.UnitPrice) {
		return Input{}, access.Invalid("unit_price", "Use a non-negative decimal price with at most 12 integer and 12 fractional digits, or null for no price.")
	}
	if len(in.ToolPrices) > 128 {
		return Input{}, access.Invalid("tool_prices", "Price at most 128 tools.")
	}
	for name, price := range in.ToolPrices {
		if name == "" || len(name) > 128 || !routing.ValidToolPrice(price) {
			return Input{}, access.Invalid("tool_prices", "Use certified tool names with non-negative decimal prices.")
		}
	}
	for _, limit := range [...]struct {
		field   string
		value   *int64
		ceiling int64
	}{
		{"calls_per_minute", in.CallsPerMinute, routing.MaxToolCallsPerMinute},
		{"per_key_calls_per_minute", in.PerKeyCallsPerMinute, routing.MaxToolCallsPerMinute},
		{"max_concurrency", in.MaxConcurrency, routing.MaxToolConcurrency},
		{"per_key_max_concurrency", in.PerKeyMaxConcurrency, routing.MaxToolConcurrency},
	} {
		if limit.value != nil && (*limit.value < 1 || *limit.value > limit.ceiling) {
			return Input{}, access.Invalid(limit.field, "Stay inside the installation tool call limits.")
		}
	}
	return Input{
		Name: name, Endpoint: endpoint, Transport: in.Transport, ProjectID: in.ProjectID, Enabled: in.Enabled,
		Stdio: in.Stdio.Clone(), OAuth: in.OAuth.Clone(), Delegation: in.Delegation.Clone(),
		Credential: bytes.Clone(in.Credential), EntraClientSecret: in.EntraClientSecret, DelegationPrivateKey: in.DelegationPrivateKey,
		UnitPrice: clonePrice(in.UnitPrice), ToolPrices: cloneToolPrices(in.ToolPrices),
		CallsPerMinute: in.CallsPerMinute, MaxConcurrency: in.MaxConcurrency,
		PerKeyCallsPerMinute: in.PerKeyCallsPerMinute, PerKeyMaxConcurrency: in.PerKeyMaxConcurrency, present: maps.Clone(in.present),
	}, nil
}

func prepareEndpoint(in Input, policy *egress.Policy) (string, error) {
	if in.Stdio != nil || in.Transport == string(routing.MCPStdio) || in.Endpoint == "" && in.Transport == "" {
		return in.Endpoint, nil
	}
	endpoint, err := policy.ValidateEndpoint(in.Endpoint)
	if err != nil {
		return "", access.Invalid("endpoint", "The MCP endpoint is not permitted by provider egress policy.")
	}
	return endpoint.String(), nil
}
