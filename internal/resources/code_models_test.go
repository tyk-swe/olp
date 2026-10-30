package resources

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/codeadapter"
	"github.com/tyk-swe/olp/internal/codeclient"
	"github.com/tyk-swe/olp/internal/codemode"
)

type codeOffersQuery struct {
	access.Queryer
	rows pgx.Rows
	err  error
}

func (q codeOffersQuery) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return q.rows, q.err
}

type codeOfferRow struct {
	models  []string
	adapter *string
	err     error
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
	if len(dest) != 2 {
		return errors.New("account offer needs models and adapter destinations")
	}
	models, validModels := dest[0].(*[]string)
	adapter, validAdapter := dest[1].(**string)
	if !validModels || !validAdapter {
		return errors.New("account offer destinations have incompatible types")
	}
	*models = row.models
	*adapter = row.adapter
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
