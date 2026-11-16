package connectors

import (
	"errors"
	"fmt"
	"maps"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/tyk-swe/olp/internal/upstream"
	"github.com/tyk-swe/olp/plugin/abi"
)

func (p *PluginProfile) Address(options map[string]string) string {
	escaped := map[string]string{}
	for name, value := range options {
		escaped[optionPlaceholder+name] = url.PathEscape(value)
	}
	return p.hosting.address.render(escaped)
}

type hosting struct {
	address        template
	headers        map[string]template
	query          map[string]template
	envelope       *envelope
	rewrites       []rewrite
	classification []upstream.Rule
	forceStreaming bool
	base           string
}

const (
	credentialValue   = "credential"
	optionPlaceholder = "options."
	grantPlaceholder  = "grant."
)

var (
	queryName = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,128}$`)
	valueName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

func parseGrant(grant *abi.GrantAuthentication) error {
	if grant == nil {
		return nil
	}
	if len(grant.Facts) > 16 {
		return &ProfileError{Field: "grant.facts", Message: "Declare at most 16 grant facts."}
	}
	if grant.Input != "" && grant.Input != abi.GrantInputSecret {
		return &ProfileError{Field: "grant.input", Message: `Declare the grant input as "secret", or leave it out for a callback URL or code.`}
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

const grantBase = "https://grant.invalid"

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
	if len(text) > 2048 || err != nil || !plainHTTPURL(u) || strings.Contains(fixed, "#") {
		return template{}, "", &ProfileError{Field: "hosting.address", Message: "Declare the address as an http or https URL without credentials, query or fragment, such as https://api.example.com/v1. Options may appear in its path, such as {options.account}."}
	}
	return address, base, nil
}

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
