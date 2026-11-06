package usage

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
)

func TestRoutingPriceSelectsApplicableRevision(t *testing.T) {
	now := time.Unix(1700000000, 0)
	price := func(id, kind, model, operation string, provider, vendor *string, at time.Time, revision int) RoutingPrice {
		return RoutingPrice{Price: Price{ProviderKind: kind, Model: model, Operation: operation, ProviderID: provider, VendorID: vendor, InputPerMillion: new("1.25"), OutputPerMillion: new("2.5"), Currency: "USD"}, RevisionID: id, Revision: revision, EffectiveAt: at}
	}
	prices := []RoutingPrice{
		price("kind", "openai", "model", "generation", nil, nil, now.Add(-time.Hour), 1),
		price("vendor", "openai", "model", "generation", nil, new("vendor"), now.Add(-time.Hour), 1),
		price("provider", "openai", "model", "generation", new("provider"), nil, now.Add(-2*time.Hour), 1),
		price("provider-newer", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 1),
		price("provider-revised", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 2),
		price("provider-tied", "openai", "model", "generation", new("provider"), nil, now.Add(-time.Hour), 2),
		price("scheduled", "openai", "model", "generation", new("provider"), nil, now.Add(time.Second), 3),
		price("both", "openai", "model", "generation", new("provider"), new("specific"), now.Add(-3*time.Hour), 1),
		price("other-kind", "anthropic", "model", "generation", nil, nil, now, 1),
		price("other-model", "openai", "other", "generation", nil, nil, now, 1),
		price("other-operation", "openai", "model", "embeddings", nil, nil, now, 1),
	}
	for _, indexed := range []bool{false, true} {
		inputs := &RoutingInputs{Prices: prices, RefreshedAt: now}
		if indexed {
			inputs.priceIndex = indexRoutingPrices(prices)
		}
		for _, tc := range []struct {
			name, kind, model, operation, provider, vendor, want string
			at                                                   time.Time
			priority                                             int
		}{
			{"provider precedence and revision tie", "openai", "model", "generation", "provider", "vendor", "provider-revised", now, 2},
			{"vendor fallback", "openai", "model", "generation", "other", "vendor", "vendor", now, 1},
			{"kind fallback", "openai", "model", "generation", "other", "other", "kind", now, 0},
			{"both scope constraints", "openai", "model", "generation", "provider", "specific", "both", now, 3},
			{"scheduled boundary", "openai", "model", "generation", "provider", "vendor", "scheduled", now.Add(time.Second), 2},
			{"freshness boundary", "openai", "model", "generation", "other", "vendor", "vendor", now.Add(PerformanceMaxAge), 1},
			{"stale", "openai", "model", "generation", "provider", "vendor", "", now.Add(PerformanceMaxAge + time.Nanosecond), 0},
			{"missing bucket", "openai", "missing", "generation", "provider", "vendor", "", now, 0},
			{"kind isolation", "anthropic", "model", "generation", "provider", "vendor", "other-kind", now, 0},
			{"model isolation", "openai", "other", "generation", "provider", "vendor", "other-model", now, 0},
			{"operation isolation", "openai", "model", "embeddings", "provider", "vendor", "other-operation", now, 0},
		} {
			t.Run(tc.name+"/indexed="+fmt.Sprint(indexed), func(t *testing.T) {
				got := inputs.Price(tc.kind, tc.provider, tc.vendor, tc.model, tc.operation, tc.at)
				if tc.want == "" {
					if got != nil {
						t.Fatalf("unexpected price %+v", got)
					}
					return
				}
				if got == nil || got.RevisionID != tc.want || got.ScopePriority != tc.priority || got.ProviderID == nil || *got.ProviderID != tc.provider {
					t.Fatalf("price %+v, want revision %s for provider %s with priority %d", got, tc.want, tc.provider, tc.priority)
				}
				if got.InputPerMillion == nil || *got.InputPerMillion != *prices[0].InputPerMillion || got.OutputPerMillion == nil || *got.OutputPerMillion != *prices[0].OutputPerMillion || got.Currency != prices[0].Currency {
					t.Fatalf("price lost its rates or currency: %+v", got)
				}
				original := *got
				got.ProviderID = new("changed")
				got.Revision = -1
				if again := inputs.Price(tc.kind, tc.provider, tc.vendor, tc.model, tc.operation, tc.at); !reflect.DeepEqual(again, &original) {
					t.Fatal("modifying the returned price changed later requests")
				}
			})
		}
	}
}

// usageProblem asserts that an error is a management problem and returns it, so
// a test can pin the status and code the console will actually see.
func usageProblem(t *testing.T, err error) *access.Problem {
	t.Helper()
	var problem *access.Problem
	if !errors.As(err, &problem) {
		t.Fatalf("error %v is not a management problem", err)
	}
	return problem
}

func samplePriceEntry() Price {
	rate := "1.5"
	return Price{ProviderKind: "openai", Model: "gpt-test", Operation: "generation",
		InputPerMillion: &rate, Currency: "USD"}
}

func sampleVendorKind(vendor string) (string, bool) {
	kinds := map[string]string{"openai": "openai", "acme": "openai_compatible"}
	kind, ok := kinds[vendor]
	return kind, ok
}

func TestPriceValidationRejectsUnusableEntries(t *testing.T) {
	unknownVendor, mismatchedVendor := "nowhere", "acme"
	badProvider := "not-a-uuid"
	zero := "0"
	for _, c := range []struct {
		name   string
		mutate func(*Price)
	}{
		{"an unknown connector kind", func(p *Price) { p.ProviderKind = "quantum" }},
		{"an unknown operation", func(p *Price) { p.Operation = "telepathy" }},
		{"a blank model", func(p *Price) { p.Model = "   " }},
		{"a two letter currency", func(p *Price) { p.Currency = "US" }},
		{"a non alphabetic currency", func(p *Price) { p.Currency = "U$D" }},
		{"no rate at all", func(p *Price) { p.InputPerMillion = nil }},
		{"a cached rate without an input rate", func(p *Price) {
			p.InputPerMillion, p.CachedInputPerMillion = nil, &zero
			p.OutputPerMillion = &zero
		}},
		{"an unknown vendor", func(p *Price) { p.VendorID = &unknownVendor }},
		{"a vendor from another connector", func(p *Price) { p.VendorID = &mismatchedVendor }},
		{"a malformed provider override", func(p *Price) { p.ProviderID = &badProvider }},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := samplePriceEntry()
			c.mutate(&entry)
			_, _, err := validatePrices([]Price{entry}, sampleVendorKind)
			if err == nil {
				t.Fatal("the entry was accepted")
			}
			if problem := usageProblem(t, err); problem.Status != 422 {
				t.Fatalf("status = %d, want 422", problem.Status)
			}
		})
	}
}

type cardRevisionTx struct {
	pgx.Tx
	execError error
	row       pgx.Row
}

func (tx cardRevisionTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, tx.execError
}

func (tx cardRevisionTx) QueryRow(context.Context, string, ...any) pgx.Row { return tx.row }

type pricingFailureRow struct{ err error }

func (row pricingFailureRow) Scan(...any) error { return row.err }

type pricingCurrencyRow string

func (row pricingCurrencyRow) Scan(destinations ...any) error {
	if len(destinations) != 1 {
		return errors.New("expected one currency destination")
	}
	currency, ok := destinations[0].(*string)
	if !ok {
		return errors.New("expected a string currency destination")
	}
	*currency = string(row)
	return nil
}

func TestCardRevisionRespectsCurrencyFailures(t *testing.T) {
	lockFailure := errors.New("currency lock unavailable")
	readFailure := errors.New("currency state unavailable")
	for _, tc := range []struct {
		name     string
		explicit bool
		tx       cardRevisionTx
		want     error
	}{
		{"lock failure", true, cardRevisionTx{execError: lockFailure, row: pricingFailureRow{readFailure}}, lockFailure},
		{"currency read failure", true, cardRevisionTx{row: pricingFailureRow{readFailure}}, readFailure},
		{"markup-only bypasses currency", false, cardRevisionTx{execError: lockFailure, row: pricingFailureRow{readFailure}}, readFailure},
		{"conflicting currency", true, cardRevisionTx{row: pricingCurrencyRow("EUR")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prices := func() []Price {
				if tc.explicit {
					return []Price{samplePriceEntry()}
				}
				return nil
			}()
			_, err := CreateCardRevision(t.Context(), tc.tx, access.NewID(), access.NewID(), "1", time.Now().UTC(), prices, "USD", sampleVendorKind)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("currency failure = %v, want %v", err, tc.want)
				}
				return
			}
			if problem := usageProblem(t, err); problem.Status != 422 {
				t.Fatalf("currency conflict = %v", problem)
			}
		})
	}
}
