package pricebook

import (
	"errors"
	"net/http"

	"github.com/tyk-swe/olp/internal/domain/problem"
	"github.com/tyk-swe/olp/internal/keyauthority"
)

var (
	ErrRateCardExists  = errors.New("the owner already has a rate card")
	ErrCatalogOutdated = errors.New("the catalog is older than one this source already took")
)

func invalid(field, detail string) error {
	return &keyauthority.ValidationError{Field: field, Detail: detail}
}

// Problem gives a price book failure its public HTTP form.
func Problem(err error) error {
	switch {
	case errors.Is(err, ErrRateCardExists):
		return problem.Fail(http.StatusConflict, "rate_card_exists", "The owner already has a rate card.")
	case errors.Is(err, ErrCatalogOutdated):
		return problem.Fail(http.StatusUnprocessableEntity, "catalog_outdated", "The catalog is older than one this source already took.")
	default:
		return err
	}
}
