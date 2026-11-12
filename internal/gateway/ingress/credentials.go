package ingress

import (
	"net/http"
	"strings"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/consumerauth"
	"github.com/tyk-swe/olp/internal/gateway/fault"
)

// BearerToken is the bearer token a request's Authorization header carries.
func BearerToken(r *http.Request) string {
	token, _ := consumerauth.Bearer(r.Header.Get("Authorization"))
	return token
}

// PresentedKey returns the gateway API key a request carries. Authorization
// wins wherever it is present; without it, a request may use the location its
// surface's official SDK sends: Anthropic's X-Api-Key, or Gemini's
// X-Goog-Api-Key and then the key query parameter.
func PresentedKey(r *http.Request) (string, *fault.Error) {
	missing := fault.AuthenticationError("invalid_api_key", "Provide an API key as a bearer token in the Authorization header.")
	if header := r.Header.Get("Authorization"); header != "" {
		if token, ok := consumerauth.Bearer(header); ok {
			return token, nil
		}
		return "", missing
	}
	var key string
	switch fault.RequestSurface(r) {
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
func bedrockPresentedKey(r *http.Request) (string, *fault.Error) {
	token := strings.TrimSpace(r.Header.Get("X-OLP-API-Key"))
	if header := r.Header.Get("Authorization"); token == "" && !strings.Contains(header, "AWS4-HMAC-SHA256") {
		token, _ = consumerauth.Bearer(header)
	}
	if token == "" {
		return "", fault.AuthenticationError("missing_authorization", "Provide the X-OLP-API-Key header or a bearer API key.")
	}
	return token, nil
}

// Authenticate resolves the key a request presents and checks the scope the
// endpoint needs.
func (g *Gate) Authenticate(r *http.Request, scope string) (keyauthority.Authority, *fault.Error) {
	token, e := PresentedKey(r)
	if e != nil {
		return keyauthority.Authority{}, e
	}
	return g.AuthenticateRequest(r, token, scope)
}

// BedrockAuthenticate is authenticate for Bedrock's credential locations.
func (g *Gate) BedrockAuthenticate(r *http.Request) (keyauthority.Authority, *fault.Error) {
	token, e := bedrockPresentedKey(r)
	if e != nil {
		return keyauthority.Authority{}, e
	}
	return g.AuthenticateRequest(r, token, "inference")
}

func ConsumerFailure(failure *consumerauth.Failure) *fault.Error {
	if failure == nil {
		return nil
	}
	switch failure.Status {
	case http.StatusUnauthorized:
		return fault.AuthenticationError(failure.Code, failure.Message)
	case http.StatusForbidden:
		return fault.PermissionError(failure.Code, failure.Message)
	default:
		return fault.ServerError(failure.Status, failure.Code, failure.Message)
	}
}

func (g *Gate) AuthenticateRequest(r *http.Request, token, scope string) (keyauthority.Authority, *fault.Error) {
	authority, failure := g.AuthenticateKey(r, token, scope)
	if failure != nil {
		return keyauthority.Authority{}, failure
	}
	if authority.TaskCapability != nil && !taskCapabilityEndpoint(r) {
		return keyauthority.Authority{}, fault.PermissionError("permission_denied", "This task capability cannot use this endpoint.")
	}
	if scope != "inference" {
		return authority, nil
	}
	return g.IdentifyEndUser(r, authority)
}

func taskCapabilityEndpoint(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/chat/completions", "/v1/responses", "/anthropic/v1/messages":
		return true
	}
	model, ok := strings.CutPrefix(r.URL.Path, "/gemini/v1beta/models/")
	if !ok || strings.Contains(model, "/") {
		return false
	}
	for _, suffix := range []string{":generateContent", ":streamGenerateContent"} {
		if name, found := strings.CutSuffix(model, suffix); found && name != "" {
			return true
		}
	}
	return false
}

func (g *Gate) CheckKeyAddress(r *http.Request, authority keyauthority.Authority) *fault.Error {
	return ConsumerFailure(consumerauth.CheckAddress(authority, r, g.trusted))
}

func (g *Gate) AuthenticateKey(r *http.Request, token, scope string) (keyauthority.Authority, *fault.Error) {
	authority, failure := consumerauth.Authenticate(g.auth, r, token, scope, g.trusted, g.now())
	if failure != nil {
		return keyauthority.Authority{}, ConsumerFailure(failure)
	}
	return authority, nil
}
