package gateway

import (
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/runtime"
)

// bearer returns the token of an Authorization header that uses the Bearer
// scheme, which clients spell in any case.
func bearer(header string) (string, bool) {
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") {
		return "", false
	}
	return strings.TrimSpace(header[7:]), true
}

// bearerToken is the bearer token a request's Authorization header carries.
func bearerToken(r *http.Request) string {
	token, _ := bearer(r.Header.Get("Authorization"))
	return token
}

// presentedKey returns the gateway API key a request carries. Authorization
// wins wherever it is present; without it, a request may use the location its
// surface's official SDK sends: Anthropic's X-Api-Key, or Gemini's
// X-Goog-Api-Key and then the key query parameter.
func presentedKey(r *http.Request) (string, *Error) {
	missing := authenticationError("invalid_api_key", "Provide an API key as a bearer token in the Authorization header.")
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := bearer(header); ok {
			return token, nil
		}
		return "", missing
	}
	var key string
	switch requestSurface(r) {
	case "anthropic":
		key = r.Header.Get("X-Api-Key")
	case "gemini":
		if key = r.Header.Get("X-Goog-Api-Key"); key == "" {
			key = r.URL.Query().Get("key")
		}
	}
	if key == "" {
		return "", missing
	}
	return strings.TrimSpace(key), nil
}

// bedrockPresentedKey returns the gateway API key of a Bedrock request. AWS
// SDKs sign Authorization with SigV4, which OLP never verifies, so the key
// travels in X-OLP-API-Key or, from other clients, as a bearer token.
func bedrockPresentedKey(r *http.Request) (string, *Error) {
	token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key"))
	if header := r.Header.Get("Authorization"); token == "" && !strings.Contains(header, "AWS4-HMAC-SHA256") {
		token, _ = bearer(header)
	}
	if token == "" {
		return "", authenticationError("missing_authorization", "Provide the X-OLP-API-Key header or a bearer API key.")
	}
	return token, nil
}

// authenticate resolves the key a request presents and checks the scope the
// endpoint needs.
func (s *Server) authenticate(r *http.Request, scope string) (access.Authority, *Error) {
	token, e := presentedKey(r)
	if e != nil {
		return access.Authority{}, e
	}
	return s.authenticateRequest(r, token, scope)
}

// bedrockAuthenticate is authenticate for Bedrock's credential locations.
func (s *Server) bedrockAuthenticate(r *http.Request) (access.Authority, *Error) {
	token, e := bedrockPresentedKey(r)
	if e != nil {
		return access.Authority{}, e
	}
	return s.authenticateRequest(r, token, "inference")
}

func (s *Server) authenticateRequest(r *http.Request, token, scope string) (access.Authority, *Error) {
	authority, e := s.authorizeKey(token, scope)
	if e != nil {
		return authority, e
	}
	if e = s.checkKeyAddress(r, authority); e != nil || scope != "inference" {
		return authority, e
	}
	return s.identifyEndUser(r, authority)
}

func (s *Server) checkKeyAddress(r *http.Request, authority access.Authority) *Error {
	if len(authority.Policy.AllowedCIDRs) != 0 && !authority.AllowsClientIP(ClientIP(r, s.cfg.TrustedProxies)) {
		return permissionError("ip_not_allowed", "This API key does not allow the client address.")
	}
	return nil
}

// authorizeKey resolves a presented key against the pinned key authority,
// which fails closed while it is stale, and checks that the key is live and
// carries scope.
func (s *Server) authorizeKey(token, scope string) (access.Authority, *Error) {
	authority, err := s.Runtime.Authenticate(token)
	switch {
	case errors.Is(err, runtime.ErrStaleAuthority):
		return access.Authority{}, serverError(http.StatusServiceUnavailable, "authority_unavailable", "Key authority is unavailable; retry shortly.")
	case err != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "Incorrect API key provided.")
	case authority.RevokedAt != nil:
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has been revoked.")
	case authority.ExpiresAt != nil && !authority.ExpiresAt.After(s.now()):
		return access.Authority{}, authenticationError("invalid_api_key", "This API key has expired.")
	case !slices.Contains(authority.Policy.Scopes, scope):
		return access.Authority{}, permissionError("permission_denied", "This API key does not have the "+scope+" scope.")
	}
	return *authority, nil
}

// dropIngressQuery removes the query parameters a client surface sends that
// are not native semantics.
func (x *execution) dropIngressQuery() *Error {
	switch x.clientSurface() {
	case "gemini":
		return x.dropQueryKey()
	case "anthropic":
		// The Anthropic SDKs add beta=true when they call the beta namespace.
		// It selects no API behavior: betas are the Anthropic-Beta header.
		if values := x.semanticQuery["beta"]; len(values) == 1 && values[0] == "true" {
			delete(x.semanticQuery, "beta")
		}
	}
	return nil
}

// dropQueryKey removes the Gemini key query parameter from a request's
// native semantics: authentication already consumed it, and it is neither
// native semantics nor anything an upstream may receive.
func (x *execution) dropQueryKey() *Error {
	keys, present := x.semanticQuery["key"]
	if !present {
		return nil
	}
	if len(keys) != 1 || keys[0] == "" {
		param := "key"
		return invalidRequest("invalid_request", "Provide one non-empty API key query parameter.", &param)
	}
	delete(x.semanticQuery, "key")
	return nil
}
