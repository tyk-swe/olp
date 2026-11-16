package connectors

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"
)

type GrantCredential struct {
	PluginDigest string            `json:"plugin_digest"`
	ProfileID    string            `json:"profile_id"`
	AccessToken  string            `json:"access_token"`
	Facts        map[string]string `json:"facts,omitempty"`
}

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

func (p *PluginProfile) credential(secret []byte) (token string, facts map[string]string, err error) {
	token = string(secret)
	if p.declared.Grant != nil {
		var grant GrantCredential
		if json.Unmarshal(secret, &grant) != nil || grant.PluginDigest == "" || grant.ProfileID == "" ||
			grant.PluginDigest != p.profile.Plugin.Digest || grant.ProfileID != p.profile.ID {
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

func (p *PluginProfile) PlacesGrant(facts map[string]string) error {
	if p.hosting.base == "" {
		return nil
	}
	_, err := p.baseURL(facts[p.hosting.base])
	return err
}

func (p *PluginProfile) baseURL(value string) (*url.URL, error) {
	base, err := url.Parse(value)
	if err != nil || !plainHTTPURL(base) {
		return nil, fmt.Errorf("grant fact %s holds no base URL", p.hosting.base)
	}
	origin := Origin(base)
	if !slices.Contains(p.origins, origin) {
		return nil, fmt.Errorf("grant fact %s places requests at %s, which is not one of the plugin's approved origins", p.hosting.base, origin)
	}
	base.Host = strings.TrimPrefix(origin, base.Scheme+"://")
	return base, nil
}

func plainHTTPURL(u *url.URL) bool {
	return (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && !u.ForceQuery && u.Fragment == ""
}

func Origin(u *url.URL) string {
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if port := u.Port(); scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		host = strings.TrimSuffix(host, ":"+port)
	}
	return scheme + "://" + host
}

func (p *PluginProfile) rebase(req *http.Request, value string) error {
	base, err := p.baseURL(value)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrCredentialRejected, err)
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
