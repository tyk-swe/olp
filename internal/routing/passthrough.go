package routing

import (
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

const PassthroughOperation = "passthrough"

var PassthroughMethods = []string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"}

var passthroughDialect = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

type PassthroughConfig struct {
	Dialect                    string   `json:"dialect"`
	Methods                    []string `json:"methods"`
	Paths                      []string `json:"paths"`
	Query                      []string `json:"query,omitempty"`
	Headers                    []string `json:"headers,omitempty"`
	AcknowledgeOpaqueUnmetered bool     `json:"acknowledge_opaque_unmetered"`
}

func (r Route) IsPassthrough() bool { return r.Passthrough != nil }

var forbiddenPassthroughHeaders = map[string]bool{
	"Host": true, "Connection": true, "Keep-Alive": true, "Proxy-Connection": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Proxy-Authenticate": true, "Proxy-Authorization": true, "Authorization": true,
	"Cookie": true, "Api-Key": true, "X-Api-Key": true, "X-Goog-Api-Key": true,
	"Content-Length": true,
}

func HopByHopOrCredentialHeader(name string) bool {
	canonical := textproto.CanonicalMIMEHeaderKey(name)
	return forbiddenPassthroughHeaders[canonical] || strings.HasPrefix(canonical, "X-Olp-")
}

func (c *PassthroughConfig) Validate() error {
	if c == nil {
		return invalidValue("passthrough", "Declare the pass-through binding, methods and path patterns.")
	}
	if !c.AcknowledgeOpaqueUnmetered {
		return &Refusal{Code: "passthrough_acknowledgement_required", Message: "Publishing a pass-through route requires acknowledging opaque provider state and unmetered work."}
	}
	if !passthroughDialect.MatchString(c.Dialect) {
		return invalidValue("passthrough.dialect", "Use a lowercase dialect name of letters, digits and hyphens for the pass-through namespace.")
	}
	if len(c.Methods) == 0 || len(c.Methods) > len(PassthroughMethods) || !distinctMembers(c.Methods, PassthroughMethods) {
		return invalidValue("passthrough.methods", "Name distinct pass-through methods among GET, HEAD, POST, PUT, PATCH and DELETE.")
	}
	if len(c.Paths) == 0 || len(c.Paths) > 32 || !distinct(c.Paths) {
		return invalidValue("passthrough.paths", "Declare 1 to 32 distinct pass-through path patterns.")
	}
	for _, pattern := range c.Paths {
		if _, err := parsePassthroughPattern(pattern); err != nil {
			return err
		}
	}
	if len(c.Query) > 32 || !distinct(c.Query) {
		return invalidValue("passthrough.query", "Allow at most 32 distinct pass-through query names.")
	}
	for _, name := range c.Query {
		if !validPassthroughQueryName(name) {
			return invalidValue("passthrough.query", "Pass-through query names use letters, digits, dot, underscore or hyphen.")
		}
	}
	if len(c.Headers) > 32 {
		return invalidValue("passthrough.headers", "Allow at most 32 pass-through headers.")
	}
	seen := map[string]bool{}
	for _, name := range c.Headers {
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		if canonical != name || seen[canonical] || HopByHopOrCredentialHeader(canonical) || !validHeaderName(canonical) {
			return invalidValue("passthrough.headers", "Allow distinct canonical pass-through headers; hop-by-hop and credential headers are never forwarded.")
		}
		seen[canonical] = true
	}
	return nil
}

func validHeaderName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}

func validPassthroughQueryName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func parsePassthroughPattern(pattern string) ([]string, error) {
	if pattern == "" || len(pattern) > 512 || !strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "//") || strings.ContainsAny(pattern, "?#\\%") {
		return nil, invalidValue("passthrough.paths", "Pass-through path patterns are anchored slash paths without query, fragment, backslash or escapes.")
	}
	segments := strings.Split(strings.TrimPrefix(pattern, "/"), "/")
	if len(segments) == 0 || len(segments) > 16 {
		return nil, invalidValue("passthrough.paths", "Pass-through path patterns have 1 to 16 segments.")
	}
	for _, segment := range segments {
		if segment == "" || segment == "." || segment == ".." {
			return nil, invalidValue("passthrough.paths", "Pass-through path patterns have no empty or dot segments.")
		}
		if segment == "*" {
			continue
		}
		if strings.Contains(segment, "*") {
			return nil, invalidValue("passthrough.paths", "A pass-through wildcard is a whole path segment.")
		}
		for _, r := range segment {
			if r < 0x21 || r > 0x7e {
				return nil, invalidValue("passthrough.paths", "Pass-through path patterns use printable ASCII segments.")
			}
		}
	}
	return segments, nil
}

func NormalizePassthroughPath(escaped string) (string, error) {
	if escaped == "" || len(escaped) > 4096 || !strings.HasPrefix(escaped, "/") || strings.HasPrefix(escaped, "//") || strings.Contains(escaped, "://") || strings.ContainsAny(escaped, "?#\\") {
		return "", invalidValue("passthrough.path", "The pass-through path is not an anchored slash path.")
	}
	raw := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(raw) == 0 || len(raw) > 16 {
		return "", invalidValue("passthrough.path", "The pass-through path has too many segments.")
	}
	segments := make([]string, 0, len(raw))
	for _, segment := range raw {
		if segment == "" {
			return "", invalidValue("passthrough.path", "The pass-through path has an empty segment.")
		}
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\#?\x00") {
			return "", invalidValue("passthrough.path", "The pass-through path has an encoded separator or dot segment.")
		}
		if strings.ContainsFunc(decoded, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return "", invalidValue("passthrough.path", "The pass-through path has a control character.")
		}
		if hasReservedEscape(decoded) {
			return "", invalidValue("passthrough.path", "The pass-through path has a doubly encoded separator or dot segment.")
		}
		segments = append(segments, decoded)
	}
	return "/" + strings.Join(segments, "/"), nil
}

func hasReservedEscape(segment string) bool {
	for i := 0; i+2 < len(segment); i++ {
		if segment[i] != '%' {
			continue
		}
		decoded, err := url.PathUnescape(segment[i : i+3])
		if err != nil {
			return true
		}
		if strings.ContainsAny(decoded, "/\\#?.\x00") {
			return true
		}
	}
	return false
}

func (c *PassthroughConfig) AllowsMethod(method string) bool {
	return c != nil && slices.Contains(c.Methods, method)
}

func (c *PassthroughConfig) Match(method, escapedPath string) (bool, error) {
	normalized, err := NormalizePassthroughPath(escapedPath)
	if err != nil {
		return false, err
	}
	if !c.AllowsMethod(method) {
		return false, nil
	}
	segments := strings.Split(strings.TrimPrefix(normalized, "/"), "/")
	for _, pattern := range c.Paths {
		parts, err := parsePassthroughPattern(pattern)
		if err != nil {
			return false, err
		}
		if len(parts) != len(segments) {
			continue
		}
		matched := true
		for i, part := range parts {
			if part != "*" && part != segments[i] {
				matched = false
				break
			}
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

func (r Route) ValidatePassthrough() error {
	if r.Passthrough == nil {
		if slices.Contains(r.Operations, PassthroughOperation) {
			return &Refusal{Code: "passthrough_conflict", Message: "The passthrough operation requires a pass-through route binding."}
		}
		return nil
	}
	conflict := func(message string) error {
		return &Refusal{Code: "passthrough_conflict", Message: message}
	}
	if err := r.Passthrough.Validate(); err != nil {
		return err
	}
	switch {
	case len(r.Operations) != 1 || r.Operations[0] != PassthroughOperation:
		return conflict("A pass-through route serves only the passthrough operation.")
	case len(r.Targets) != 1:
		return conflict("A pass-through route binds exactly one connection.")
	case r.Targets[0].Shadow != nil:
		return conflict("A pass-through route cannot mirror traffic to a shadow target.")
	case r.Fidelity.Mode == FidelityTransformed:
		return conflict("A pass-through route is neither strict nor transformed and cannot declare translation.")
	case r.ContentPolicy != nil && len(r.ContentPolicy.Rules) > 0:
		return conflict("A pass-through route cannot carry guardrails or a content policy.")
	case r.Cache != nil || r.PromptCache != nil:
		return conflict("A pass-through route cannot use response caching.")
	case len(r.Fallbacks) > 0:
		return conflict("A pass-through route cannot fall back to another route.")
	case len(r.Selectors) > 0 || r.Affinity != nil:
		return conflict("A pass-through route cannot steer requests with selectors or affinity.")
	case len(r.Retry) > 0 || r.MaxAttempts != 1:
		return conflict("A pass-through route makes exactly one attempt and cannot retry opaque work.")
	}
	return nil
}
