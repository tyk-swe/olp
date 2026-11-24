package providers

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access/authz"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/connectors/providerconfig"
	"github.com/tyk-swe/olp/internal/domain/vendors"
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

func listingValues(listing *routing.RevisionListing) (endpoint, apiVersion, credentialID *string, credentialVersion *int, listedAt *time.Time) {
	if listing == nil {
		return nil, nil, nil, nil, nil
	}
	return &listing.Endpoint, &listing.APIVersion, &listing.CredentialID, &listing.CredentialVersion, &listing.ListedAt
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

type listingDifferences struct {
	NewModels           []listingNewModel      `json:"new_models"`
	ChangedDisplayNames []listingDisplayChange `json:"changed_display_names"`
	NotListed           []listingOmission      `json:"not_listed"`
	Unverified          []listingOmission      `json:"unverified"`
}

type listingOutcome struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
	listingDifferences
}

type listingComparison struct {
	ProviderID        string    `json:"provider_id"`
	RevisionID        string    `json:"revision_id"`
	Revision          int       `json:"revision"`
	CheckedAt         time.Time `json:"checked_at"`
	APIVersion        string    `json:"api_version"`
	CredentialVersion int       `json:"credential_version"`
	listingOutcome
}

func compareListingModels(published []routing.RevisionModel, listed []probe.DiscoveredModel, scope listingScope, routes map[string][]listingRoute) listingDifferences {
	differences := noListingDifferences()
	publishedByModel := map[string]routing.RevisionModel{}
	for _, model := range published {
		publishedByModel[model.UpstreamModel] = model
	}
	listedModels := map[string]bool{}
	for _, model := range listed {
		listedModels[model.Name] = true
		before, known := publishedByModel[model.Name]
		if !known {
			differences.NewModels = append(differences.NewModels, listingNewModel{UpstreamModel: model.Name, DisplayName: model.Display})
			continue
		}
		if model.HasDisplay && model.Display != before.DisplayName {
			differences.ChangedDisplayNames = append(differences.ChangedDisplayNames, listingDisplayChange{UpstreamModel: model.Name, PublishedDisplayName: before.DisplayName, ListedDisplayName: model.Display})
		}
	}
	for _, model := range published {
		if listedModels[model.UpstreamModel] {
			continue
		}
		omission := listingOmission{UpstreamModel: model.UpstreamModel, DisplayName: model.DisplayName, Routes: append([]listingRoute{}, routes[model.UpstreamModel]...)}
		if scope.matches(model.Listing) {
			differences.NotListed = append(differences.NotListed, omission)
		} else {
			differences.Unverified = append(differences.Unverified, omission)
		}
	}
	slices.SortFunc(differences.NewModels, func(a, b listingNewModel) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(differences.ChangedDisplayNames, func(a, b listingDisplayChange) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(differences.NotListed, func(a, b listingOmission) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
	slices.SortFunc(differences.Unverified, func(a, b listingOmission) int { return strings.Compare(a.UpstreamModel, b.UpstreamModel) })
	return differences
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
			return cmp.Or(strings.Compare(a.Slug, b.Slug), strings.Compare(a.ID, b.ID))
		})
	}
	return routes
}

func errListingUnsupported() error {
	return httpkit.Fail(422, "listing_comparison_unsupported", "Listing comparison supports a native anthropic provider with one enabled, stored API-key credential slot.")
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
	contract, known := vendors.Lookup(providerconfig.Value(cfg.Options.VendorID))
	if (known && !contract.Discovery) || cfg.Kind != connectors.KindAnthropic || cfg.AuthMode != connectors.AuthAPIKey || cfg.CredentialSource == "caller" {
		return probe.Target{}, nil, listingScope{}, errListingUnsupported()
	}
	var enabled []routing.RevisionSlot
	for _, slot := range revision.Slots {
		if slot.Enabled {
			enabled = append(enabled, slot)
		}
	}
	if len(enabled) != 1 || enabled[0].CredentialID == nil || enabled[0].CredentialVersion == nil {
		return probe.Target{}, nil, listingScope{}, errListingUnsupported()
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
	scope := listingScope{
		endpoint:          providerconfig.Value(cfg.Endpoint),
		apiVersion:        providerconfig.Value(cfg.APIVersion),
		credentialID:      *slot.CredentialID,
		credentialVersion: *slot.CredentialVersion,
	}
	return probe.Target{ProviderID: current.ID, Connection: connection.Stored(cfg), Credential: credential}, revision, scope, nil
}

func (s *Server) publishedRoutesByModel(ctx context.Context, providerID string, p authz.Principal) (map[string][]listingRoute, error) {
	rows, err := s.access.Pool.Query(ctx, `SELECT r.id::text,r.slug,r.project_id::text,t->>'provider_model'
		FROM olp.routes r JOIN olp.route_revisions v ON v.id=r.latest_revision_id
		CROSS JOIN LATERAL jsonb_array_elements(v.targets) t
		WHERE r.state='active' AND t->>'provider_id'=$1 ORDER BY r.slug`, providerID)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[listingRouteRow])
	if err != nil {
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
	outcome, err := s.compareWithListing(r.Context(), p, target, revision.Models, scope)
	if err != nil {
		return httpkit.Reply{}, err
	}
	return httpkit.OK(listingComparison{
		ProviderID:        id,
		RevisionID:        revision.ID,
		Revision:          revision.Revision,
		CheckedAt:         at,
		APIVersion:        scope.apiVersion,
		CredentialVersion: scope.credentialVersion,
		listingOutcome:    outcome,
	}), nil
}

func (s *Server) compareWithListing(ctx context.Context, p authz.Principal, target probe.Target, published []routing.RevisionModel, scope listingScope) (listingOutcome, error) {
	listing, err := s.prober.ListCompleteModelFacts(ctx, target)
	if err != nil {
		return incompleteListing(probe.Classify(err).Detail), nil
	}
	if !listing.Complete {
		return incompleteListing(listing.Detail), nil
	}
	routes, err := s.publishedRoutesByModel(ctx, target.ProviderID, p)
	if err != nil {
		return listingOutcome{}, err
	}
	return listingOutcome{
		Status:             "complete",
		Detail:             fmt.Sprintf("Compared %d listed models with %d published models.", len(listing.Models), len(published)),
		listingDifferences: compareListingModels(published, listing.Models, scope, routes),
	}, nil
}

func incompleteListing(detail string) listingOutcome {
	return listingOutcome{
		Status:             "incomplete",
		Detail:             detail,
		listingDifferences: noListingDifferences(),
	}
}

func noListingDifferences() listingDifferences {
	return listingDifferences{NewModels: []listingNewModel{}, ChangedDisplayNames: []listingDisplayChange{}, NotListed: []listingOmission{}, Unverified: []listingOmission{}}
}
