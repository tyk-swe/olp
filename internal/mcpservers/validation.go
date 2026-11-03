package mcpservers

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/routing"
)

func ValidateAndNormalize(in *Input, policy *egress.Policy) error {
	if err := validateEntra(in); err != nil {
		return err
	}
	if err := validateDelegation(in); err != nil {
		return err
	}
	in.Name = strings.TrimSpace(in.Name)
	if err := access.ValidText("name", in.Name, 100); err != nil {
		return err
	}
	if in.Enabled == nil {
		return access.Invalid("enabled", "Supply an explicit enabled flag.")
	}
	if policy == nil {
		return access.Fail(503, "egress_policy_unavailable", "Certification requires provider egress policy.")
	}
	if in.Transport != "" && !routing.MCPTransport(in.Transport).Valid() {
		return access.Invalid("transport", "Use streamable_http, sse or stdio.")
	}
	if in.Stdio != nil || in.Transport == string(routing.MCPStdio) {
		if !in.Stdio.Valid() || in.Endpoint != "" || in.OAuth != nil || len(in.Credential) != 0 {
			return access.Invalid("stdio", "Pin an installed executable and digest without HTTP credentials or endpoint.")
		}
		if in.Transport != "" && in.Transport != string(routing.MCPStdio) {
			return access.Invalid("transport", "A stdio binding requires stdio transport.")
		}
	} else if in.Endpoint != "" || in.Transport != "" {
		u, err := policy.ValidateEndpoint(in.Endpoint)
		if err != nil {
			return access.Invalid("endpoint", "The MCP endpoint is not permitted by provider egress policy.")
		}
		in.Endpoint = u.String()
	}
	if err := validateOAuth(in.OAuth, policy); err != nil {
		return err
	}
	if in.UnitPrice != nil && !routing.ValidToolPrice(*in.UnitPrice) {
		return access.Invalid("unit_price", "Use a non-negative decimal price with at most 12 integer and 12 fractional digits, or null for no price.")
	}
	if len(in.ToolPrices) > 128 {
		return access.Invalid("tool_prices", "Price at most 128 tools.")
	}
	for name, price := range in.ToolPrices {
		if name == "" || len(name) > 128 || !routing.ValidToolPrice(price) {
			return access.Invalid("tool_prices", "Use certified tool names with non-negative decimal prices.")
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
			return access.Invalid(limit.field, "Stay inside the installation tool call limits.")
		}
	}
	return nil
}

func Credential(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) < 1 || len(value) > 4096 || strings.ContainsAny(value, "\r\n\x00") {
		return nil, access.Invalid("credential", "Supply bounded bearer material, or null to remove it.")
	}
	return []byte(value), nil
}
