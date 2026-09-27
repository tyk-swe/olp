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

// AuthGrant authenticates a plugin provider with a grant: rotating upstream
// authorization that the plugin's grant enrollment obtains and OLP holds
// beneath a credential version (ADR 0006). The hosting adaptation places the
// grant's current access token and grant facts.
const AuthGrant = "grant"

// A GrantCredential is the secret of a credential version that has a grant:
// what the credential source serves for it. It holds the grant's current
// access token and its grant facts, never refresh material.
type GrantCredential struct {
	AccessToken string            `json:"access_token"`
	Facts       map[string]string `json:"facts,omitempty"`
}

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
	// Digest is the SHA-256 of the plugin's module, or of an unconfined
	// plugin's executable, which a provider pins as its profile revision.
	Digest  string `json:"digest"`
	Name    string `json:"name"`
	Version string `json:"version"`
	// Unconfined is set for an unconfined plugin, which runs with native
	// privileges where the deployment enables the unconfined tier.
	Unconfined bool `json:"unconfined,omitempty"`
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
	// origins are the plugin's approved origins, where a grant's base URL
	// must lie.
	origins []string
	// patterns are the compiled patterns of the options that declare one.
	patterns map[string]*regexp.Regexp
}

// NewPluginProfile returns the profile id that a confined plugin's manifest
// declares, identified by the digest of the plugin's module.
func NewPluginProfile(digest string, manifest abi.Manifest, id string) (*PluginProfile, error) {
	return declaredProfile(Plugin{Digest: digest}, manifest, id)
}

// NewUnconfinedPluginProfile returns the profile id that an unconfined
// plugin's manifest declares, identified by the digest of its executable.
func NewUnconfinedPluginProfile(digest string, manifest abi.Manifest, id string) (*PluginProfile, error) {
	return declaredProfile(Plugin{Digest: digest, Unconfined: true}, manifest, id)
}

func declaredProfile(plugin Plugin, manifest abi.Manifest, id string) (*PluginProfile, error) {
	plugin.Name, plugin.Version = manifest.Name, manifest.Version
	for _, declared := range manifest.Profiles {
		if declared.ID == id {
			return newPluginProfile(plugin, manifest.Origins, declared)
		}
	}
	return nil, fmt.Errorf("plugin %s declares no profile %q", manifest.Name, id)
}

// An InstalledPlugin is what OLP holds of an installed plugin that its
// profiles are read from: the manifest it declared and its tier.
type InstalledPlugin struct {
	Manifest   abi.Manifest `json:"manifest"`
	Unconfined bool         `json:"unconfined"`
}

// DecodePluginProfile returns the profile id of the installed plugin with
// digest, from its InstalledPlugin JSON; installed is nil when no plugin
// with the digest is installed.
func DecodePluginProfile(digest string, installed []byte, id string) (*PluginProfile, error) {
	if installed == nil {
		return nil, fmt.Errorf("plugin %s is not installed", digest)
	}
	var plugin InstalledPlugin
	if err := json.Unmarshal(installed, &plugin); err != nil {
		return nil, err
	}
	return declaredProfile(Plugin{Digest: digest, Unconfined: plugin.Unconfined}, plugin.Manifest, id)
}

// Unconfined reports whether an unconfined plugin supplies the profile.
func (p *PluginProfile) Unconfined() bool { return p.profile.Plugin.Unconfined }

// ValidatePluginProfile reports what is wrong with a profile a plugin
// declares, as a *ProfileError locating the offending value.
func ValidatePluginProfile(declared abi.Profile) error {
	_, err := newPluginProfile(Plugin{}, nil, declared)
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

func newPluginProfile(plugin Plugin, origins []string, declared abi.Profile) (*PluginProfile, error) {
	base, ok := dialectProfile(declared.Dialect)
	if !ok {
		return nil, &ProfileError{Field: "dialect", Message: "Serve one of the dialects plugin profiles can serve: " + strings.Join(pluginDialects, ", ") + "."}
	}
	patterns, err := parseOptions(declared.Options)
	if err != nil {
		return nil, err
	}
	if err = parseGrant(declared.Grant); err != nil {
		return nil, err
	}
	placed, err := parseHosting(declared, base)
	if err != nil {
		return nil, err
	}
	authentication := AuthStaticCredential
	if declared.Grant != nil {
		authentication = AuthGrant
	}
	transport := "http"
	if declared.CarriesTraffic {
		transport = pluginTransport
	}
	p := Profile{
		ID: declared.ID, Revision: plugin.Digest, Label: declared.Label, Kind: KindPlugin,
		Dialect: declared.Dialect, DialectRevision: base.DialectRevision, Hosting: pluginHosting,
		Authentication: []string{authentication}, Transport: transport, Operations: []string{"generation"},
		// Semantic headers and query settings belong to the dialect, so the
		// provider configures them as it would for the dialect's direct hosting.
		SemanticHeaders: slices.Clone(base.SemanticHeaders), QuerySettings: slices.Clone(base.QuerySettings),
		Plugin: &plugin, ModelDiscovery: declared.Hosting.Discovery != nil, OptionsSchema: optionsSchema(declared.Options),
		// An adaptation that changes only authorization, address and declared
		// headers serves strict routes. An envelope or rewrite changes the
		// dialect's bodies, forced streaming how non-streaming requests reach
		// the upstream, and a plugin that carries the traffic sees and may
		// change all of it, so the profile serves transformed routes only.
		Strict: placed.envelope == nil && len(placed.rewrites) == 0 && !placed.forceStreaming && !declared.CarriesTraffic,
	}
	completeProfileMetadata(&p)
	return &PluginProfile{profile: p, hosting: placed, declared: declared, origins: slices.Clone(origins), patterns: patterns}, nil
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
// of one. An address that begins with a grant fact has grantBase as its
// origin, which placement replaces with each grant's base URL.
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
	Origins []string    `json:"origins"`
	Profile abi.Profile `json:"profile"`
}

func (p *PluginProfile) MarshalJSON() ([]byte, error) {
	return json.Marshal(pluginProfileJSON{Plugin: *p.profile.Plugin, Origins: p.origins, Profile: p.declared})
}

func (p *PluginProfile) UnmarshalJSON(data []byte) error {
	var stored pluginProfileJSON
	if err := json.Unmarshal(data, &stored); err != nil {
		return err
	}
	decoded, err := newPluginProfile(stored.Plugin, stored.Origins, stored.Profile)
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
	forceStreaming bool
	// base names the grant fact that holds the upstream's base URL, when the
	// address begins with one.
	base string
}

// Templates name a provider's values: credentialValue is its static
// credential or its grant's current access token, optionPlaceholder prefixes
// the name of one of its options, and grantPlaceholder the name of one of its
// grant's facts.
const (
	credentialValue   = "credential"
	optionPlaceholder = "options."
	grantPlaceholder  = "grant."
)

var (
	queryName = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	// valueName is how options and grant facts are named.
	valueName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
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
		case !valueName.MatchString(option.Name):
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

// parseGrant checks the grant facts a profile that authenticates with a grant
// declares.
func parseGrant(grant *abi.GrantAuthentication) error {
	if grant == nil {
		return nil
	}
	if len(grant.Facts) > 16 {
		return &ProfileError{Field: "grant.facts", Message: "Declare at most 16 grant facts."}
	}
	for i, fact := range grant.Facts {
		field := fmt.Sprintf("grant.facts[%d]", i)
		if !valueName.MatchString(fact) {
			return &ProfileError{Field: field, Message: "Name the grant fact with 1–64 lowercase letters, digits and underscores, starting with a letter."}
		}
		if slices.Contains(grant.Facts[:i], fact) {
			return &ProfileError{Field: field, Message: "Declare each grant fact once."}
		}
	}
	return nil
}

// placeholders returns what a profile's templates may reference: the static
// credential or a grant's access token, the options the profile requires and
// the facts of its grant, so that every placed value is set.
func placeholders(options []abi.Option, grant *abi.GrantAuthentication) func(name string) error {
	return func(name string) error {
		if name == credentialValue {
			return nil
		}
		if fact, ok := strings.CutPrefix(name, grantPlaceholder); ok {
			switch {
			case grant == nil:
				return errors.New("The profile authenticates without a grant, so it has no grant facts.")
			case !slices.Contains(grant.Facts, fact):
				return fmt.Errorf("The profile declares no grant fact %s.", fact)
			}
			return nil
		}
		option, ok := strings.CutPrefix(name, optionPlaceholder)
		if !ok {
			return fmt.Errorf("OLP has no placeholder {%s}; use {credential}, {options.<name>} or {grant.<name>}.", name)
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
	known := placeholders(declared.Options, declared.Grant)
	address, baseFact, err := parseAddress(declaredHosting.Address, known)
	if err != nil {
		return hosting{}, err
	}
	placed := hosting{address: address, base: baseFact, headers: map[string]template{}, query: map[string]template{}}
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
		if declared.Grant != nil {
			return hosting{}, &ProfileError{Field: "hosting", Message: "Place the grant's access token with {credential} in a header or query parameter, or sign requests with it."}
		}
		return hosting{}, &ProfileError{Field: "hosting", Message: "Place the static credential with {credential} in a header or query parameter, or sign requests with it."}
	}
	if err := validateDiscovery(declaredHosting); err != nil {
		return hosting{}, err
	}
	if placed.classification, err = parseClassification(declaredHosting.Classification); err != nil {
		return hosting{}, err
	}
	if placed.envelope, err = parseEnvelope(declaredHosting.Envelope, known); err != nil {
		return hosting{}, err
	}
	if placed.rewrites, err = parseRewrites(declaredHosting.Rewrites, declared.Dialect); err != nil {
		return hosting{}, err
	}
	if placed.forceStreaming, err = parseForceStreaming(declared); err != nil {
		return hosting{}, err
	}
	return placed, nil
}

// grantBase is the origin of the endpoint of a provider whose address begins
// with a grant fact, which stands for each grant's base URL. Its name is
// reserved never to resolve (RFC 6761), so only placement, which moves each
// request to its grant's base URL, sends a request there.
const grantBase = "https://grant.invalid"

// parseAddress reads the address template. Options may fill its path, so its
// origin is fixed at install. A profile that authenticates with a grant may
// instead begin it with the grant fact that holds the upstream's base URL,
// named by base, which differs per credential version: placement checks its
// origin on every request. The credential never appears in the address, nor
// grant facts elsewhere in it.
func parseAddress(text string, known func(name string) error) (address template, base string, err error) {
	fixed := text
	if at := placeholder.FindStringSubmatchIndex(text); at != nil && at[0] == 0 && strings.HasPrefix(text[at[2]:at[3]], grantPlaceholder) {
		name, path := text[at[2]:at[3]], text[at[1]:]
		if err := known(name); err != nil {
			return template{}, "", &ProfileError{Field: "hosting.address", Message: err.Error()}
		}
		if path != "" && !strings.HasPrefix(path, "/") {
			return template{}, "", &ProfileError{Field: "hosting.address", Message: "Follow the grant fact that begins the address with its path, such as {grant.api_base}/v1."}
		}
		base, fixed = strings.TrimPrefix(name, grantPlaceholder), grantBase+path
	}
	address, err = parseTemplate(fixed, func(name string) error {
		switch {
		case name == credentialValue:
			return errors.New("The address can't carry the credential; place it with {credential} in a header or query parameter.")
		case strings.HasPrefix(name, grantPlaceholder):
			return errors.New("A grant fact may only begin the address, as the upstream's base URL, such as {grant.api_base}/v1; place other facts in a header or query parameter.")
		}
		return known(name)
	})
	if err != nil {
		return template{}, "", &ProfileError{Field: "hosting.address", Message: err.Error()}
	}
	// Go's URL parser refuses braces everywhere but the path.
	u, err := url.Parse(fixed)
	if len(text) > 2048 || err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(fixed, "#") {
		return template{}, "", &ProfileError{Field: "hosting.address", Message: "Declare the address as an http or https URL without credentials, query or fragment, such as https://api.example.com/v1. Options may appear in its path, such as {options.account}."}
	}
	return address, base, nil
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
// static credential or its grant's access token, its option values and its
// grant's facts, by placeholder name.
func templateValues(credential string, options, facts map[string]string) map[string]string {
	values := map[string]string{credentialValue: credential}
	for name, value := range options {
		values[optionPlaceholder+name] = value
	}
	for name, value := range facts {
		values[grantPlaceholder+name] = value
	}
	return values
}

// credential reads what a provider's credential secret holds for the
// profile: the static credential, or the access token and facts of a grant's
// GrantCredential. A secret whose token can't be sent in a header, or whose
// grant lacks a fact the profile declares, rejects the request.
func (p *PluginProfile) credential(secret []byte) (token string, facts map[string]string, err error) {
	token = string(secret)
	if p.declared.Grant != nil {
		var grant GrantCredential
		if json.Unmarshal(secret, &grant) != nil {
			return "", nil, ErrCredentialRejected
		}
		for _, fact := range p.declared.Grant.Facts {
			if _, recorded := grant.Facts[fact]; !recorded {
				return "", nil, ErrCredentialRejected
			}
		}
		token, facts = grant.AccessToken, grant.Facts
	}
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return "", nil, ErrCredentialRejected
	}
	return token, facts, nil
}

// place fills the declared headers and query parameters from the static
// credential, or a grant's access token and grant facts, and the provider's
// options, and moves a request to its grant's base URL when the address
// begins with one. It returns the placed values that carry the credential,
// and a grant's access token.
func (p *PluginProfile) place(req *http.Request, secret []byte, options map[string]string) ([]string, error) {
	token, facts, err := p.credential(secret)
	if err != nil {
		return nil, err
	}
	if p.hosting.base != "" {
		if err := p.rebase(req, facts[p.hosting.base]); err != nil {
			return nil, err
		}
	}
	values := templateValues(token, options, facts)
	var sensitive []string
	if p.declared.Grant != nil {
		// Apply redacts the secret as a whole; a grant's access token is
		// only part of it.
		sensitive = append(sensitive, token)
	}
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
			sensitive = append(sensitive, url.QueryEscape(token))
		}
	}
	req.URL.RawQuery = query.Encode()
	return sensitive, nil
}

// rebase moves a request addressed from the provider's endpoint, at
// grantBase, to the base URL a grant's fact holds: an http or https URL
// without credentials, query or fragment, at one of the plugin's approved
// origins, written the same way. A grant whose base URL is elsewhere can't
// serve, so nothing is sent.
func (p *PluginProfile) rebase(req *http.Request, value string) error {
	base, err := url.Parse(value)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" || base.User != nil || base.RawQuery != "" || base.ForceQuery || base.Fragment != "" {
		return fmt.Errorf("%w: grant fact %s holds no base URL", ErrCredentialRejected, p.hosting.base)
	}
	if origin := base.Scheme + "://" + base.Host; !slices.Contains(p.origins, origin) {
		return fmt.Errorf("%w: grant fact %s places requests at %s, which is not one of the plugin's approved origins", ErrCredentialRejected, p.hosting.base, origin)
	}
	if req.URL.Scheme+"://"+req.URL.Host != grantBase {
		return errors.New("the request is not addressed from the provider's endpoint")
	}
	rebased := *req.URL
	rebased.Scheme, rebased.Host = base.Scheme, base.Host
	rebased.Path = strings.TrimSuffix(base.Path, "/") + req.URL.Path
	rebased.RawPath = strings.TrimSuffix(base.EscapedPath(), "/") + req.URL.EscapedPath()
	req.URL, req.Host = &rebased, base.Host
	return nil
}

// A template is a hosting adaptation value with placeholders, such as
// "Token {credential}" or "{grant.account}". Braces appear only around a
// placeholder.
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
