package connectors

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/plugin/abi"
)

type Signer interface {
	Sign(ctx context.Context, digest string, provider abi.Provider, request abi.SignRequest, secrets []string) (abi.SignResult, error)
}

const maxSignedHeaders = 16

var ErrSigningUnavailable = errors.New("the plugin's signing hook is unavailable")

func (p *PluginProfile) sign(ctx context.Context, signer Signer, options map[string]string, req *http.Request, credential, body []byte, secrets []string) ([]string, error) {
	if !p.declared.Signing {
		return nil, nil
	}
	if signer == nil {
		return nil, fmt.Errorf("%w: this process runs no plugin signing hooks", ErrSigningUnavailable)
	}
	token, _, err := p.credential(credential)
	if err != nil {
		return nil, err
	}
	provider := abi.Provider{Profile: p.profile.ID, Options: options}
	result, err := signer.Sign(ctx, p.profile.Revision, provider, abi.SignRequest{
		Profile: p.profile.ID, Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body, Credential: token,
	}, secrets)
	if err != nil {
		if reported, ok := errors.AsType[*abi.Error](err); ok && reported.Code != abi.CodeInternal {
			return nil, fmt.Errorf("%w: the plugin's signing hook failed: %w", ErrAuthentication, err)
		}
		return nil, fmt.Errorf("%w: the plugin's signing hook failed: %w", ErrSigningUnavailable, err)
	}
	if len(result.Headers) > maxSignedHeaders {
		return nil, fmt.Errorf("%w: the plugin's signing hook returned more than %d headers", ErrSigningUnavailable, maxSignedHeaders)
	}
	signed := make([]string, 0, len(result.Headers))
	for _, name := range slices.Sorted(maps.Keys(result.Headers)) {
		value := result.Headers[name]
		// Like a declared header, a signed one is OLP's to refuse; unlike
		// one, it never replaces anything the request already carries.
		if !ConfigurableHeader(name) || slices.ContainsFunc(p.profile.SemanticHeaders, func(semantic string) bool { return strings.EqualFold(semantic, name) }) ||
			len(req.Header.Values(name)) > 0 || !httpguts.ValidHeaderFieldValue(value) {
			// The hook may put credentials in the name as well as the value.
			return nil, fmt.Errorf("%w: the plugin's signing hook returned a header which a signature can't add", ErrSigningUnavailable)
		}
		req.Header.Set(name, value)
		signed = append(signed, value)
	}
	return signed, nil
}
