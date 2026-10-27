// Package duplexcontract compiles the native-only duplex profiles admitted
// by strict routes for providers whose realtime wire is not the OpenAI
// WebSocket contract. Event payloads stay opaque native frames. The package
// owns only the setup fields each profile must gate (route model binding and
// transcription-only declarations) and each provider's terminal usage
// evidence. The gateway owns sockets, admission and settlement.
package duplexcontract

import (
	"slices"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/oif"
)

const (
	XAIRealtime     = "xai-realtime"
	VertexRealtime  = "vertex-realtime"
	BedrockRealtime = "bedrock-realtime"
)

type Transport string

const (
	TransportWebSocket   Transport = "websocket"
	TransportBedrockBidi Transport = "bedrock-bidi"
)

type Profile struct {
	Dialect           string
	ProfileID         string
	Kind              string
	Hosting           string
	Vendor            string
	TranscriptionOnly bool
	Transport         Transport
}

var profiles = []Profile{
	{Dialect: XAIRealtime, ProfileID: "xai-realtime", Kind: "openai_compatible", Hosting: "xai-realtime", Vendor: "xai", Transport: TransportWebSocket},
	{Dialect: VertexRealtime, ProfileID: "vertex-realtime", Kind: "vertex_ai", Hosting: "vertex-live", Vendor: "google-vertex", Transport: TransportWebSocket},
}

func Profiles() []Profile { return slices.Clone(profiles) }

func Lookup(dialect string) (Profile, bool) {
	for _, p := range profiles {
		if p.Dialect == dialect {
			return p, true
		}
	}
	return Profile{}, false
}

func IsProfile(profileID string) bool {
	for _, p := range profiles {
		if p.ProfileID == profileID {
			return true
		}
	}
	return false
}

type Config struct {
	Provider connectors.Config
	Model    string
	Policy   *contentpolicy.Policy
}

type Template struct {
	profile         Profile
	model           string
	profileRevision string
}

func refuse(code, field, requirement, message string) error {
	return &oif.Incompatibility{Code: code, Field: field, Requirement: requirement, Message: message}
}

func Compile(c Config) (*Template, error) {
	c.Provider = c.Provider.Clone()
	c.Policy = c.Policy.Clone()
	p, err := c.Provider.Profile()
	if err != nil || c.Provider.ValidateProfile() != nil {
		return nil, refuse("target_capability", "/profile", "explicit_profile", "Native duplex requires a registered versioned provider profile.")
	}
	descriptor, ok := Lookup(p.Dialect)
	if !ok || descriptor.ProfileID != p.ID || descriptor.Kind != p.Kind || descriptor.Hosting != p.Hosting ||
		!slices.Contains(p.Operations, "realtime") ||
		!c.Provider.Supports("realtime", "native", "realtime") {
		return nil, refuse("target_capability", "/profile", "native_duplex_contract", "This provider profile has no native duplex contract.")
	}
	if descriptor.Transport == TransportWebSocket {
		if _, urlErr := c.Provider.DuplexURL(c.Model); urlErr != nil {
			return nil, refuse("resource_affinity", "/model", "duplex_address", "This target cannot address its native duplex model.")
		}
		if _, modelErr := c.Provider.DuplexBoundModel(c.Model); modelErr != nil {
			return nil, refuse("resource_affinity", "/model", "duplex_address", "This target cannot address its native duplex model.")
		}
	}
	if c.Policy != nil && len(c.Policy.Rules) > 0 {
		return nil, refuse("policy_conflict", "/content_policy", "inspectable_policy", "Native duplex frames cannot be admitted with a content policy this relay cannot enforce.")
	}
	if len(c.Provider.SemanticHeaders) > 0 || len(c.Provider.QuerySettings) > 0 {
		return nil, refuse("target_capability", "/provider", "unchanged_duplex_handshake", "Native duplex has no qualified provider handshake overrides or implicit defaults.")
	}
	if defaults, present := c.Provider.OperationDefaults["realtime"]; present && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
		return nil, refuse("target_capability", "/operation_defaults/realtime", "native_duplex_defaults", "Realtime defaults cannot be applied to opaque duplex events.")
	}
	if binding, present := c.Provider.Bindings[c.Model]; present {
		if defaults, has := binding.Defaults["realtime"]; has && (len(defaults.Values) > 0 || len(defaults.NativeOptions) > 0) {
			return nil, refuse("target_capability", "/bindings/realtime", "native_duplex_defaults", "Realtime model defaults cannot be applied to opaque duplex events.")
		}
	}
	return &Template{profile: descriptor, model: c.Provider.Model(c.Model), profileRevision: p.Revision}, nil
}

func (t *Template) Dialect() string         { return t.profile.Dialect }
func (t *Template) ProfileID() string       { return t.profile.ProfileID }
func (t *Template) ProfileRevision() string { return t.profileRevision }
func (t *Template) Model() string           { return t.model }
func (t *Template) TranscriptionOnly() bool { return t.profile.TranscriptionOnly }
func (t *Template) Transport() Transport    { return t.profile.Transport }
func (t *Template) Vendor() string          { return t.profile.Vendor }
func (t *Template) SetupAck() bool          { return t.profile.Dialect == VertexRealtime }

type Setup struct {
	Model             string
	TranscriptionOnly bool
}

func (t *Template) AdmitSetup(setup Setup) error { return AdmitProfileSetup(t.profile, setup) }

func (t *Template) InspectSetup(frame []byte, maxBytes int) (Setup, error) {
	return InspectProfileSetup(t.profile, frame, maxBytes)
}

func InspectProfileSetup(profile Profile, frame []byte, maxBytes int) (Setup, error) {
	switch profile.Dialect {
	case XAIRealtime:
		return inspectXAISetup(frame, maxBytes)
	case VertexRealtime:
		return inspectVertexSetup(frame, maxBytes)
	}
	return Setup{}, refuse("target_capability", "/profile", "native_duplex_contract", "This duplex profile has no setup inspector.")
}

func AdmitProfileSetup(profile Profile, setup Setup) error {
	if setup.TranscriptionOnly && !profile.TranscriptionOnly {
		return refuse("transcription_unsupported", "/setup", "declared_transcription", "This duplex profile does not declare transcription-only sessions.")
	}
	return nil
}

func (t *Template) BindSetup(frame []byte, boundModel string) ([]byte, error) {
	switch t.profile.Dialect {
	case XAIRealtime:
		return frame, nil
	case VertexRealtime:
		return bindVertexSetup(frame, boundModel, len(frame)+len(boundModel)+2048)
	}
	return nil, refuse("target_capability", "/profile", "native_duplex_contract", "This duplex profile cannot bind its setup frame.")
}

func parseObject(frame []byte, maxBytes int) (oif.Document, error) {
	if maxBytes < 1 || len(frame) > maxBytes {
		return oif.Document{}, refuse("invalid_duplex_frame", "/frame", "bounded_frame", "The duplex frame exceeds its byte limit.")
	}
	doc, err := oif.ParseJSON(frame, oif.Limits{MaxBytes: maxBytes})
	if err != nil || doc.Root().Kind() != oif.Object {
		return oif.Document{}, refuse("invalid_duplex_frame", "/frame", "json_object_frame", "Native duplex frames must be bounded JSON objects.")
	}
	return doc, nil
}

func textField(parent oif.Value, name string) (string, bool) {
	v, ok := parent.Lookup(name)
	if !ok {
		return "", false
	}
	return v.Text()
}
