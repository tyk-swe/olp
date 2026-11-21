package providers

import (
	"reflect"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
)

func TestCompareListingModelsClassifiesAgainstThePublishedScope(t *testing.T) {
	t.Parallel()
	scope := listingScope{endpoint: "https://upstream.example", apiVersion: "2023-06-01", credentialID: "credential-published", credentialVersion: 3}
	listedBefore := func() *routing.RevisionListing {
		return &routing.RevisionListing{Endpoint: scope.endpoint, APIVersion: scope.apiVersion, CredentialID: scope.credentialID, CredentialVersion: scope.credentialVersion, ListedAt: time.Unix(1700000000, 0).UTC()}
	}
	rotated := listedBefore()
	rotated.CredentialID, rotated.CredentialVersion = "credential-rotated", 4
	otherEndpoint := listedBefore()
	otherEndpoint.Endpoint = "https://other.example"
	published := []routing.RevisionModel{
		{ID: "kept", UpstreamModel: "model-kept", DisplayName: "Kept", Listing: listedBefore()},
		{ID: "renamed", UpstreamModel: "model-renamed", DisplayName: "Old name", Listing: listedBefore()},
		{ID: "quiet", UpstreamModel: "model-quiet", DisplayName: "Quiet name", Listing: listedBefore()},
		{ID: "gone", UpstreamModel: "model-gone", DisplayName: "Gone", Listing: listedBefore()},
		{ID: "manual", UpstreamModel: "model-manual", DisplayName: "Manual"},
		{ID: "rotated", UpstreamModel: "model-rotated", DisplayName: "Rotated", Listing: rotated},
		{ID: "elsewhere", UpstreamModel: "model-elsewhere", DisplayName: "Elsewhere", Listing: otherEndpoint},
	}
	listed := []probe.DiscoveredModel{
		{Name: "model-kept", Display: "Kept", HasDisplay: true},
		{Name: "model-renamed", Display: "New name", HasDisplay: true},
		{Name: "model-quiet", Display: "model-quiet"},
		{Name: "model-brand-new", Display: "Brand New", HasDisplay: true},
	}
	routes := map[string][]listingRoute{
		"model-gone":    {{ID: "route-gone", Slug: "chat"}},
		"model-manual":  {{ID: "route-manual", Slug: "support"}},
		"model-rotated": {{ID: "route-rotated", Slug: "internal"}},
	}
	newModels, changed, notListed, unverified := compareListingModels(published, listed, scope, routes)
	if want := []listingNewModel{{UpstreamModel: "model-brand-new", DisplayName: "Brand New"}}; !reflect.DeepEqual(newModels, want) {
		t.Fatalf("new models %+v, want %+v", newModels, want)
	}
	if want := []listingDisplayChange{{UpstreamModel: "model-renamed", PublishedDisplayName: "Old name", ListedDisplayName: "New name"}}; !reflect.DeepEqual(changed, want) {
		t.Fatalf("display changes %+v, want %+v", changed, want)
	}
	if want := []listingOmission{{UpstreamModel: "model-gone", DisplayName: "Gone", Routes: routes["model-gone"]}}; !reflect.DeepEqual(notListed, want) {
		t.Fatalf("not listed %+v, want %+v", notListed, want)
	}
	wantUnverified := []string{"model-elsewhere", "model-manual", "model-rotated"}
	gotUnverified := make([]string, len(unverified))
	for i, omission := range unverified {
		gotUnverified[i] = omission.UpstreamModel
	}
	if !reflect.DeepEqual(gotUnverified, wantUnverified) {
		t.Fatalf("unverified %v, want %v", gotUnverified, wantUnverified)
	}
}

func TestVisibleListingRoutesHideRoutesOutsideTheCallersScope(t *testing.T) {
	t.Parallel()
	readable, hidden := "project-readable", "project-hidden"
	principal := authz.Principal{Projects: map[string]string{readable: "viewer"}}
	rows := []listingRouteRow{
		{ID: "route-visible", Slug: "chat", ProjectID: &readable, Model: "model-gone"},
		{ID: "route-visible", Slug: "chat", ProjectID: &readable, Model: "model-gone"},
		{ID: "route-hidden", Slug: "secret-chat", ProjectID: &hidden, Model: "model-gone"},
		{ID: "route-global", Slug: "global-chat", ProjectID: nil, Model: "model-gone"},
		{ID: "route-other", Slug: "batch", ProjectID: &readable, Model: "model-other"},
	}
	routes := visibleListingRoutes(principal, rows)
	want := map[string][]listingRoute{
		"model-gone":  {{ID: "route-visible", Slug: "chat"}},
		"model-other": {{ID: "route-other", Slug: "batch"}},
	}
	if !reflect.DeepEqual(routes, want) {
		t.Fatalf("visible routes %+v, want %+v", routes, want)
	}
}
