package pricebook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/authority"
	"github.com/tyk-swe/olp/internal/usage/rates"
)

func samplePriceEntry() rates.Price {
	rate := "1.5"
	return rates.Price{ProviderKind: "openai", Model: "gpt-test", Operation: "generation",
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
		mutate func(*rates.Price)
	}{
		{"an unknown connector kind", func(p *rates.Price) { p.ProviderKind = "quantum" }},
		{"an unknown operation", func(p *rates.Price) { p.Operation = "telepathy" }},
		{"a blank model", func(p *rates.Price) { p.Model = "   " }},
		{"a two letter currency", func(p *rates.Price) { p.Currency = "US" }},
		{"a non alphabetic currency", func(p *rates.Price) { p.Currency = "U$D" }},
		{"no rate at all", func(p *rates.Price) { p.InputPerMillion = nil }},
		{"a cached rate without an input rate", func(p *rates.Price) {
			p.InputPerMillion, p.CachedInputPerMillion = nil, &zero
			p.OutputPerMillion = &zero
		}},
		{"an unknown vendor", func(p *rates.Price) { p.VendorID = &unknownVendor }},
		{"a vendor from another connector", func(p *rates.Price) { p.VendorID = &mismatchedVendor }},
		{"a malformed provider override", func(p *rates.Price) { p.ProviderID = &badProvider }},
	} {
		t.Run(c.name, func(t *testing.T) {
			entry := samplePriceEntry()
			c.mutate(&entry)
			_, _, err := validatePrices([]rates.Price{entry}, NewNormalizer(sampleVendorKind))
			if err == nil {
				t.Fatal("the entry was accepted")
			}
			var problem *authority.ValidationError
			if !errors.As(err, &problem) {
				t.Fatalf("error = %v, want a validation failure", err)
			}
		})
	}
}

func TestPricesRejectOperationsThatCarryNoPrice(t *testing.T) {
	for _, operation := range []string{"file", "guardrail"} {
		entry := samplePriceEntry()
		entry.Operation = operation
		if _, _, err := validatePrices([]rates.Price{entry}, NewNormalizer(sampleVendorKind)); err == nil {
			t.Fatalf("accepted a price for %s", operation)
		}
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
			prices := func() []rates.Price {
				if tc.explicit {
					return []rates.Price{samplePriceEntry()}
				}
				return nil
			}()
			_, err := CreateCardRevision(t.Context(), tc.tx, access.NewID(), access.NewID(), "1", time.Now().UTC(), prices, "USD", NewNormalizer(sampleVendorKind))
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("currency failure = %v, want %v", err, tc.want)
				}
				return
			}
			var problem *authority.ValidationError
			if !errors.As(err, &problem) {
				t.Fatalf("currency conflict = %v", problem)
			}
		})
	}
}

func TestPriceBookFailuresKeepTheirPublicForm(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		status      int
		code, field string
	}{
		{"validation", invalid("prices", "Use a supported provider kind."), 422, "validation_failed", "prices"},
		{"owner already carded", fmt.Errorf("create: %w", ErrRateCardExists), 409, "rate_card_exists", ""},
		{"catalog replay", ErrCatalogOutdated, 422, "catalog_outdated", ""},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			access.WriteProblem(recorder, Problem(test.err))
			var body struct {
				Type   string                     `json:"type"`
				Errors map[string]json.RawMessage `json:"errors"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			_, carriesField := body.Errors[test.field]
			if recorder.Code != test.status || !strings.HasSuffix(body.Type, "/"+test.code) || carriesField != (test.field != "") {
				t.Fatalf("response = %d %s %v, want %d %s %q", recorder.Code, body.Type, body.Errors, test.status, test.code, test.field)
			}
		})
	}
}
