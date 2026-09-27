package connectors

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// A Signer runs the signing hooks of plugin profiles: plugin code, which it
// finds by the digest of the plugin's module.
type Signer interface {
	// Sign runs the hook of the plugin with digest over request for provider,
	// which the plugin receives as its call's provider, and returns the
	// headers to add. The plugin's output never reveals secrets.
	Sign(ctx context.Context, digest string, provider abi.Provider, request abi.SignRequest, secrets []string) (abi.SignResult, error)
}

// maxSignedHeaders bounds the headers a signing hook adds to a request.
const maxSignedHeaders = 16

// sign runs the profile's signing hook, if it declares one, over a finished
// request of a provider with options and adds the headers it returns. It
// returns their values, which it treats like the credential: they are
// redacted wherever upstream text is recorded, and the hook's own output never
// reveals secrets. The request is not sent unless its hook succeeds.
func (p *PluginProfile) sign(ctx context.Context, signer Signer, options map[string]string, req *http.Request, credential, body []byte, secrets []string) ([]string, error) {
	if !p.declared.Signing {
		return nil, nil
	}
	if signer == nil {
		return nil, fmt.Errorf("%w: this process runs no plugin signing hooks", ErrAuthentication)
	}
	provider := abi.Provider{Profile: p.profile.ID, Options: options}
	result, err := signer.Sign(ctx, p.profile.Revision, provider, abi.SignRequest{
		Profile: p.profile.ID, Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body, Credential: string(credential),
	}, secrets)
	if err != nil {
		return nil, fmt.Errorf("%w: the plugin's signing hook failed: %w", ErrAuthentication, err)
	}
	if len(result.Headers) > maxSignedHeaders {
		return nil, fmt.Errorf("%w: the plugin's signing hook returned more than %d headers", ErrAuthentication, maxSignedHeaders)
	}
	signed := make([]string, 0, len(result.Headers))
	for _, name := range slices.Sorted(maps.Keys(result.Headers)) {
		value := result.Headers[name]
		// Like a declared header, a signed one is OLP's to refuse; unlike
		// one, it never replaces anything the request already carries.
		if !ConfigurableHeader(name) || slices.ContainsFunc(p.profile.SemanticHeaders, func(semantic string) bool { return strings.EqualFold(semantic, name) }) ||
			len(req.Header.Values(name)) > 0 || !httpguts.ValidHeaderFieldValue(value) {
			return nil, fmt.Errorf("%w: the plugin's signing hook returned header %q, which a signature can't add", ErrAuthentication, name)
		}
		req.Header.Set(name, value)
		signed = append(signed, value)
	}
	return signed, nil
}
