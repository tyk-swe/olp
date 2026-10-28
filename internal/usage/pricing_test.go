package usage

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/tyk-swe/olp/internal/access"
)

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
		{"conflicting currency", true, cardRevisionTx{row: signalTestRow{"EUR"}}, nil},
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
