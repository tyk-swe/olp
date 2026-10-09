// Package workload verifies bounded workload JWTs against operator-declared
// issuers. Unverified claims never select network destinations or permissions.
package workload

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

const MaxTokenBytes = 32768

var ErrToken = errors.New("workload token is invalid")
var ErrKeys = errors.New("workload verification keys are unavailable")
var name = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)

type Mapping struct {
	Name               string            `json:"name"`
	Match              map[string]string `json:"match"`
	ProjectID          string            `json:"project_id"`
	LimitTemplate      string            `json:"limit_template"`
	RouteGroups        []string          `json:"route_groups"`
	Scopes             []string          `json:"scopes"`
	EndUserClaim       *string           `json:"end_user_claim"`
	AllowProviderState bool              `json:"allow_provider_state"`
}

type Config struct {
	Name               string    `json:"name"`
	Issuer             string    `json:"issuer"`
	JWKSURL            string    `json:"jwks_url"`
	Enabled            bool      `json:"enabled"`
	Audiences          []string  `json:"audiences"`
	Algorithms         []string  `json:"algorithms"`
	MaxLifetimeSeconds int64     `json:"max_lifetime_seconds"`
	DisabledKeyIDs     []string  `json:"disabled_key_ids"`
	Mappings           []Mapping `json:"mappings"`
}

func (c Config) Validate() error {
	bad := errors.New("invalid workload issuer configuration")
	if strings.TrimSpace(c.Name) == "" || len(c.Name) > 100 || !utf8.ValidString(c.Name) || c.MaxLifetimeSeconds < 60 || c.MaxLifetimeSeconds > 86400 {
		return bad
	}
	for _, raw := range []string{c.Issuer, c.JWKSURL} {
		u, e := url.Parse(raw)
		if e != nil || len(raw) > 2048 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return bad
		}
	}
	if !unique(c.Audiences, 1, 32, 256) || !unique(c.Algorithms, 1, 3, 16) || c.DisabledKeyIDs == nil || !unique(c.DisabledKeyIDs, 0, 64, 128) || len(c.Mappings) < 1 || len(c.Mappings) > 32 {
		return bad
	}
	for _, alg := range c.Algorithms {
		if !slices.Contains([]string{"RS256", "ES256", "EdDSA"}, alg) {
			return bad
		}
	}
	names := map[string]bool{}
	for _, m := range c.Mappings {
		if !name.MatchString(m.Name) || names[m.Name] || !name.MatchString(m.LimitTemplate) || m.Match == nil || len(m.Match) > 16 {
			return bad
		}
		names[m.Name] = true
		if id, e := uuid.Parse(m.ProjectID); e != nil || id.String() != m.ProjectID {
			return bad
		}
		if !unique(m.RouteGroups, 1, 64, 100) || !unique(m.Scopes, 1, 2, 32) {
			return bad
		}
		for _, group := range m.RouteGroups {
			if !name.MatchString(group) {
				return bad
			}
		}
		for _, scope := range m.Scopes {
			if scope != "inference" && scope != "models_read" {
				return bad
			}
		}
		for pointer, value := range m.Match {
			if !claimPointer(pointer) || len(value) == 0 || len(value) > 1024 || !utf8.ValidString(value) {
				return bad
			}
		}
		if m.EndUserClaim != nil && !claimPointer(*m.EndUserClaim) {
			return bad
		}
	}
	return nil
}
func claimPointer(p string) bool {
	return strings.HasPrefix(p, "/") && len(p) <= 256 && utf8.ValidString(p)
}
func unique(values []string, min, max, width int) bool {
	if len(values) < min || len(values) > max {
		return false
	}
	seen := map[string]bool{}
	for _, s := range values {
		if s == "" || len(s) > width || !utf8.ValidString(s) || seen[s] || strings.ContainsAny(s, "\r\n\x00") {
			return false
		}
		seen[s] = true
	}
	return true
}
