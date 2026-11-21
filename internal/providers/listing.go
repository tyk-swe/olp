package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/authz"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/domain/vendors"
	"github.com/tyk-swe/olp/internal/providerconfig"
	"github.com/tyk-swe/olp/internal/providers/connection"
	"github.com/tyk-swe/olp/internal/providers/probe"
	"github.com/tyk-swe/olp/internal/routing"
)

type listingScope struct {
	endpoint          string
	apiVersion        string
	credentialID      string
	credentialVersion int
}

func (scope listingScope) matches(listing *routing.RevisionListing) bool {
	return listing != nil &&
		listing.Endpoint == scope.endpoint &&
		listing.APIVersion == scope.apiVersion &&
		listing.CredentialID == scope.credentialID &&
		listing.CredentialVersion == scope.credentialVersion
}

func completeListingProvenance(cfg connection.Configuration, slot *slotRow, at time.Time) *routing.RevisionListing {
	if cfg.AuthMode != connectors.AuthAPIKey || cfg.CredentialSource == "caller" || slot == nil || slot.CredentialID == nil || slot.CredentialVersion == nil {
		return nil
	}
	return &routing.RevisionListing{
		Endpoint:          providerconfig.Value(cfg.Endpoint),
		APIVersion:        providerconfig.Value(cfg.APIVersion),
		CredentialID:      *slot.CredentialID,
		CredentialVersion: *slot.CredentialVersion,
		ListedAt:          at,
	}
}

func listingValues(listing *routing.RevisionListing) (endpoint, apiVersion, credentialID, credentialVersion, listedAt any) {
	if listing == nil {
		return nil, nil, nil, nil, nil
	}
	return listing.Endpoint, listing.APIVersion, listing.CredentialID, listing.CredentialVersion, listing.ListedAt
}

type listingNewModel struct {
	UpstreamModel string `json:"upstream_model"`
	DisplayName   string `json:"display_name"`
}

type listingDisplayChange struct {
	UpstreamModel        string `json:"upstream_model"`
	PublishedDisplayName string `json:"published_display_name"`
	ListedDisplayName    string `json:"listed_display_name"`
}

type listingRoute struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
}

type listingOmission struct {
	UpstreamModel string         `json:"upstream_model"`
	DisplayName   string         `json:"display_name"`
	Routes        []listingRoute `json:"routes"`
}

type listingComparison struct {
	ProviderID          string                 `json:"provider_id"`
	RevisionID          string                 `json:"revision_id"`
	Revision            int                    `json:"revision"`
	Status              string                 `json:"status"`
	Detail              string                 `json:"detail"`
	CheckedAt           time.Time              `json:"checked_at"`
	APIVersion          string                 `json:"api_version"`
	CredentialVersion   int                    `json:"credential_version"`
	NewModels           []listingNewModel      `json:"new_models"`
	ChangedDisplayNames []listingDisplayChange `json:"changed_display_names"`
	NotListed           []listingOmission      `json:"not_listed"`
	Unverified          []listingOmission      `json:"unverified"`
}

func compareListingModels(published []routing.RevisionModel, listed []probe.DiscoveredModel, scope listingScope, routes map[string][]listingRoute) ([]listingNewModel, []listingDisplayChange, []listingOmission, []listingOmission) {
	newModels := []listingNewModel{}
	changed := []listingDisplayChange{}
	notListed := []listingOmission{}
	unverified := []listingOmission{}
	publishedByModel := map[string]routing.RevisionModel{}
	for _, model := range published {
		publishedByModel[model.UpstreamModel] = model
	}
	listedByModel := map[string]probe.DiscoveredModel{}
	for _, model := range listed {
		if _, seen := listedByModel[model.Name]; !seen {
			listedByModel[model.Name] = model
		}
	}
	for _, model := range listed {
		before, known := publishedByModel[model.Name]
		if !known {
			newModels = append(newModels, listingNewModel{UpstreamModel: model.Name, DisplayName: model.Display})
			continue
		}
		if model.HasDisplay && model.Display != before.DisplayName {
			changed = append(changed, listingDisplayChange{UpstreamModel: model.Name, PublishedDisplayName: before.DisplayName, ListedDisplayName: model.Display})
		}
	}
	for _, model := range published {
		if _, listed := listedByModel[model.UpstreamModel]; listed {
			continue
		}
		modelRoutes := routes[model.UpstreamModel]
		if modelRoutes == nil {
			modelRoutes = []listingRoute{}
		}
		omission := listingOmission{UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Routes: modelRoutes}
		if scope.matches(model.Listing) {
			notListed = append(notListed, omission)
		} else {
			unverified = append(unverified, omission)
		}
	}
	slices.SortFunc(newModels, func(a, b listingNewModel) int { return compareStrings(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(changed, func(a, b listingDisplayChange) int { return compareStrings(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(notListed, func(a, b listingOmission) int { return compareStrings(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(unverified, func(a, b listingOmission) int { return compareStrings(a.UpstreamModel, b.UpstreamModel) })
	return newModels, changed, notListed, unverified
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

type listingRouteRow struct {
	ID        string
	Slug      string
	ProjectID *string
	Model     string
}

func visibleListingRoutes(p authz.Principal, rows []listingRouteRow) map[string][]listingRoute {
	routes := map[string][]listingRoute{}
	seen := map[string]bool{}
	for _, row := range rows {
		if p.Project(row.ProjectID, authz.View) != nil {
			continue
		}
		key := row.Model + "\x00" + row.ID
		if seen[key] {
			continue
		}
		seen[key] = true
		routes[row.Model] = append(routes[row.Model], listingRoute{ID: row.ID, Slug: row.Slug})
	}
	for model := range routes {
		slices.SortFunc(routes[model], func(a, b listingRoute) int {
			if a.Slug != b.Slug {
				return compareStrings(a.Slug, b.Slug)
			}
			return compareStrings(a.ID, b.ID)
		})
	}
	return routes
}

func (s *Server) publishedListingTarget(ctx context.Context, current *record) (probe.Target, *revisionRow, listingScope, error) {
	if current.State != stateActive || current.ActiveRevisionID == nil {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "no_active_revision", "Activate a revision before comparing its published inventory with the upstream listing.")
	}
	revision, err := loadRevision(ctx, s.access.Pool, current.ID, *current.ActiveRevisionID)
	if err != nil {
		return probe.Target{}, nil, listingScope{}, err
	}
	cfg := revision.Configuration
	if contract, ok := vendors.Lookup(providerconfig.Value(cfg.Options.VendorID)); ok && !contract.Discovery {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "listing_comparison_unsupported", "Listing comparison supports a native anthropic provider with one enabled, stored API-key credential slot.")
	}
	if cfg.Kind != connectors.KindAnthropic || cfg.AuthMode != connectors.AuthAPIKey || cfg.CredentialSource == "caller" {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "listing_comparison_unsupported", "Listing comparison supports a native anthropic provider with one enabled, stored API-key credential slot.")
	}
	var enabled []routing.RevisionSlot
	for _, slot := range revision.Slots {
		if slot.Enabled {
			enabled = append(enabled, slot)
		}
	}
	if len(enabled) != 1 || enabled[0].CredentialID == nil || enabled[0].CredentialVersion == nil {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "listing_comparison_unsupported", "Listing comparison supports a native anthropic provider with one enabled, stored API-key credential slot.")
	}
	slot := enabled[0]
	var version int
	var revoked bool
	if err := s.access.Pool.QueryRow(ctx, "SELECT version,revoked_at IS NOT NULL FROM olp.provider_credentials WHERE id=$1 AND provider_id=$2", *slot.CredentialID, current.ID).Scan(&version, &revoked); err != nil {
		return probe.Target{}, nil, listingScope{}, err
	}
	if revoked {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "credential_revoked", "The published credential version was revoked, so its listing cannot be compared.")
	}
	if version != *slot.CredentialVersion {
		return probe.Target{}, nil, listingScope{}, httpkit.Fail(422, "credential_mismatch", "The published credential version no longer matches the stored credential.")
	}
	credential, err := s.readCredential(ctx, s.access.Pool, *slot.CredentialID)
	if err != nil {
		return probe.Target{}, nil, listingScope{}, err
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		clear(credential)
		return probe.Target{}, nil, listingScope{}, err
	}
	prepared, err := connection.Load(encoded, nil)
	if err != nil {
		clear(credential)
		return probe.Target{}, nil, listingScope{}, err
	}
	scope := listingScope{
		endpoint:          providerconfig.Value(cfg.Endpoint),
		apiVersion:        providerconfig.Value(cfg.APIVersion),
		credentialID:      *slot.CredentialID,
		credentialVersion: *slot.CredentialVersion,
	}
	return probe.Target{ProviderID: current.ID, Connection: prepared, Credential: credential}, revision, scope, nil
}

func (s *Server) publishedRoutesByModel(ctx context.Context, providerID string, p authz.Principal) (map[string][]listingRoute, error) {
	rows, err := s.access.Pool.Query(ctx, `SELECT r.id::text,r.slug,r.project_id::text,t->>'provider_model'
		FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id
		CROSS JOIN LATERAL jsonb_array_elements(v.targets) t
		WHERE r.state='active' AND t->>'provider_id'=$1 ORDER BY r.slug`, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var found []listingRouteRow
	for rows.Next() {
		var row listingRouteRow
		if err := rows.Scan(&row.ID, &row.Slug, &row.ProjectID, &row.Model); err != nil {
			return nil, err
		}
		found = append(found, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return visibleListingRoutes(p, found), nil
}

func (s *Server) compareListing(r *http.Request, p authz.Principal) (httpkit.Reply, error) {
	id, err := httpkit.IDParam(r, "provider_id")
	if err != nil {
		return httpkit.Reply{}, err
	}
	current, err := visibleProvider(r.Context(), s.access.Pool, p, id)
	if err != nil {
		return httpkit.Reply{}, err
	}
	if err := p.Project(current.ProjectID, authz.Change); err != nil {
		return httpkit.Reply{}, err
	}
	target, revision, scope, err := s.publishedListingTarget(r.Context(), current)
	if err != nil {
		return httpkit.Reply{}, err
	}
	defer clear(target.Credential)
	at := time.Now().UTC()
	result := listingComparison{
		ProviderID:          id,
		RevisionID:          revision.ID,
		Revision:            revision.Revision,
		Status:              "incomplete",
		CheckedAt:           at,
		APIVersion:          scope.apiVersion,
		CredentialVersion:   scope.credentialVersion,
		NewModels:           []listingNewModel{},
		ChangedDisplayNames: []listingDisplayChange{},
		NotListed:           []listingOmission{},
		Unverified:          []listingOmission{},
	}
	listing, err := s.prober.ListCompleteModelFacts(r.Context(), target)
	if err != nil {
		result.Detail = probe.Classify(err).Detail
		return httpkit.OK(result), nil
	}
	if !listing.Complete {
		result.Detail = listing.Detail
		return httpkit.OK(result), nil
	}
	routes, err := s.publishedRoutesByModel(r.Context(), id, p)
	if err != nil {
		return httpkit.Reply{}, err
	}
	result.Status = "complete"
	result.Detail = fmt.Sprintf("Compared %d listed models with %d published models.", len(listing.Models), len(revision.Models))
	result.NewModels, result.ChangedDisplayNames, result.NotListed, result.Unverified = compareListingModels(revision.Models, listing.Models, scope, routes)
	return httpkit.OK(result), nil
}
