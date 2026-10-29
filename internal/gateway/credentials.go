package gateway

import (
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/consumerauth"
)

// bearer returns the token of an Authorization header that uses the Bearer
// scheme, which clients spell in any case.
func bearer(header string) (string, bool) {
	return consumerauth.Bearer(header)
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

// CatalogAuthority shares the gateway's pinned authority, revocation, scope
// and client-address checks with the management catalog's consumer-key reads.
func (s *Server) CatalogAuthority(r *http.Request) (access.Authority, error) {
	authority, failure := s.authenticate(r, "models_read")
	if failure != nil {
		return access.Authority{}, access.Fail(failure.Status, failure.Code, failure.Message)
	}
	return authority, nil
}

// bedrockAuthenticate is authenticate for Bedrock's credential locations.
func (s *Server) bedrockAuthenticate(r *http.Request) (access.Authority, *Error) {
	token, e := bedrockPresentedKey(r)
	if e != nil {
		return access.Authority{}, e
	}
	return s.authenticateRequest(r, token, "inference")
}

func consumerFailure(failure *consumerauth.Failure) *Error {
	if failure == nil {
		return nil
	}
	switch failure.Status {
	case http.StatusUnauthorized:
		return authenticationError(failure.Code, failure.Message)
	case http.StatusForbidden:
		return permissionError(failure.Code, failure.Message)
	default:
		return serverError(failure.Status, failure.Code, failure.Message)
	}
}

func (s *Server) authenticateRequest(r *http.Request, token, scope string) (access.Authority, *Error) {
	authority, failure := consumerauth.Authenticate(s.Runtime, r, token, scope, s.cfg.TrustedProxies, s.now())
	if failure != nil {
		return access.Authority{}, consumerFailure(failure)
	}
	if authority.TaskCapability != nil && (r.Method != http.MethodPost || (r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/v1/responses")) {
		return access.Authority{}, permissionError("permission_denied", "This task capability cannot use this endpoint.")
	}
	if scope != "inference" {
		return authority, nil
	}
	return s.identifyEndUser(r, authority)
}

func (s *Server) checkKeyAddress(r *http.Request, authority access.Authority) *Error {
	return consumerFailure(consumerauth.CheckAddress(authority, r, s.cfg.TrustedProxies))
}

// dropIngressQuery removes the query parameters a client surface sends that
// are not native semantics.
func (x *call) dropIngressQuery() *Error {
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
func (x *call) dropQueryKey() *Error {
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
