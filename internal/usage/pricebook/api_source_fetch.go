package pricebook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/pricingpolicy"
	"github.com/tyk-swe/olp/internal/signing"
)

func (s *API) fetchSource(ctx context.Context, source Source) (fetchedSource, error) {
	if source.Format == FormatCatalog {
		return s.fetchCatalog(ctx, source.URL)
	}
	if source.URL == nil {
		return fetchedSource{}, errors.New("stored price list source has no URL")
	}
	return s.fetchPriceDocument(ctx, *source.URL)
}

// fetchBounded GETs a source document through the egress policy. A price
// list must declare JSON; a signed catalog may come as anything, because its
// signature, not its media type, establishes what it is.
func (s *API) fetchBounded(ctx context.Context, rawURL string, limit int64, requireJSON bool) ([]byte, error) {
	if s.Egress == nil {
		return nil, access.Fail(503, "egress_policy_unavailable",
			"Source fetching is not configured on this installation.")
	}
	client := s.Egress.Client(sourceFetchTimeout)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, access.Fail(502, "source_fetch_failed", "The pricing source could not be fetched.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
		return nil, access.Fail(502, "source_fetch_failed", "The pricing source did not answer successfully.")
	}
	if requireJSON {
		mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
			return nil, access.Fail(415, "invalid_source_document",
				"The pricing source did not return a JSON document.")
		}
	}
	body, err := readBounded(response.Body, limit)
	if err != nil {
		return nil, access.Fail(413, "source_document_too_large",
			fmt.Sprintf("The pricing source document exceeds the %d MiB limit.", limit>>20))
	}
	return body, nil
}

func (s *API) fetchPriceDocument(ctx context.Context, rawURL string) (fetchedSource, error) {
	body, err := s.fetchBounded(ctx, rawURL, maxSourceDocumentBytes, true)
	if err != nil {
		return fetchedSource{}, err
	}
	var doc sourceDocument
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var trailing any
	if err := decoder.Decode(&doc); err != nil || doc.Prices == nil ||
		decoder.Decode(&trailing) != io.EOF {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source document is not a valid pricing catalogue.")
	}
	doc.Currency = strings.ToUpper(strings.TrimSpace(doc.Currency))
	if !pricingpolicy.ValidCurrency(doc.Currency) {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source document does not declare a valid currency.")
	}
	return s.validatedSource(doc, nil, nil)
}

// fetchCatalog reads a signed reference catalog: the one this release ships,
// or one fetched with its detached signature at the same URL plus ".sig".
// The signature must verify against the keys this build trusts before any
// price in it is read, and a catalog older than the one this release ships
// is refused.
func (s *API) fetchCatalog(ctx context.Context, rawURL *string) (fetchedSource, error) {
	signed := s.Catalog
	if rawURL != nil {
		document, err := s.fetchBounded(ctx, *rawURL, catalog.MaxBytes, false)
		if err != nil {
			return fetchedSource{}, err
		}
		signatureAt, err := signatureURL(*rawURL)
		if err != nil {
			return fetchedSource{}, access.Fail(422, "invalid_url", "The source URL is not permitted by egress policy.")
		}
		signature, err := s.fetchBounded(ctx, signatureAt, signing.MaxSignatureFile, false)
		if err != nil {
			return fetchedSource{}, err
		}
		signed, err = catalog.Load(document, signature, s.CatalogKeys)
		switch {
		case errors.Is(err, signing.ErrInvalidSignature) || errors.Is(err, signing.ErrUnknownKey) || errors.Is(err, signing.ErrMalformedSignature):
			return fetchedSource{}, access.Fail(422, "catalog_signature_invalid",
				"The catalog's signature does not verify against the keys this release trusts.")
		case errors.Is(err, signing.ErrUnsupportedVersion):
			return fetchedSource{}, access.Fail(422, "catalog_unsupported_version",
				"The catalog is in a format this release cannot read.")
		case err != nil:
			return fetchedSource{}, access.Fail(422, "invalid_source_document", "The catalog is not a valid reference catalog.")
		}
	}
	if signed == nil {
		return fetchedSource{}, access.Fail(503, "reference_catalog_unavailable",
			"This process holds no verified reference catalog.")
	}
	if s.Catalog != nil && signed.Catalog.PublishedAt.Before(s.Catalog.Catalog.PublishedAt) {
		return fetchedSource{}, access.Fail(422, "catalog_outdated",
			"The catalog is older than the one this release ships.")
	}
	doc, skipped := catalogDocument(signed.Catalog, s.Normalizer)
	return s.validatedSource(doc, signed, skipped)
}

func (s *API) validatedSource(doc sourceDocument, signed *catalog.Signed, skipped []SkippedPrice) (fetchedSource, error) {
	normalized, currency, err := validatePrices(doc.Prices, s.Normalizer)
	if err != nil {
		return fetchedSource{}, err
	}
	doc.Prices = normalized
	if currency != doc.Currency {
		return fetchedSource{}, access.Fail(422, "invalid_source_document",
			"The pricing source currency does not match its prices.")
	}
	if skipped == nil {
		skipped = []SkippedPrice{}
	}
	return fetchedSource{Document: doc, Canonical: canonicalSourceDocument(doc), Catalog: signed, Skipped: skipped}, nil
}
