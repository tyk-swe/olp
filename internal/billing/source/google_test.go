package source

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/egress"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGoogleADCRefusesMetadataHostOutsideEgressPolicy(t *testing.T) {
	var hits atomic.Int32
	metadata := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Metadata-Flavor", "Google")
		_, _ = w.Write([]byte(`{"access_token":"marker-from-metadata","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer metadata.Close()
	t.Setenv("GCE_METADATA_HOST", strings.TrimPrefix(metadata.URL, "http://"))
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "")
	t.Setenv("HOME", t.TempDir())
	transport := googleTransport{
		base: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("BigQuery request sent with a metadata token")
		}),
		auth:       connectors.NewBillingAuth(&egress.Policy{}, &http.Client{Transport: http.DefaultTransport}),
		credential: googleADC{},
	}
	request := httptest.NewRequest(http.MethodPost, googleBigQueryEndpoint+"/bigquery/v2/projects/p/queries", nil)
	if _, err := transport.RoundTrip(request); !errors.Is(err, errGoogleAuthentication) {
		t.Fatalf("got %v, want Google authentication failure", err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("metadata host outside the egress policy received %d requests", got)
	}
}
