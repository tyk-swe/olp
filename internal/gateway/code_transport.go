package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/codewire"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/runtime"
)

func (a *codeAttempt) headers(source http.Header) http.Header {
	h := codewire.ForwardHeaders(source, true)
	for name, values := range a.auth.Headers {
		h[http.CanonicalHeaderKey(name)] = append([]string(nil), values...)
	}
	if _, present := h["User-Agent"]; !present {
		h["User-Agent"] = []string{""}
	}
	return h
}

func (s *Server) codeEndpoint(cfg routing.Configuration, path, query string) (string, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", codemode.Refuse(502, "code_connection_invalid")
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/" + path
	u.RawPath = ""
	if _, err := s.egress.ValidateEndpoint(u.String()); err != nil {
		return "", codemode.Refuse(502, "code_egress_refused")
	}
	u.RawQuery = query
	return u.String(), nil
}

func (s *Server) codeClient(ctx context.Context, release *runtime.Release, cfg routing.Configuration, providerID string) (*http.Client, error) {
	var secret []byte
	if network := cfg.Options.Network; network != nil && network.CredentialID != "" {
		var err error
		secret, err = s.Runtime.NetworkSecret(ctx, release, providerID, network.CredentialID)
		if err != nil {
			return nil, codemode.Refuse(503, "code_network_credential_unavailable")
		}
	}
	client, err := s.egress.RawConnectionClient(cfg.Options.Network, secret, upstreamHeaderTimeout)
	if err != nil {
		return nil, codemode.Refuse(502, "code_connection_invalid")
	}
	return client, nil
}
