package main

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/domain/vendors"
)

type recordedListing struct {
	listing listing
	asked   *http.Request
}

func (r *recordedListing) RoundTrip(request *http.Request) (*http.Response, error) {
	r.asked = request
	body, err := json.Marshal(map[string][]map[string]string{r.listing.array: {{r.listing.id: r.listing.trimPrefix + "listed-model"}}})
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(strings.NewReader(string(body))), Request: request}, nil
}

func TestEveryListingAsksItsVendorsReviewedEndpointForTheWholeModelList(t *testing.T) {
	t.Parallel()
	for _, l := range listings {
		contract, _ := vendors.Lookup(l.vendor)
		recorded := &recordedListing{listing: l}
		models, err := l.models(t.Context(), &http.Client{Transport: recorded}, "a-listing-key")
		if err != nil || !slices.Equal(models, []string{"listed-model"}) {
			t.Errorf("%s listed %v, %v, want the one model its vendor answered", l.vendor, models, err)
			continue
		}
		asked := recorded.asked.URL
		if !strings.HasPrefix(asked.String(), contract.Endpoint+"/") || asked.Scheme != "https" || asked.RawQuery != strings.TrimPrefix(l.query, "?") {
			t.Errorf("%s asked %s, want its models under %s with the query %q", l.vendor, asked, contract.Endpoint, l.query)
		}
	}
	unreviewed := &recordedListing{}
	if _, err := (listing{vendor: "a vendor without a contract"}).models(t.Context(), &http.Client{Transport: unreviewed}, "a-listing-key"); err == nil || unreviewed.asked != nil {
		t.Errorf("a vendor without a contract was asked for its models at %v", unreviewed.asked)
	}
}
