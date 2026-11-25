package routing

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/oif"
)

type realtimeContractConfig struct {
	Provider connectors.Config
	Model    string
}

type RealtimeTemplate struct {
	model, profileID, profileRevision string
	webrtc                            bool
}

func (t *RealtimeTemplate) Model() string {
	if t == nil {
		return ""
	}
	return t.model
}

func (t *RealtimeTemplate) WebRTC() bool { return t != nil && t.webrtc }

func realtimeRefuse(code, field, requirement, message string) error {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}

func compileRealtimeContract(c realtimeContractConfig) (*RealtimeTemplate, error) {
	return compileRealtimeTemplate(c, false)
}

func compileWebRTCContract(c realtimeContractConfig) (*RealtimeTemplate, error) {
	return compileRealtimeTemplate(c, true)
}

func realtimeMode(webrtc bool) string {
	if webrtc {
		return "webrtc"
	}
	return "realtime"
}

func compileRealtimeTemplate(c realtimeContractConfig, webrtc bool) (*RealtimeTemplate, error) {
	c.Provider = c.Provider.Clone()
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, realtimeRefuse("target_capability", "/profile", "explicit_profile", "Strict realtime requires a registered versioned provider profile.")
	}
	mode := realtimeMode(webrtc)
	if p.OperationDialect("realtime") != "openai-realtime" ||
		!slices.Contains(p.Operations, "realtime") ||
		!c.Provider.Supports("realtime", "openai", mode) ||
		!p.HostsRealtime(mode) {
		return nil, realtimeRefuse("target_capability", "/profile", "native_realtime_contract", "This provider profile has no strict OpenAI Realtime WebSocket contract.")
	}
	if _, err := c.Provider.RealtimeURL(c.Model); err != nil {
		return nil, realtimeRefuse("resource_affinity", "/model", "realtime_address", "This target cannot address its native realtime model.")
	}
	if webrtc {
		if _, err := c.Provider.RealtimeCallsURL(c.Model); err != nil {
			return nil, realtimeRefuse("resource_affinity", "/model", "realtime_address", "This target cannot address its native realtime model.")
		}
	}
	if len(c.Provider.SemanticHeaders) > 0 || len(c.Provider.QuerySettings) > 0 {
		return nil, realtimeRefuse("target_capability", "/provider", "unchanged_realtime_handshake", "Strict realtime has no qualified provider handshake overrides or implicit defaults.")
	}
	if defaults, ok := c.Provider.OperationDefaults["realtime"]; ok && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
		return nil, realtimeRefuse("target_capability", "/operation_defaults/realtime", "native_realtime_defaults", "Realtime defaults cannot be applied to opaque duplex events.")
	}
	if binding, ok := c.Provider.Bindings[c.Model]; ok {
		if defaults, ok := binding.Defaults["realtime"]; ok && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
			return nil, realtimeRefuse("target_capability", "/bindings/realtime", "native_realtime_defaults", "Realtime model defaults cannot be applied to opaque duplex events.")
		}
	}
	return &RealtimeTemplate{model: c.Provider.Model(c.Model), profileID: p.ID, profileRevision: p.Revision, webrtc: webrtc}, nil
}

// AdmitHandshake accepts only route selection and OLP accounting metadata.
func (t *RealtimeTemplate) AdmitHandshake(rawQuery string, headers http.Header, route string) error {
	if t == nil || t.profileID == "" || t.profileRevision == "" || t.model == "" {
		return realtimeRefuse("realtime_control_unavailable", "/profile", "compiled_realtime_contract", "The selected target has no compiled realtime contract.")
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return realtimeRefuse("realtime_control_unavailable", "/query", "unambiguous_realtime_query", "The realtime query is malformed.")
	}
	for name, values := range query {
		switch name {
		case "model":
			if len(values) != 1 || values[0] != route {
				return realtimeRefuse("realtime_control_unavailable", "/query/model", "one_published_route", "Realtime requires exactly one published route model.")
			}
		case "attribution":
			if len(values) != 1 {
				return realtimeRefuse("realtime_control_unavailable", "/query/attribution", "one_attribution", "Realtime accepts at most one attribution value.")
			}
		default:
			return realtimeRefuse("realtime_control_unavailable", "/query/"+name, "native_realtime_query", "This realtime query control has no qualified native binding.")
		}
	}
	if len(query["model"]) != 1 {
		return realtimeRefuse("realtime_control_unavailable", "/query/model", "one_published_route", "Realtime requires exactly one published route model.")
	}
	for name, values := range headers {
		if len(values) != 1 {
			return realtimeRefuse("realtime_control_unavailable", "/headers/"+name, "single_header", "Realtime does not admit repeated handshake headers.")
		}
		switch strings.ToLower(name) {
		case "authorization", "connection", "upgrade", "sec-websocket-key", "sec-websocket-version",
			"sec-websocket-extensions", "origin", "user-agent", "accept-encoding", "x-olp-attribution", "x-olp-end-user", "x-olp-provider-credential", "x-olp-agent-identity",
			"forwarded", "x-forwarded-for", "x-forwarded-host", "x-forwarded-proto", "via",
			"traceparent", "tracestate":
		default:
			return realtimeRefuse("realtime_control_unavailable", "/headers/"+name, "native_realtime_header", "This realtime handshake header has no qualified native binding.")
		}
	}
	if len(headers.Values("Authorization")) != 1 {
		return realtimeRefuse("realtime_control_unavailable", "/headers/Authorization", "one_authorization", "Strict realtime requires one Authorization header.")
	}
	return nil
}
