// Package realtimecontract compiles the narrow same-dialect WebSocket contract
// admitted by strict OpenAI Realtime routes. Event payloads remain native bytes;
// the gateway's bounded relay owns their framing and terminal behavior.
package realtimecontract

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
)

// Config is compiled once for each published strict route target.
type Config struct {
	Provider connectors.Config
	Model    string
	Policy   *contentpolicy.Policy
}

// Template contains only reviewed configuration, never caller-owned event data.
type Template struct {
	model, profileID, profileRevision string
}

func refuse(code, field, requirement, message string) error {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}

// Compile admits only the reviewed GA OpenAI and Azure v1 WebSocket addresses.
// A realtime tuple on a versioned Chat/Responses profile uses its own operation
// dialect; neither generation codec is asked to interpret duplex events.
func Compile(c Config) (*Template, error) {
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, refuse("target_capability", "/profile", "explicit_profile", "Strict realtime requires a registered versioned provider profile.")
	}
	if p.OperationDialect("realtime") != "openai-realtime" ||
		!slices.Contains(p.Operations, "realtime") ||
		!c.Provider.Supports("realtime", "openai", "realtime") ||
		!((p.Kind == "openai" && p.Hosting == "direct-openai") ||
			(p.Kind == "azure_openai" && p.Hosting == "azure-v1")) {
		return nil, refuse("target_capability", "/profile", "native_realtime_contract", "This provider profile has no strict OpenAI Realtime WebSocket contract.")
	}
	if _, err := c.Provider.RealtimeURL(c.Model); err != nil {
		return nil, refuse("resource_affinity", "/model", "realtime_address", "This target cannot address its native realtime model.")
	}
	if c.Policy != nil && len(c.Policy.Rules) > 0 {
		return nil, refuse("policy_conflict", "/content_policy", "inspectable_policy", "Realtime frames cannot be admitted with a content policy this relay cannot enforce.")
	}
	if len(c.Provider.SemanticHeaders) > 0 || len(c.Provider.QuerySettings) > 0 {
		return nil, refuse("target_capability", "/provider", "unchanged_realtime_handshake", "Strict realtime has no qualified provider handshake overrides or implicit defaults.")
	}
	if defaults, ok := c.Provider.OperationDefaults["realtime"]; ok && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
		return nil, refuse("target_capability", "/operation_defaults/realtime", "native_realtime_defaults", "Realtime defaults cannot be applied to opaque duplex events.")
	}
	if binding, ok := c.Provider.Bindings[c.Model]; ok {
		if defaults, ok := binding.Defaults["realtime"]; ok && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
			return nil, refuse("target_capability", "/bindings/realtime", "native_realtime_defaults", "Realtime model defaults cannot be applied to opaque duplex events.")
		}
	}
	return &Template{model: c.Provider.Model(c.Model), profileID: p.ID, profileRevision: p.Revision}, nil
}

// AdmitHandshake accepts only route selection and OLP accounting metadata.
// No caller semantic header, subprotocol, query option, or alternate model may
// disappear at the gateway boundary and silently change the native session.
func (t *Template) AdmitHandshake(rawQuery string, headers http.Header, route string) error {
	if t == nil || t.profileID == "" || t.profileRevision == "" || t.model == "" {
		return refuse("realtime_control_unavailable", "/profile", "compiled_realtime_contract", "The selected target has no compiled realtime contract.")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return refuse("realtime_control_unavailable", "/query", "unambiguous_realtime_query", "The realtime query is malformed.")
	}
	for name, values := range query {
		switch name {
		case "model":
			if len(values) != 1 || values[0] != route {
				return refuse("realtime_control_unavailable", "/query/model", "one_published_route", "Realtime requires exactly one published route model.")
			}
		case "attribution":
			if len(values) != 1 {
				return refuse("realtime_control_unavailable", "/query/attribution", "one_attribution", "Realtime accepts at most one attribution value.")
			}
		default:
			return refuse("realtime_control_unavailable", "/query/"+name, "native_realtime_query", "This realtime query control has no qualified native binding.")
		}
	}
	if len(query["model"]) != 1 {
		return refuse("realtime_control_unavailable", "/query/model", "one_published_route", "Realtime requires exactly one published route model.")
	}
	for name, values := range headers {
		if len(values) != 1 {
			return refuse("realtime_control_unavailable", "/headers/"+name, "single_header", "Realtime does not admit repeated handshake headers.")
		}
		switch strings.ToLower(name) {
		case "authorization", "connection", "upgrade", "sec-websocket-key", "sec-websocket-version",
			"sec-websocket-extensions", "origin", "user-agent", "accept-encoding", "x-olp-attribution",
			"forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via",
			"traceparent", "tracestate":
			// Transport and OLP metadata do not become native session controls.
		default:
			return refuse("realtime_control_unavailable", "/headers/"+name, "native_realtime_header", "This realtime handshake header has no qualified native binding.")
		}
	}
	if len(headers.Values("Authorization")) != 1 {
		return refuse("realtime_control_unavailable", "/headers/Authorization", "one_authorization", "Strict realtime requires one Authorization header.")
	}
	return nil
}
