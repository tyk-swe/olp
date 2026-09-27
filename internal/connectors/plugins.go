package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// KindPlugin is the provider kind of providers whose profile a provider plugin
// supplies. No built-in kind's endpoint, discovery, API-key header or vendor
// prices apply to it: its profile's hosting adaptation places every request.
const KindPlugin = "plugin"

// AuthStaticCredential authenticates a plugin provider with a static
// credential, which its profile's hosting adaptation places.
const AuthStaticCredential = "static_credential"

// pluginHosting is the hosting of every plugin profile: the plugin's declared
// hosting adaptation, which OLP runs.
const pluginHosting = "plugin"

// pluginDialects are the built-in dialects a plugin profile may serve: those
// whose generation requests and events are plain HTTP JSON and SSE, which a
// hosting adaptation places without framing of its own.
var pluginDialects = []string{"openai-chat", "openai-responses", "anthropic-messages", "gemini-generate-content"}

// PluginDialects returns the built-in dialects a plugin profile may serve.
func PluginDialects() []string { return slices.Clone(pluginDialects) }

// Plugin identifies the provider plugin that supplies a profile.
type Plugin struct {
	// Digest is the SHA-256 of the plugin's module, which a provider pins as
	// its profile revision.
	Digest  string `json:"digest"`
	Name    string `json:"name"`
	Version string `json:"version"`
}

// A PluginProfile is a provider profile a provider plugin declares: a
// built-in dialect placed at an upstream by the plugin's hosting adaptation.
// OLP runs the adaptation itself; no plugin code runs per request. Its
// revision is the plugin's digest, so moving a provider to another build of
// the plugin is a new profile revision. A PluginProfile never changes.
type PluginProfile struct {
	profile  Profile
	hosting  hosting
	declared abi.Profile
}

// NewPluginProfile returns the profile id that a plugin's manifest declares,
// identified by the digest of the plugin's module.
func NewPluginProfile(digest string, manifest abi.Manifest, id string) (*PluginProfile, error) {
	for _, declared := range manifest.Profiles {
		if declared.ID == id {
			return newPluginProfile(Plugin{Digest: digest, Name: manifest.Name, Version: manifest.Version}, declared)
		}
	}
	return nil, fmt.Errorf("plugin %s declares no profile %q", manifest.Name, id)
}

// DecodePluginProfile returns the profile id that a plugin's stored manifest
// declares, identified by the digest of the plugin's module.
func DecodePluginProfile(digest string, manifest []byte, id string) (*PluginProfile, error) {
	if manifest == nil {
		return nil, fmt.Errorf("plugin %s is not installed", digest)
	}
	var declared abi.Manifest
	if err := json.Unmarshal(manifest, &declared); err != nil {
		return nil, err
	}
	return NewPluginProfile(digest, declared, id)
}

// ValidatePluginProfile reports what is wrong with a profile a plugin
// declares, as a *ProfileError locating the offending value.
func ValidatePluginProfile(declared abi.Profile) error {
	_, err := newPluginProfile(Plugin{}, declared)
	return err
}

// A ProfileError locates what is wrong with a profile a plugin declares.
type ProfileError struct {
	// Field locates the offending value in the profile, such as
	// hosting.headers.Authorization.
	Field   string
	Message string
}

func (e *ProfileError) Error() string { return e.Field + ": " + e.Message }

func newPluginProfile(plugin Plugin, declared abi.Profile) (*PluginProfile, error) {
	base, ok := dialectProfile(declared.Dialect)
	if !ok {
		return nil, &ProfileError{Field: "dialect", Message: "Serve one of the dialects plugin profiles can serve: " + strings.Join(pluginDialects, ", ") + "."}
	}
	placed, err := parseHosting(declared, base)
	if err != nil {
		return nil, err
	}
	p := Profile{
		ID: declared.ID, Revision: plugin.Digest, Label: declared.Label, Kind: KindPlugin,
		Dialect: declared.Dialect, DialectRevision: base.DialectRevision, Hosting: pluginHosting,
		Authentication: []string{AuthStaticCredential}, Transport: "http", Operations: []string{"generation"},
		// Semantic headers and query settings belong to the dialect, so the
		// provider configures them as it would for the dialect's direct hosting.
		SemanticHeaders: slices.Clone(base.SemanticHeaders), QuerySettings: slices.Clone(base.QuerySettings),
		Plugin: &plugin, ModelDiscovery: declared.Hosting.Discovery != nil,
		// The adaptation changes only authorization, address and declared
		// headers, so the profile serves strict routes.
		Strict: true,
	}
	completeProfileMetadata(&p)
	return &PluginProfile{profile: p, hosting: placed, declared: declared}, nil
}

// dialectProfile returns the built-in profile that hosts a plugin dialect
// directly, whose dialect revision and semantic configuration a plugin
// profile of that dialect shares.
func dialectProfile(dialect string) (Profile, bool) {
	if !slices.Contains(pluginDialects, dialect) {
		return Profile{}, false
	}
	profileMu.RLock()
	defer profileMu.RUnlock()
	for _, p := range profileRegistry {
		if p.Dialect == dialect {
			return p, true
		}
	}
	return Profile{}, false
}

// Profile returns the profile's catalogue metadata.
func (p *PluginProfile) Profile() Profile { return cloneProfile(p.profile) }

// Address is the upstream's base URL, which is a plugin provider's endpoint.
func (p *PluginProfile) Address() string { return p.declared.Hosting.Address }

// pluginProfileJSON is how a PluginProfile travels in published snapshots:
// what the plugin declared, which OLP validates again when it reads it.
type pluginProfileJSON struct {
	Plugin  Plugin      `json:"plugin"`
	Profile abi.Profile `json:"profile"`
}

func (p *PluginProfile) MarshalJSON() ([]byte, error) {
	return json.Marshal(pluginProfileJSON{Plugin: *p.profile.Plugin, Profile: p.declared})
}

func (p *PluginProfile) UnmarshalJSON(data []byte) error {
	var stored pluginProfileJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	decoded, err := newPluginProfile(stored.Plugin, stored.Profile)
	if err != nil {
		return err
	}
	*p = *decoded
	return nil
}

// hosting is a parsed hosting adaptation.
type hosting struct {
	headers        map[string]template
	query          map[string]template
	classification []upstream.Rule
}

// credentialValue names the static credential in templates.
const credentialValue = "credential"

var queryName = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)

func parseHosting(declared abi.Profile, base Profile) (hosting, error) {
	declaredHosting := declared.Hosting
	if err := validateAddress(declaredHosting.Address); err != nil {
		return hosting{}, err
	}
	placed := hosting{headers: map[string]template{}, query: map[string]template{}}
	if len(declaredHosting.Headers) > 16 {
		return hosting{}, &ProfileError{Field: "hosting.headers", Message: "Declare at most 16 headers."}
	}
	for _, name := range slices.Sorted(maps.Keys(declaredHosting.Headers)) {
		field := "hosting.headers." + name
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		switch {
		case !ConfigurableHeader(name):
			return hosting{}, &ProfileError{Field: field, Message: "Use a valid header name other than a hop-by-hop, framing, content negotiation, tracing or X-OLP header."}
		case slices.ContainsFunc(base.SemanticHeaders, func(semantic string) bool { return strings.EqualFold(semantic, name) }):
			return hosting{}, &ProfileError{Field: field, Message: "This header is the dialect's semantic header, which the provider configures."}
		}
		if _, duplicate := placed.headers[canonical]; duplicate {
			return hosting{}, &ProfileError{Field: field, Message: "Declare each header once."}
		}
		value, err := parseValue(field, declaredHosting.Headers[name])
		if err != nil {
			return hosting{}, err
		}
		placed.headers[canonical] = value
	}
	if len(declaredHosting.Query) > 16 {
		return hosting{}, &ProfileError{Field: "hosting.query", Message: "Declare at most 16 query parameters."}
	}
	for _, name := range slices.Sorted(maps.Keys(declaredHosting.Query)) {
		field := "hosting.query." + name
		switch {
		case !queryName.MatchString(name):
			return hosting{}, &ProfileError{Field: field, Message: "Name the query parameter with 1–128 letters, digits, dots, underscores, tildes and hyphens."}
		case slices.Contains(base.QuerySettings, name):
			return hosting{}, &ProfileError{Field: field, Message: "This query parameter is the dialect's semantic setting, which the provider configures."}
		case declared.Dialect == "gemini-generate-content" && name == "alt":
			// Gemini addresses streaming with alt=sse.
			return hosting{}, &ProfileError{Field: field, Message: "The dialect addresses its operations with this query parameter."}
		}
		value, err := parseValue(field, declaredHosting.Query[name])
		if err != nil {
			return hosting{}, err
		}
		placed.query[name] = value
	}
	if !placed.uses(credentialValue) {
		return hosting{}, &ProfileError{Field: "hosting", Message: "Place the static credential with {credential} in a header or query parameter."}
	}
	if err := validateDiscovery(declaredHosting); err != nil {
		return hosting{}, err
	}
	classification, err := parseClassification(declaredHosting.Classification)
	if err != nil {
		return hosting{}, err
	}
	placed.classification = classification
	return placed, nil
}

func validateAddress(address string) error {
	invalid := &ProfileError{Field: "hosting.address", Message: "Declare the address as an http or https URL without placeholders, credentials, query or fragment, such as https://api.example.com/v1."}
	if len(address) > 2048 || strings.ContainsAny(address, "{}") {
		return invalid
	}
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(address, "#") {
		return invalid
	}
	return nil
}

func parseValue(field, text string) (template, error) {
	value, err := parseTemplate(text, func(name string) bool { return name == credentialValue })
	if err != nil {
		return template{}, &ProfileError{Field: field, Message: err.Error()}
	}
	if len(text) > 2048 || strings.ContainsFunc(text, unicode.IsControl) {
		return template{}, &ProfileError{Field: field, Message: "Use a value of at most 2048 characters without control characters."}
	}
	return value, nil
}

func (h hosting) uses(name string) bool {
	for _, value := range h.headers {
		if value.uses(name) {
			return true
		}
	}
	for _, value := range h.query {
		if value.uses(name) {
			return true
		}
	}
	return false
}

// place fills the declared headers and query parameters from the static
// credential. It returns the placed values that carry the credential.
func (p *PluginProfile) place(req *http.Request, credential []byte) ([]string, error) {
	if len(credential) == 0 || strings.ContainsAny(string(credential), "\r\n\x00") {
		return nil, ErrCredentialRejected
	}
	values := map[string]string{credentialValue: string(credential)}
	var sensitive []string
	for name, value := range p.hosting.headers {
		placed := value.render(values)
		if !httpguts.ValidHeaderFieldValue(placed) {
			return nil, ErrCredentialRejected
		}
		req.Header.Set(name, placed)
		if value.uses(credentialValue) {
			sensitive = append(sensitive, placed)
		}
	}
	if len(p.hosting.query) == 0 {
		return sensitive, nil
	}
	query := req.URL.Query()
	for name, value := range p.hosting.query {
		if _, found := query[name]; found {
			return nil, fmt.Errorf("declared query parameter %s collides with operation addressing", name)
		}
		query.Set(name, value.render(values))
		if value.uses(credentialValue) {
			sensitive = append(sensitive, url.QueryEscape(string(credential)))
		}
	}
	req.URL.RawQuery = query.Encode()
	return sensitive, nil
}

// A template is a hosting adaptation value with placeholders, such as
// "Token {credential}". Braces appear only around a placeholder.
type template struct {
	text  string
	names []string
}

var placeholder = regexp.MustCompile(`\{([^{}]*)\}`)

// parseTemplate reads a template whose placeholders name values known admits.
func parseTemplate(text string, known func(name string) bool) (template, error) {
	parsed := template{text: text}
	literal := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		parsed.names = append(parsed.names, match[1:len(match)-1])
		return ""
	})
	if strings.ContainsAny(literal, "{}") {
		return template{}, errors.New("Use braces only around a placeholder, such as {credential}.")
	}
	for _, name := range parsed.names {
		if !known(name) {
			return template{}, fmt.Errorf("OLP has no placeholder {%s}; use {credential}.", name)
		}
	}
	return parsed, nil
}

func (t template) uses(name string) bool { return slices.Contains(t.names, name) }

func (t template) render(values map[string]string) string {
	return placeholder.ReplaceAllStringFunc(t.text, func(match string) string { return values[match[1:len(match)-1]] })
}

// ConfigurableHeader reports whether configuration may set the named request
// header: a valid name other than a hop-by-hop, framing, content negotiation,
// tracing or X-OLP header, which OLP owns.
func ConfigurableHeader(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	h := strings.ToLower(name)
	if strings.HasPrefix(h, "x-olp-") {
		return false
	}
	switch h {
	case "host", "content-length", "transfer-encoding", "connection", "cookie", "proxy-authorization", "proxy-connection", "upgrade", "te", "trailer", "content-type", "content-encoding", "accept", "traceparent", "tracestate", "x-request-id":
		return false
	}
	return true
}
