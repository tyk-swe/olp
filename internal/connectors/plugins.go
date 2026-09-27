package connectors

import (
	"bytes"
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
	"unicode/utf8"

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
	// patterns are the compiled patterns of the options that declare one.
	patterns map[string]*regexp.Regexp
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
	patterns, err := parseOptions(declared.Options)
	if err != nil {
		return nil, err
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
		Plugin: &plugin, ModelDiscovery: declared.Hosting.Discovery != nil, OptionsSchema: optionsSchema(declared.Options),
		// An adaptation that changes only authorization, address and declared
		// headers serves strict routes. An envelope or rewrite changes the
		// dialect's bodies, so the profile serves transformed routes only.
		Strict: placed.envelope == nil && len(placed.rewrites) == 0,
	}
	completeProfileMetadata(&p)
	return &PluginProfile{profile: p, hosting: placed, declared: declared, patterns: patterns}, nil
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

// Address is the upstream's base URL with a provider's options in its path,
// which is the provider's endpoint. Each value fills one path segment or part
// of one.
func (p *PluginProfile) Address(options map[string]string) string {
	escaped := map[string]string{}
	for name, value := range options {
		escaped[optionPlaceholder+name] = url.PathEscape(value)
	}
	return p.hosting.address.render(escaped)
}

// An OptionError locates what is wrong with a provider's value for an option
// its plugin profile declares.
type OptionError struct {
	// Option names the option.
	Option  string
	Message string
}

func (e *OptionError) Error() string { return "option " + e.Option + ": " + e.Message }

// ValidateOptions reports what is wrong with a provider's option values, as an
// *OptionError naming the option: every value is for an option the profile
// declares, and every option that is not optional has one.
func (p *PluginProfile) ValidateOptions(values map[string]string) error {
	for _, name := range slices.Sorted(maps.Keys(values)) {
		if !slices.ContainsFunc(p.declared.Options, func(option abi.Option) bool { return option.Name == name }) {
			return &OptionError{Option: name, Message: "The profile declares no option with this name."}
		}
	}
	for _, option := range p.declared.Options {
		value, set := values[option.Name]
		switch {
		case !set && !option.Optional:
			return &OptionError{Option: option.Name, Message: "Set " + option.Label + "; the profile requires it."}
		case !set:
		case !validOptionValue(value):
			return &OptionError{Option: option.Name, Message: "Use 1–256 characters without control characters."}
		case len(option.Enum) > 0 && !slices.Contains(option.Enum, value):
			return &OptionError{Option: option.Name, Message: "Choose one of " + strings.Join(option.Enum, ", ") + "."}
		case option.Pattern != "" && !p.patterns[option.Name].MatchString(value):
			return &OptionError{Option: option.Name, Message: "Use a value matching " + option.Pattern + "."}
		case (value == "." || value == "..") && p.hosting.address.uses(optionPlaceholder+option.Name):
			return &OptionError{Option: option.Name, Message: "Use a value other than . or .., which would move the address."}
		}
	}
	return nil
}

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
	address        template
	headers        map[string]template
	query          map[string]template
	envelope       *envelope
	rewrites       []rewrite
	classification []upstream.Rule
}

// Templates name a provider's values: credentialValue is its static
// credential, and optionPlaceholder prefixes the name of one of its options.
const (
	credentialValue   = "credential"
	optionPlaceholder = "options."
)

var (
	queryName  = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	optionName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// parseOptions checks the options a profile declares and compiles their
// patterns.
func parseOptions(declared []abi.Option) (map[string]*regexp.Regexp, error) {
	if len(declared) > 16 {
		return nil, &ProfileError{Field: "options", Message: "Declare at most 16 options."}
	}
	patterns := map[string]*regexp.Regexp{}
	for i, option := range declared {
		field := fmt.Sprintf("options[%d]", i)
		switch {
		case !optionName.MatchString(option.Name):
			return nil, &ProfileError{Field: field + ".name", Message: "Name the option with 1–64 lowercase letters, digits and underscores, starting with a letter."}
		case slices.ContainsFunc(declared[:i], func(prior abi.Option) bool { return prior.Name == option.Name }):
			return nil, &ProfileError{Field: field + ".name", Message: "Declare each option once."}
		case !plainText(option.Label, 1, 100):
			return nil, &ProfileError{Field: field + ".label", Message: "Label the option with 1–100 characters, without control characters."}
		case !plainText(option.Description, 0, 500):
			return nil, &ProfileError{Field: field + ".description", Message: "Describe the option in at most 500 characters, without control characters."}
		case len(option.Enum) > 0 && option.Pattern != "":
			return nil, &ProfileError{Field: field + ".pattern", Message: "Declare an enum or a pattern, not both: the enum already fixes the values."}
		case len(option.Enum) > 64:
			return nil, &ProfileError{Field: field + ".enum", Message: "List at most 64 values."}
		}
		for j, value := range option.Enum {
			if !validOptionValue(value) || slices.Contains(option.Enum[:j], value) {
				return nil, &ProfileError{Field: fmt.Sprintf("%s.enum[%d]", field, j), Message: "List distinct values of 1–256 characters without control characters."}
			}
		}
		if option.Pattern == "" {
			continue
		}
		pattern, err := regexp.Compile(option.Pattern)
		if err != nil || len(option.Pattern) > 512 {
			return nil, &ProfileError{Field: field + ".pattern", Message: "Use a regular expression of at most 512 characters in RE2 syntax."}
		}
		patterns[option.Name] = pattern
	}
	return patterns, nil
}

// validOptionValue reports whether value is one an option may take.
func validOptionValue(value string) bool { return plainText(value, 1, 256) }

func plainText(text string, least, most int) bool {
	n := utf8.RuneCountInString(text)
	return utf8.ValidString(text) && n >= least && n <= most && !strings.ContainsFunc(text, unicode.IsControl)
}

// optionsSchema describes a profile's options as the JSON Schema of a
// provider's option values, with the options in declared order.
func optionsSchema(declared []abi.Option) json.RawMessage {
	var properties bytes.Buffer
	required := []string{}
	properties.WriteByte('{')
	for i, option := range declared {
		schema := map[string]any{"type": "string", "title": option.Label, "minLength": 1, "maxLength": 256}
		if option.Description != "" {
			schema["description"] = option.Description
		}
		if len(option.Enum) > 0 {
			schema["enum"] = option.Enum
		}
		if option.Pattern != "" {
			schema["pattern"] = option.Pattern
		}
		if !option.Optional {
			required = append(required, option.Name)
		}
		name, _ := json.Marshal(option.Name)
		value, _ := json.Marshal(schema)
		if i > 0 {
			properties.WriteByte(',')
		}
		properties.Write(name)
		properties.WriteByte(':')
		properties.Write(value)
	}
	properties.WriteByte('}')
	schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": json.RawMessage(properties.Bytes())})
	return schema
}

// placeholders returns what a profile's templates may reference: the static
// credential, and the options the profile requires, so that every placed
// value is set.
func placeholders(options []abi.Option) func(name string) error {
	return func(name string) error {
		if name == credentialValue {
			return nil
		}
		option, ok := strings.CutPrefix(name, optionPlaceholder)
		if !ok {
			return fmt.Errorf("OLP has no placeholder {%s}; use {credential} or {options.<name>}.", name)
		}
		i := slices.IndexFunc(options, func(declared abi.Option) bool { return declared.Name == option })
		switch {
		case i < 0:
			return fmt.Errorf("The profile declares no option %s.", option)
		case options[i].Optional:
			return fmt.Errorf("Option %s is optional, so templates can't reference it.", option)
		}
		return nil
	}
}

func parseHosting(declared abi.Profile, base Profile) (hosting, error) {
	declaredHosting := declared.Hosting
	known := placeholders(declared.Options)
	address, err := parseAddress(declaredHosting.Address, known)
	if err != nil {
		return hosting{}, err
	}
	placed := hosting{address: address, headers: map[string]template{}, query: map[string]template{}}
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
		value, err := parseValue(field, declaredHosting.Headers[name], known)
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
		value, err := parseValue(field, declaredHosting.Query[name], known)
		if err != nil {
			return hosting{}, err
		}
		placed.query[name] = value
	}
	if !placed.uses(credentialValue) && !declared.Signing {
		return hosting{}, &ProfileError{Field: "hosting", Message: "Place the static credential with {credential} in a header or query parameter, or sign requests with it."}
	}
	if err := validateDiscovery(declaredHosting); err != nil {
		return hosting{}, err
	}
	if placed.classification, err = parseClassification(declaredHosting.Classification); err != nil {
		return hosting{}, err
	}
	if placed.envelope, err = parseEnvelope(declaredHosting.Envelope); err != nil {
		return hosting{}, err
	}
	if placed.rewrites, err = parseRewrites(declaredHosting.Rewrites, declared.Dialect); err != nil {
		return hosting{}, err
	}
	return placed, nil
}

// parseAddress reads the address template. Options may fill its path, so its
// origin is fixed at install; the credential never appears in it.
func parseAddress(text string, known func(name string) error) (template, error) {
	address, err := parseTemplate(text, func(name string) error {
		if name == credentialValue {
			return errors.New("The address can't carry the credential; place it with {credential} in a header or query parameter.")
		}
		return known(name)
	})
	if err != nil {
		return template{}, &ProfileError{Field: "hosting.address", Message: err.Error()}
	}
	// Go's URL parser refuses braces everywhere but the path.
	u, err := url.Parse(text)
	if len(text) > 2048 || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(text, "#") {
		return template{}, &ProfileError{Field: "hosting.address", Message: "Declare the address as an http or https URL without credentials, query or fragment, such as https://api.example.com/v1. Options may appear in its path, such as {options.account}."}
	}
	return address, nil
}

// parseValue reads a template whose placeholders each name a value that known
// admits.
func parseValue(field, text string, known func(name string) error) (template, error) {
	value, err := parseTemplate(text, known)
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

// templateValues are what a provider's hosting templates are filled from: its
// static credential and its option values, by placeholder name.
func templateValues(credential []byte, options map[string]string) map[string]string {
	values := map[string]string{credentialValue: string(credential)}
	for name, value := range options {
		values[optionPlaceholder+name] = value
	}
	return values
}

// place fills the declared headers and query parameters from the static
// credential and the provider's options. It returns the placed values that
// carry the credential.
func (p *PluginProfile) place(req *http.Request, credential []byte, options map[string]string) ([]string, error) {
	if len(credential) == 0 || strings.ContainsAny(string(credential), "\r\n\x00") {
		return nil, ErrCredentialRejected
	}
	values := templateValues(credential, options)
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

// parseTemplate reads a template whose placeholders each name a value that
// known admits, or says why it does not.
func parseTemplate(text string, known func(name string) error) (template, error) {
	parsed := template{text: text}
	literal := placeholder.ReplaceAllStringFunc(text, func(match string) string {
		parsed.names = append(parsed.names, match[1:len(match)-1])
		return ""
	})
	if strings.ContainsAny(literal, "{}") {
		return template{}, errors.New("Use braces only around a placeholder.")
	}
	for _, name := range parsed.names {
		if err := known(name); err != nil {
			return template{}, err
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
