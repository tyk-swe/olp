package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/http/httpguts"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// HTTP is the HTTP capability OLP may grant a call: requests only to the
// plugin's approved origins, sent by Client, which the caller gives the
// provider's network path under the egress policy. A redirect is returned to
// the plugin rather than followed, so every request it makes is checked.
type HTTP struct {
	// Origins are the plugin's approved origins, in canonical form.
	Origins []string
	Client  *http.Client
}

// maxHTTPBody bounds a body the capability carries either way, so that the
// body, in base64 within a JSON message, stays within the ABI's bound.
const maxHTTPBody = 512 << 10

type httpKey struct{}

// ownedHeaders are the request headers OLP's transport sets itself.
var ownedHeaders = []string{"Host", "Content-Length", "Transfer-Encoding", "Connection", "Keep-Alive", "Te", "Trailer", "Upgrade", "Proxy-Authorization", "Proxy-Connection"}

// serveHTTP serves the http capability for the call ctx belongs to.
func serveHTTP(ctx context.Context, params json.RawMessage) (json.RawMessage, *abi.Error) {
	grant, _ := ctx.Value(httpKey{}).(*HTTP)
	if grant == nil {
		return nil, &abi.Error{Code: abi.CodeUnknownMethod, Message: "OLP grants this call no capability named " + abi.CapabilityHTTP + "."}
	}
	var request abi.HTTPRequest
	if json.Unmarshal(params, &request) != nil {
		return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: "An http request carries an HTTP request."}
	}
	req, failure := grant.request(ctx, request)
	if failure != nil {
		return nil, failure
	}
	client := *grant.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(req)
	if err != nil {
		return nil, &abi.Error{Code: abi.CodeHTTPFailed, Message: "OLP could not complete the request: " + err.Error()}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxHTTPBody+1))
	if err != nil {
		return nil, &abi.Error{Code: abi.CodeHTTPFailed, Message: "OLP could not read the response: " + err.Error()}
	}
	if len(body) > maxHTTPBody {
		return nil, &abi.Error{Code: abi.CodeHTTPFailed, Message: "The response body exceeds 512 KiB."}
	}
	data, err := json.Marshal(abi.HTTPResponse{Status: response.StatusCode, Header: response.Header, Body: body})
	if err != nil {
		return nil, &abi.Error{Code: abi.CodeInternal, Message: "OLP could not encode the response."}
	}
	return data, nil
}

// request builds the request a plugin asked for, refusing one to an origin
// the owner did not approve or one that sets what OLP's transport owns.
func (h *HTTP) request(ctx context.Context, r abi.HTTPRequest) (*http.Request, *abi.Error) {
	invalid := func(message string) (*http.Request, *abi.Error) {
		return nil, &abi.Error{Code: abi.CodeInvalidRequest, Message: message}
	}
	if !slices.Contains([]string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete}, r.Method) {
		return invalid("Use the GET, POST, PUT, PATCH or DELETE method.")
	}
	target, err := url.Parse(r.URL)
	if err != nil || (target.Scheme != "https" && target.Scheme != "http") || target.Host == "" || target.User != nil || target.Fragment != "" {
		return invalid("Send the request to an absolute http or https URL without credentials or a fragment.")
	}
	if origin := connectors.Origin(target); !slices.Contains(h.Origins, origin) {
		return nil, &abi.Error{Code: abi.CodeOriginNotApproved, Message: "The plugin may reach only the origins an owner approved, and " + origin + " is not one of them."}
	}
	if len(r.Body) > maxHTTPBody {
		return invalid("The request body exceeds 512 KiB.")
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, target.String(), bytes.NewReader(r.Body))
	if err != nil {
		return invalid("OLP can't send this request.")
	}
	for name, values := range r.Header {
		if !httpguts.ValidHeaderFieldName(name) || slices.Contains(ownedHeaders, http.CanonicalHeaderKey(name)) || strings.HasPrefix(http.CanonicalHeaderKey(name), "Proxy-") {
			return invalid("Header " + name + " is not one a plugin may set.")
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return invalid("Header " + name + " has an invalid value.")
			}
			req.Header.Add(name, value)
		}
	}
	return req, nil
}
