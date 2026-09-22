package operationplan

import (
	"maps"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/oif"
)

func (t *Template) bindSemantic(context Context, native bool) (connectors.Config, []oif.Disposition, error) {
	config := t.config.Provider
	config.SemanticHeaders = maps.Clone(config.SemanticHeaders)
	config.QuerySettings = maps.Clone(config.QuerySettings)
	if config.SemanticHeaders == nil {
		config.SemanticHeaders = map[string]string{}
	}
	if config.QuerySettings == nil {
		config.QuerySettings = map[string]string{}
	}
	receipts := []oif.Disposition{}
	known := map[string]bool{"Anthropic-Version": true, "Anthropic-Beta": true, "Openai-Beta": true}
	seen := map[string]bool{}
	for name, values := range context.Headers {
		name = textproto.CanonicalMIMEHeaderKey(name)
		if name == "Idempotency-Key" || name == "X-Idempotency-Key" {
			return connectors.Config{}, nil, fail("target_capability", "/headers", "upstream_idempotency", "The caller requested an upstream idempotency contract that this profile has not qualified.")
		}
		if slices.Contains([]string{"Openai-Organization", "Openai-Project", "X-Goog-User-Project", "X-Goog-Request-Params", "X-Ms-Region", "X-Ms-Routing-Name"}, name) {
			return connectors.Config{}, nil, fail("resource_affinity", "/headers", "serving_header", "Caller serving or tenant headers cannot override the published serving identity.")
		}
		if name == "Openai-Version" || name == "Api-Version" || strings.HasPrefix(name, "X-Amzn-Bedrock-") {
			return connectors.Config{}, nil, fail("target_capability", "/headers", "native_semantic_header", "A native semantic header has no admitted mapping in this profile.")
		}
		if !known[name] {
			continue
		}
		if seen[name] || len(values) != 1 || !native {
			return connectors.Config{}, nil, fail("target_capability", "/headers", "semantic_header", "A caller semantic header has no unambiguous mapping in the selected profile.")
		}
		if len(values[0]) > 2048 || !httpguts.ValidHeaderFieldValue(values[0]) {
			return connectors.Config{}, nil, fail("target_capability", "/headers", "semantic_configuration", "Caller semantic settings are malformed or outside the profile revision.")
		}
		seen[name] = true
		if handled, err := t.profile.BindIngressSemanticHeader(name, values[0]); handled {
			if err != nil {
				return connectors.Config{}, nil, fail("target_capability", "/headers/Anthropic-Version", "hosting_api_revision", "The caller API revision has no qualified binding to the selected hosting profile.")
			}
			receipts = append(receipts, oif.Disposition{Field: "/headers/Anthropic-Version", Disposition: "mapped", Rule: "hosting_api_revision", Evidence: t.codec.Evidence})
			continue
		}
		if !slices.Contains(t.profile.SemanticHeaders, name) || name == "Anthropic-Version" && values[0] != t.profile.DialectRevision {
			return connectors.Config{}, nil, fail("target_capability", "/headers", "semantic_header", "A caller semantic header has no unambiguous mapping in the selected profile.")
		}
		for configured, value := range config.SemanticHeaders {
			if strings.EqualFold(configured, name) {
				if value != values[0] {
					return connectors.Config{}, nil, fail("target_capability", "/headers", "semantic_header_conflict", "A caller semantic header conflicts with the published profile.")
				}
				delete(config.SemanticHeaders, configured)
			}
		}
		config.SemanticHeaders[name] = values[0]
		receipts = append(receipts, oif.Disposition{Field: "/headers/" + name, Disposition: "preserved", Rule: "caller_semantic_header", Evidence: t.codec.Evidence})
	}
	for name, values := range context.Query {
		// Gemini chooses SSE at the ingress path. This selector never becomes a
		// configurable semantic query override on the provider URL.

		if len(values) != 1 || !native || !slices.Contains(t.profile.QuerySettings, name) {
			return connectors.Config{}, nil, fail("target_capability", "/query", "semantic_query", "A caller query setting has no mapping in the selected profile.")
		}
		if len(values[0]) > 2048 || !httpguts.ValidHeaderFieldValue(values[0]) || name == "$xgafv" && values[0] != "1" && values[0] != "2" || name == "api-version" && values[0] != config.APIVersion {
			return connectors.Config{}, nil, fail("target_capability", "/query", "semantic_configuration", "Caller semantic settings are malformed or outside the profile revision.")
		}
		if value, present := config.QuerySettings[name]; present && value != values[0] {
			return connectors.Config{}, nil, fail("target_capability", "/query", "semantic_query_conflict", "A caller query setting conflicts with the published profile.")
		}
		config.QuerySettings[name] = values[0]
		receipts = append(receipts, oif.Disposition{Field: "/query/" + name, Disposition: "preserved", Rule: "caller_semantic_query", Evidence: t.codec.Evidence})
	}
	for name := range t.config.Provider.SemanticHeaders {
		if !seen[http.CanonicalHeaderKey(name)] {
			receipts = append(receipts, oif.Disposition{Field: "/headers/" + http.CanonicalHeaderKey(name), Disposition: "introduced", Rule: "profile_semantic_header", Evidence: t.codec.Evidence})
		}
	}
	for name := range t.config.Provider.QuerySettings {
		if _, present := context.Query[name]; !present {
			receipts = append(receipts, oif.Disposition{Field: "/query/" + name, Disposition: "introduced", Rule: "profile_semantic_query", Evidence: t.codec.Evidence})
		}
	}
	if t.profile.Hosting == "direct-anthropic" && !seen["Anthropic-Version"] {
		receipts = append(receipts, oif.Disposition{Field: "/headers/Anthropic-Version", Disposition: "introduced", Rule: "profile_api_revision", Evidence: t.codec.Evidence})
	}
	return config, receipts, nil
}
