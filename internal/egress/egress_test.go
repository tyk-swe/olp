package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/testutil"
	"github.com/tyk-swe/olp/tests/fixtures"
)

type endpointCase struct {
	Name                    string `json:"name"`
	URL                     string `json:"url"`
	AcceptedAtConfiguration bool   `json:"accepted_at_configuration"`
	RequiresResolutionCheck bool   `json:"requires_resolution_check"`
}

func TestValidateEndpointCorpus(t *testing.T) {
	cases := testutil.JSON[[]endpointCase](t, fixtures.Files, "security/custom-endpoints.json")
	var policy Policy
	for _, c := range cases {
		parsed, err := policy.ValidateEndpoint(c.URL)
		if (err == nil) != c.AcceptedAtConfiguration {
			t.Errorf("%s: accepted=%v err=%v", c.Name, err == nil, err)
		}
		if err == nil && parsed.Host == "" {
			t.Errorf("%s: parsed host missing", c.Name)
		}
	}
}

func TestOperatorExceptions(t *testing.T) {
	policy := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	if _, err := policy.ValidateEndpoint("http://127.0.0.1:8080/v1"); err != nil {
		t.Fatalf("exception rejected: %v", err)
	}
	if _, err := policy.ValidateEndpoint("http://localhost:8080/v1"); err == nil {
		t.Fatal("plain http accepted for a host outside the exception list")
	}
	if _, err := policy.ValidateEndpoint("https://[::1]/v1"); err == nil {
		t.Fatal("IPv6 loopback accepted without an exception")
	}
	if _, err := policy.ValidateEndpoint("https://api.openai.com/v1/"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRejectsUnsafeAnswers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := (Policy{}).Resolve(ctx, "localhost"); !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("expected unsafe destination, got %v", err)
	}
	loopback := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}}
	if _, err := loopback.Resolve(ctx, "localhost"); err != nil {
		t.Fatalf("loopback exception rejected: %v", err)
	}
}

func TestClientRefusesRedirectsAndPinsDial(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/other", http.StatusFound)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer upstream.Close()
	blocked := (Policy{}).Client(time.Second)
	if _, err := blocked.Get(upstream.URL + "/"); err == nil || !errors.Is(err, ErrUnsafeDestination) {
		t.Fatalf("loopback dial should be refused without an exception: %v", err)
	}
	allowed := Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}}.Client(time.Second)
	response, err := allowed.Get(upstream.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err = allowed.Get(upstream.URL + "/redirect"); err == nil || !errors.Is(err, ErrRedirect) {
		t.Fatalf("redirect should be refused: %v", err)
	}
}
