package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/sdk/plugin/abi"
)

// loopbackClient is the provider network path of a test: the egress policy
// admits the loopback servers the tests start.
func loopbackClient(t *testing.T) *http.Client {
	t.Helper()
	policy := egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	client, err := policy.ConnectionClient(nil, nil, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func fetch(t *testing.T, grant *HTTP, request abi.HTTPRequest) (abi.HTTPResponse, *abi.Error) {
	t.Helper()
	params, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var response abi.HTTPResponse
	data, failure := serveHTTP(context.WithValue(t.Context(), httpKey{}, grant), params)
	if failure == nil {
		if err = json.Unmarshal(data, &response); err != nil {
			t.Fatal(err)
		}
	}
	return response, failure
}

func TestHTTPReachesOnlyApprovedOriginsOverTheProviderNetworkPath(t *testing.T) {
	var elsewhere atomic.Int64
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	approved := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, other.URL, http.StatusFound)
		case "/large":
			w.Write([]byte(strings.Repeat("x", maxHTTPBody+1)))
		default:
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Seen", r.Method+" "+r.Header.Get("Content-Type"))
			w.WriteHeader(http.StatusCreated)
			w.Write(body)
		}
	}))
	defer approved.Close()
	grant := &HTTP{Origins: []string{approved.URL}, Client: loopbackClient(t)}

	response, failure := fetch(t, grant, abi.HTTPRequest{Method: "POST", URL: approved.URL + "/token", Header: map[string][]string{"Content-Type": {"application/x-www-form-urlencoded"}}, Body: []byte("grant_type=x")})
	if failure != nil || response.Status != http.StatusCreated || string(response.Body) != "grant_type=x" || response.Header["X-Seen"][0] != "POST application/x-www-form-urlencoded" {
		t.Fatalf("approved request: %+v %v", response, failure)
	}
	// A redirect comes back to the plugin rather than leading it elsewhere.
	if response, failure = fetch(t, grant, abi.HTTPRequest{Method: "GET", URL: approved.URL + "/redirect"}); failure != nil || response.Status != http.StatusFound || response.Header["Location"][0] != other.URL {
		t.Fatalf("redirect: %+v %v", response, failure)
	}
	for name, tc := range map[string]struct {
		grant   *HTTP
		request abi.HTTPRequest
		code    string
	}{
		"unapproved origin":               {grant, abi.HTTPRequest{Method: "GET", URL: other.URL}, abi.CodeOriginNotApproved},
		"approved server by another name": {grant, abi.HTTPRequest{Method: "GET", URL: strings.Replace(approved.URL, "127.0.0.1", "localhost", 1)}, abi.CodeOriginNotApproved},
		"transport header":                {grant, abi.HTTPRequest{Method: "GET", URL: approved.URL, Header: map[string][]string{"Host": {"elsewhere"}}}, abi.CodeInvalidRequest},
		"method":                          {grant, abi.HTTPRequest{Method: "CONNECT", URL: approved.URL}, abi.CodeInvalidRequest},
		"credentials in the URL":          {grant, abi.HTTPRequest{Method: "GET", URL: strings.Replace(approved.URL, "//", "//user:pass@", 1)}, abi.CodeInvalidRequest},
		"large response":                  {grant, abi.HTTPRequest{Method: "GET", URL: approved.URL + "/large"}, abi.CodeHTTPFailed},
		// The provider's network path carries the egress policy, which here
		// admits no loopback address.
		"egress policy": {&HTTP{Origins: grant.Origins, Client: must(egress.Policy{}.ConnectionClient(nil, nil, time.Second))}, abi.HTTPRequest{Method: "GET", URL: approved.URL}, abi.CodeHTTPFailed},
		"not granted":   {nil, abi.HTTPRequest{Method: "GET", URL: approved.URL}, abi.CodeUnknownMethod},
	} {
		t.Run(name, func(t *testing.T) {
			if _, failure := fetch(t, tc.grant, tc.request); failure == nil || failure.Code != tc.code {
				t.Fatalf("want %s, got %v", tc.code, failure)
			}
		})
	}
	if elsewhere.Load() != 0 {
		t.Fatal("a request reached an origin the owner did not approve")
	}
}

func must[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

// The reference plugin enrolls a grant against a fake authority: grant_start
// builds the authorization request with state and PKCE, and grant_exchange
// reaches the authority only through the HTTP capability.
func TestReferencePluginEnrollsAGrant(t *testing.T) {
	t.Parallel()
	authority := testutil.NewOAuthServer(t)
	r := newTestRuntime(t, DefaultLimits, nil)
	m, err := r.Load(t.Context(), testutil.BuildPlugin(t, "./sdk/plugin/reference", "-X=main.authority="+authority.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close(t.Context())
	grant := &HTTP{Origins: []string{"https://api.example.com", authority.URL}, Client: loopbackClient(t)}

	var authorization abi.GrantAuthorization
	if err = m.Call(t.Context(), Call{Method: abi.MethodGrantStart, Params: abi.GrantStart{Profile: "reference-grant-chat"}}, &authorization); err != nil {
		t.Fatal(err)
	}
	request, err := url.Parse(authorization.URL)
	if err != nil || !strings.HasPrefix(authorization.URL, authority.URL+"/authorize?") || request.Query().Get("state") == "" || request.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("authorization request %q", authorization.URL)
	}
	browser := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	signedIn, err := browser.Get(authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	signedIn.Body.Close()
	callback, _ := url.Parse(signedIn.Header.Get("Location"))
	code, state := callback.Query().Get("code"), callback.Query().Get("state")
	exchange := func(input string, http *HTTP) (abi.Grant, error) {
		var enrolled abi.Grant
		err := m.Call(t.Context(), Call{Method: abi.MethodGrantExchange, Params: abi.GrantExchange{Profile: "reference-grant-chat", Session: authorization.Session, Input: input}, HTTP: http}, &enrolled)
		return enrolled, err
	}
	for _, tc := range []struct {
		input string
		http  *HTTP
		code  string
	}{
		// Another sign-in's value is refused before the plugin reaches out.
		{strings.Replace(callback.String(), "state="+state, "state=another", 1), grant, abi.CodeStateMismatch},
		{code + "#another", grant, abi.CodeStateMismatch},
		// Without the HTTP capability, the plugin can't reach its authority.
		{code + "#" + state, nil, abi.CodeUnknownMethod},
	} {
		if _, err = exchange(tc.input, tc.http); !reported(err, tc.code) {
			t.Fatalf("%q: want %s, got %v", tc.input, tc.code, err)
		}
	}
	// Out of band, the operator pastes code#state.
	enrolled, err := exchange(code+"#"+state, grant)
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewRequest("GET", "/", nil)
	upstream.Header.Set("Authorization", "Bearer "+enrolled.AccessToken)
	// The token response names no regional API, so the account uses the
	// shared one.
	if identity, ok := authority.Authorized(upstream); !ok || enrolled.Principal != identity.Subject || enrolled.Facts["account"] != identity.Account ||
		enrolled.Facts["api_base"] != "https://api.example.com/v1" || enrolled.RefreshToken == "" || enrolled.ExpiresIn != 3600 {
		t.Fatalf("enrolled %+v", enrolled)
	}
	// The authority spends a code once.
	if _, err = exchange(callback.String(), grant); !reported(err, "invalid_grant") {
		t.Fatalf("exchanged a spent code: %v", err)
	}
}

func reported(err error, code string) bool {
	reported, ok := errors.AsType[*abi.Error](err)
	return ok && reported.Code == code
}
