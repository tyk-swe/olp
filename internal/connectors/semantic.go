package connectors

import (
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/vendors"
)

func (c Config) DeclaresSemanticHeader(name string) bool {
	if c.ProfileID == "" {
		return false
	}
	p, err := c.profile()
	return err == nil && slices.ContainsFunc(p.SemanticHeaders, func(declared string) bool { return strings.EqualFold(declared, name) })
}

func (c Config) SemanticHeader(name string) (string, bool) {
	for published, value := range c.SemanticHeaders {
		if strings.EqualFold(published, name) {
			return value, true
		}
	}
	return "", false
}

func (c Config) WithSemanticHeader(name, value string) Config {
	published := make(map[string]string, len(c.SemanticHeaders)+1)
	for existing, v := range c.SemanticHeaders {
		if !strings.EqualFold(existing, name) {
			published[existing] = v
		}
	}
	published[textproto.CanonicalMIMEHeaderKey(name)] = value
	c.SemanticHeaders = published
	return c
}

func (c Config) host(req *http.Request, credential []byte) ([]string, error) {
	if err := c.ApplySemantic(req); err != nil {
		return nil, err
	}
	if c.Kind == "anthropic" && c.ProfileID == "" {
		req.Header.Set("Anthropic-Version", anthropicMessagesRevision)
	}
	for name, value := range vendors.HeadersFor(c.VendorID) {
		req.Header.Set(name, value)
	}
	if c.Plugin != nil {
		return c.Plugin.place(req, credential, c.PluginOptions)
	}
	return nil, nil
}

func (c Config) ApplySemantic(req *http.Request) error {
	if c.ProfileID == "" {
		return nil
	}
	if err := c.ValidateProfile(); err != nil {
		return err
	}
	p, _ := c.profile()
	if p.Hosting == "direct-anthropic" {
		req.Header.Set("Anthropic-Version", p.DialectRevision)
	}
	for name, value := range c.SemanticHeaders {
		req.Header.Set(name, value)
	}
	query := req.URL.Query()
	for name, value := range c.QuerySettings {
		if existing, found := query[name]; found && (len(existing) != 1 || existing[0] != value) {
			return fmt.Errorf("query setting %s collides with operation addressing", name)
		}
		query.Set(name, value)
	}
	req.URL.RawQuery = query.Encode()
	return nil
}

func (p Profile) BindIngressSemanticHeader(name, value string) (bool, error) {
	if !strings.EqualFold(name, "Anthropic-Version") || (p.Hosting != "vertex-anthropic" && p.Hosting != "bedrock-anthropic-invoke") {
		return false, nil
	}
	if value != anthropicMessagesRevision {
		return true, errors.New("the ingress API revision has no qualified hosting binding")
	}
	return true, nil
}
