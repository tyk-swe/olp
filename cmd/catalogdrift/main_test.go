package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestEveryListingReadsModelsFromItsVendorsReviewedEndpoint(t *testing.T) {
	t.Parallel()
	for _, l := range listings {
		target, err := l.url()
		if err != nil {
			t.Errorf("%s: %v", l.vendor, err)
			continue
		}
		parsed, err := url.Parse(target)
		if err != nil || parsed.Scheme != "https" || !strings.HasSuffix(parsed.Path, "/models") {
			t.Errorf("%s lists models at %q, want an HTTPS models URL", l.vendor, target)
		}
	}
	if target, err := (listing{vendor: "a vendor without a contract"}).url(); err == nil {
		t.Errorf("a vendor without a contract lists models at %q", target)
	}
}
