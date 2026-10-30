package resources

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
	"github.com/tyk-swe/olp/internal/connectors"
	"github.com/tyk-swe/olp/internal/routing"
)

type codeOffersQuery struct {
	access.Queryer
	rows pgx.Rows
	row  pgx.Row
	err  error
}

func (q codeOffersQuery) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return q.rows, q.err
}

func (q codeOffersQuery) QueryRow(context.Context, string, ...any) pgx.Row {
	return q.row
}

type codeOfferTargetRow struct {
	pgx.Row
	restrictions  []byte
	configuration []byte
	limits        []byte
	err           error
}

func (r codeOfferTargetRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for index, value := range [][]byte{r.restrictions, r.configuration, r.limits} {
		destination, ok := dest[index].(*[]byte)
		if !ok {
			return errors.New("paid offer target destination has incompatible type")
		}
		*destination = value
	}
	return nil
}

type codeOfferRow struct {
	publication []byte
	models      []string
	adapter     *string
	err         error
}

// pgx.Rows requires a mutable cursor and caller-owned Scan destinations.
type codeOfferRows struct {
	pgx.Rows
	data   []codeOfferRow
	cursor int
	err    error
}

func (r *codeOfferRows) Close()     {}
func (r *codeOfferRows) Err() error { return r.err }
func (r *codeOfferRows) Next() bool {
	r.cursor++
	return r.cursor <= len(r.data)
}
func (r *codeOfferRows) Scan(dest ...any) error {
	row := r.data[r.cursor-1]
	if row.err != nil {
		return row.err
	}
	if len(dest) != 3 {
		return errors.New("account offer needs models and adapter destinations")
	}
	models, validModels := dest[0].(*[]string)
	adapter, validAdapter := dest[1].(**string)
	publication, validPublication := dest[2].(*[]byte)
	if !validModels || !validAdapter || !validPublication {
		return errors.New("account offer destinations have incompatible types")
	}
	*models = row.models
	*adapter = row.adapter
	*publication = row.publication
	return nil
}

func TestCodeOffersPreservesUsableAccountModelGroups(t *testing.T) {
	models := []string{"private-main", "private-plan", "private-other", "private-unrecognized"}
	rows := &codeOfferRows{data: []codeOfferRow{
		{adapter: new(string(codemode.AdapterZAICoding)), models: models[:2]},
		{models: models[3:]},
		{adapter: new(string(codemode.AdapterZAICoding)), models: models[2:3]},
	}}
	offers, err := CodeOffers(t.Context(), codeOffersQuery{rows: rows}, "published-revision")
	if err != nil {
		t.Fatal(err)
	}
	route := codemode.Route{Slug: "coding", Enabled: true, RevisionID: "published-revision", Models: models}
	configuration, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientClaudeCode, Model: models[0], PlanModel: models[1]})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(configuration.NativeModels, models[:3]) || configuration.Model != models[0] || configuration.PlanModel == nil || *configuration.PlanModel != models[1] {
		t.Fatalf("eligible account models were changed or an unrecognized adapter leaked %+v", configuration)
	}
	if _, err := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientClaudeCode, Model: models[0], PlanModel: models[2]}); err == nil {
		t.Fatal("models from disjoint accounts became usable in one conversation")
	}
}

func TestCodeOffersPropagatesDataSourceFailure(t *testing.T) {
	failure := errors.New("account catalog source unavailable")
	valid := codeOfferRow{adapter: new(string(codemode.AdapterCodex)), models: []string{"private-model"}}
	for _, tc := range []struct {
		name   string
		source codeOffersQuery
	}{
		{name: "query", source: codeOffersQuery{err: failure}},
		{name: "scan after valid account", source: codeOffersQuery{rows: &codeOfferRows{data: []codeOfferRow{valid, {err: failure}}}}},
		{name: "stream after valid account", source: codeOffersQuery{rows: &codeOfferRows{data: []codeOfferRow{valid}, err: failure}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := CodeOffers(t.Context(), tc.source, "published-revision"); !errors.Is(err, failure) {
				t.Fatalf("data-source failure was reported as success or replaced, got %v", err)
			}
		})
	}
}

func TestCodeOffersInvalidPaidPublicationFailsClosed(t *testing.T) {
	source := codeOffersQuery{rows: &codeOfferRows{data: []codeOfferRow{{publication: []byte("{")}}}}
	if _, err := CodeOffers(t.Context(), source, "published-revision"); err == nil {
		t.Fatal("invalid paid publication was treated as an empty catalog")
	}
}

func TestCodeOffersPreservesModelPoolDiscoveryWhenPaidAuthorityChanges(t *testing.T) {
	for _, test := range []struct {
		name               string
		publishedKeys      []string
		liveRestrictions   routing.Slot
		liveLimits         routing.Slot
		liveProviderLimits *routing.Limits
		sourceErr          error
		available          bool
		management         bool
	}{
		{name: "eligible paid target", available: true},
		{name: "authorized restricted key", publishedKeys: []string{"caller-key"}, liveRestrictions: routing.Slot{AllowedAPIKeys: []string{"caller-key"}}, available: true},
		{name: "published key restriction", publishedKeys: []string{"another-key"}},
		{name: "live key restriction", liveRestrictions: routing.Slot{AllowedAPIKeys: []string{"another-key"}}},
		{name: "live model restriction", liveRestrictions: routing.Slot{AllowedModels: []string{"another-model"}}},
		{name: "live route restriction", liveRestrictions: routing.Slot{AllowedRoutes: []string{"another-route"}}},
		{name: "live slot quota changed", liveLimits: routing.Slot{TokensPerMinute: new(int64(10))}},
		{name: "live provider quota changed", liveProviderLimits: &routing.Limits{RequestsPerMinute: new(int64(10))}},
		{name: "credential unavailable", sourceErr: pgx.ErrNoRows},
		{name: "management setup with key restrictions", management: true, publishedKeys: []string{"caller-key"}, liveRestrictions: routing.Slot{AllowedAPIKeys: []string{"caller-key"}}, available: true},
		{name: "management live model restriction", management: true, liveRestrictions: routing.Slot{AllowedModels: []string{"another-model"}}},
		{name: "management live route restriction", management: true, liveRestrictions: routing.Slot{AllowedRoutes: []string{"another-route"}}},
		{name: "management credential unavailable", management: true, sourceErr: pgx.ErrNoRows},
	} {
		t.Run(test.name, func(t *testing.T) {
			models := []string{"native-main", "native-alternate", "paid-model"}
			route := codemode.Route{Slug: "coding", Enabled: true, RevisionID: "published-revision", Models: models,
				ModelRewriting: true, Aliases: map[string]string{"team-code": models[0]}, ModelPools: map[string][]string{"team-code": models[:2]},
				PaidParticipation: true, PaidTargets: map[string]codemode.PaidTarget{models[2]: {ProviderID: "paid-provider", SlotID: "paid-slot"}}}
			provider := routing.Provider{ID: "paid-provider", Kind: "openai_compatible", ProfileID: "compatible-chat", ProfileRevision: connectors.ProfileRevision, Capabilities: []routing.Capability{{Model: models[2], Operation: "generation", Surface: "openai", Mode: "unary"}},
				Slots: []routing.Slot{{ID: "paid-slot", Enabled: true, CredentialID: new("paid-credential"), CredentialVersion: new(1), AllowedAPIKeys: test.publishedKeys}}}
			publication, publicationErr := json.Marshal(struct {
				Route       codemode.Route              `json:"route"`
				Connections map[string]routing.Provider `json:"connections"`
			}{Route: route, Connections: map[string]routing.Provider{provider.ID: provider}})
			restrictions, restrictionsErr := json.Marshal(test.liveRestrictions)
			limits, limitsErr := json.Marshal(test.liveLimits)
			liveConfigurationJSON, liveConfigurationError := json.Marshal(map[string]map[string]*routing.Limits{"options": {"limits": test.liveProviderLimits}})
			if err := errors.Join(publicationErr, restrictionsErr, limitsErr, liveConfigurationError); err != nil {
				t.Fatal(err)
			}
			source := codeOffersQuery{rows: &codeOfferRows{data: []codeOfferRow{{publication: publication, adapter: new(string(codemode.AdapterCodex)), models: models[:2]}}},
				row: codeOfferTargetRow{restrictions: restrictions, configuration: liveConfigurationJSON, limits: limits, err: test.sourceErr}}
			keys := func() []string {
				if test.management {
					return nil
				}
				return []string{"caller-key"}
			}()
			offers, err := CodeOffers(t.Context(), source, route.RevisionID, keys...)
			if err != nil {
				t.Fatal(err)
			}
			catalog := codeclient.Catalog(route, offers)
			paid := slices.IndexFunc(catalog, func(model codemode.Model) bool { return model.ID == models[2] && model.Paid })
			if (paid >= 0) != test.available {
				t.Fatalf("paid catalog ignored current target authority %+v", catalog)
			}
			if test.management && paid >= 0 {
				configuration, configurationErr := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientOpenCode, Model: models[2]})
				if configurationErr != nil || configuration.Model != models[2] || !slices.Contains(configuration.PaidModels, models[2]) {
					t.Fatalf("paid setup unavailable %+v, %v", configuration, configurationErr)
				}
			}
			public := slices.IndexFunc(catalog, func(model codemode.Model) bool { return model.ID == "team-code" })
			if public < 0 || !slices.Equal(catalog[public].ModelPool, route.ModelPools[catalog[public].ID]) {
				t.Fatalf("paid authority change removed subscription pool discovery %+v", catalog)
			}
			configuration, configurationErr := codeclient.Generate(route, offers, codeclient.Request{GatewayURL: "https://gateway.example", Client: codeadapter.ClientCodex, Model: catalog[public].ID})
			if configurationErr != nil || configuration.Model != catalog[public].ID {
				t.Fatalf("paid authority change affected subscription setup %+v, %v", configuration, configurationErr)
			}
		})
	}
}
