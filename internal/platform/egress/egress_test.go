package egress

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestValidateEndpointRefusesNonPublicAddresses(t *testing.T) {
	listed := Policy{PlainHTTPHosts: []string{"127.0.0.1"}}
	for _, c := range []struct {
		policy   Policy
		endpoint string
	}{
		{Policy{}, "https://127.0.0.1/v1"},
		{Policy{}, "https://10.0.0.8/v1"},
		{Policy{}, "https://172.16.4.4"},
		{Policy{}, "https://192.168.1.1"},
		{Policy{}, "https://169.254.169.254/latest/meta-data"},
		{Policy{}, "https://100.64.0.1"},
		{Policy{}, "https://0.0.0.0"},
		{Policy{}, "https://[::1]/v1"},
		{Policy{}, "https://[::ffff:10.0.0.1]/v1"},
		{Policy{}, "https://[fd00::1]"},
		{Policy{}, "https://[fe80::1]"},
		{Policy{}, "https://[64:ff9b::7f00:1]"},
		{Policy{}, "https://[2002:7f00:1::]"},
		{Policy{}, "https://[2001::1]"},
		{Policy{}, "https://[2001:db8::1]"},
		{listed, "http://127.0.0.1:8080/v1"},
	} {
		if _, err := c.policy.ValidateEndpoint(c.endpoint); !errors.Is(err, ErrUnsafeDestination) {
			t.Errorf("%s: got %v, want %v", c.endpoint, err, ErrUnsafeDestination)
		}
	}
}

func TestValidateEndpointRefusesMalformedEndpoints(t *testing.T) {
	for _, endpoint := range []string{
		"",
		" https://api.example.com",
		"api.example.com/v1",
		"//api.example.com/v1",
		"http://api.example.com/v1",
		"ftp://api.example.com/v1",
		"https://user:secret@api.example.com/v1",
		"https://api.example.com/v1?key=secret",
		"https://api.example.com/v1?",
		"https://api.example.com/v1#section",
		"https://api.example.com/v1/../admin",
		"https://api.example.com:99999/v1",
		"https://api_internal.example.com/v1",
		"https://-api.example.com/v1",
	} {
		if got, err := (Policy{}).ValidateEndpoint(endpoint); err == nil {
			t.Errorf("%q was accepted as %s", endpoint, got)
		}
	}
}

func TestValidateEndpointNormalizesAcceptedEndpoints(t *testing.T) {
	policy := Policy{
		AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("10.1.0.0/16")},
		PlainHTTPHosts:  []string{"Gateway.Internal"},
	}
	for _, c := range []struct{ endpoint, want string }{
		{"https://API.Example.com/v1/", "https://api.example.com/v1"},
		{"https://api.example.com:8443", "https://api.example.com:8443"},
		{"https://10.1.2.3/v1", "https://10.1.2.3/v1"},
		{"http://gateway.internal:8080/v1//", "http://gateway.internal:8080/v1"},
	} {
		got, err := policy.ValidateEndpoint(c.endpoint)
		if err != nil {
			t.Errorf("%s: %v", c.endpoint, err)
			continue
		}
		if got.String() != c.want {
			t.Errorf("%s: got %s, want %s", c.endpoint, got, c.want)
		}
	}
	if _, err := policy.ValidateEndpoint("https://10.2.0.1/v1"); !errors.Is(err, ErrUnsafeDestination) {
		t.Errorf("an address outside the exception was accepted: %v", err)
	}
}

func TestPermitsAppliesExceptionsToTheirOwnAddressFamily(t *testing.T) {
	policy := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("fd12::/16")}}
	for address, want := range map[string]bool{
		"127.0.0.1":        true,
		"::ffff:127.0.0.1": true,
		"fd12::1":          true,
		"::1":              false,
		"fd13::1":          false,
		"10.0.0.1":         false,
		"4000::1":          false,
		"2606:4700::1111":  true,
		"8.8.8.8":          true,
	} {
		if got := policy.Permits(netip.MustParseAddr(address)); got != want {
			t.Errorf("Permits(%s) = %v, want %v", address, got, want)
		}
	}
}

func TestClientPinsDialsToPermittedAddressesAndRefusesRedirects(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "reached")
	}))
	t.Cleanup(target.Close)
	redirecting := httptest.NewServer(http.RedirectHandler(target.URL, http.StatusFound))
	t.Cleanup(redirecting.Close)
	loopback := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}

	response, err := loopback.Client(time.Second).Get(target.URL)
	if err != nil {
		t.Fatalf("a permitted destination was refused: %v", err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if string(body) != "reached" {
		t.Fatalf("permitted destination answered %q", body)
	}
	if _, err := (Policy{}).Client(time.Second).Get(target.URL); !errors.Is(err, ErrUnsafeDestination) {
		t.Errorf("a loopback dial without an exception: got %v, want %v", err, ErrUnsafeDestination)
	}
	if _, err := loopback.Client(time.Second).Get(redirecting.URL); !errors.Is(err, ErrRedirect) {
		t.Errorf("a redirecting upstream: got %v, want %v", err, ErrRedirect)
	}
}

func TestValidateConnectionRefusesProxiesOnNonPublicAddresses(t *testing.T) {
	for proxy, want := range map[string]bool{
		"https://proxy.example.com:3128":  true,
		"socks5://proxy.example.com:1080": true,
		"socks5://127.0.0.1:1080":         false,
		"https://10.0.0.1:3128":           false,
		"http://proxy.example.com:3128":   false,
		"socks5h://proxy.example.com":     false,
		"https://proxy.example.com/path":  false,
	} {
		err := (Policy{}).ValidateConnection(&ConnectionOptions{ProxyURL: proxy})
		if (err == nil) != want {
			t.Errorf("%s: accepted=%v, want %v (%v)", proxy, err == nil, want, err)
		}
	}
}

func TestValidateConnectionSecretRefusesAmbiguousDocuments(t *testing.T) {
	if err := ValidateConnectionSecret([]byte(`{"proxy_username":"agent","proxy_password":"secret"}`)); err != nil {
		t.Fatalf("a proxy credential was refused: %v", err)
	}
	for _, secret := range []string{
		``,
		`[]`,
		`{}`,
		`{"proxy_username":"agent","proxy_username":"other"}`,
		`{"proxy_username":null}`,
		`{"proxy_username":"agent","token":"extra"}`,
		`{"proxy_username":"agent"} {"proxy_username":"other"}`,
		`{"proxy_password":"secret"}`,
		`{"proxy_username":"agent:other"}`,
		`{"proxy_username":"agent","proxy_password":"line\nbreak"}`,
		`{"proxy_username":"agent","client_certificate_pem":"-----BEGIN CERTIFICATE-----"}`,
	} {
		if err := ValidateConnectionSecret([]byte(secret)); err == nil {
			t.Errorf("%s was accepted", secret)
		}
	}
}

func TestRedactScrubsEveryEncodingOfACredential(t *testing.T) {
	var sensitive Sensitive
	sensitive.Add("sk-a", "  sk-a\"b<c\t", "")
	got := sensitive.Redact(`raw=sk-a"b<c escaped=sk-a\"b\u003cc lax=sk-a\"b<c short=sk-a`)
	if want := "raw=[REDACTED] escaped=[REDACTED] lax=[REDACTED] short=[REDACTED]"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !sensitive.Contains("Bearer sk-a") || sensitive.Contains("Bearer sk-b") {
		t.Errorf("Contains did not track the recorded values")
	}
}

func TestApplyCredentialHeadersRequiresTheConfiguredNames(t *testing.T) {
	names := []string{"X-Api-Key", "x-organization"}
	header := http.Header{}
	if err := ApplyCredentialHeaders(header, names, []byte(`{"x-api-key":"key","X-Organization":"org"}`)); err != nil {
		t.Fatal(err)
	}
	if header.Get("X-Api-Key") != "key" || header.Get("X-Organization") != "org" {
		t.Fatalf("configured headers were not applied: %v", header)
	}
	for _, credential := range []string{
		`not json`,
		`{}`,
		`{"x-api-key":"key"}`,
		`{"x-api-key":"key","x-other":"org"}`,
		`{"x-api-key":"key","X-API-KEY":"again"}`,
		`{"x-api-key":"key","x-organization":null}`,
		`{"x-api-key":"key","x-organization":"org\r\nX-Injected: 1"}`,
	} {
		untouched := http.Header{}
		if err := ApplyCredentialHeaders(untouched, names, []byte(credential)); err == nil {
			t.Errorf("%s was accepted", credential)
		}
		if len(untouched) != 0 {
			t.Errorf("%s changed the request before failing: %v", credential, untouched)
		}
	}
}
