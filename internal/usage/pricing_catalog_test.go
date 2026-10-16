package usage

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/catalog"
	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/signing"
	"github.com/tyk-swe/olp/internal/vendors"
)

// TestEmbeddedCatalogMapsToValidPrices proves the catalog this release ships
// becomes a snapshot an operator can publish: every price valid, within the
// revision and snapshot bounds, and every alias its own row.
func TestEmbeddedCatalogMapsToValidPrices(t *testing.T) {
	signed, err := catalog.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	doc, skipped := catalogDocument(signed.Catalog, vendors.Kind)
	if len(skipped) != 0 {
		t.Fatalf("this build cannot store %d prices of its own catalog, first %+v", len(skipped), skipped[0])
	}
	normalized, currency, err := validatePrices(doc.Prices, vendors.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if currency != signed.Catalog.Currency || len(normalized) > MaxRevisionPrices {
		t.Fatalf("%d prices in %s", len(normalized), currency)
	}
	doc.Prices = normalized
	if size := len(canonicalSourceDocument(doc)); size > maxSourceDocumentBytes {
		t.Fatalf("the catalog snapshot is %d bytes, over the %d snapshot limit", size, maxSourceDocumentBytes)
	}
	names := 0
	for _, vendor := range signed.Catalog.Vendors {
		for _, model := range vendor.Models {
			names += len(model.Prices) * (1 + len(model.Aliases))
		}
	}
	if len(doc.Prices) != names {
		t.Fatalf("mapped %d prices for %d priced names", len(doc.Prices), names)
	}
}

// catalogUpstream serves a signed catalog and its signature the way a static
// file host does, with whatever media type the host chose.
type catalogUpstream struct {
	document, signature []byte
	contentType         string
}

func (u *catalogUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", u.contentType)
	switch r.URL.Path {
	case "/catalog.json":
		w.Write(u.document)
	case "/catalog.json.sig":
		if u.signature == nil {
			http.NotFound(w, r)
			return
		}
		w.Write(u.signature)
	default:
		http.NotFound(w, r)
	}
}

func signedCatalog(t *testing.T, published time.Time) ([]byte, []byte, signing.Keyring, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provenance := catalog.Provenance{SourceURL: "https://example.test/pricing", ObservedAt: published}
	three := catalog.Decimal("3")
	document, err := catalog.Encode(&catalog.Catalog{APIVersion: catalog.APIVersion, PublishedAt: published, Currency: "USD", Vendors: []catalog.Vendor{{ID: "anthropic", Name: "Anthropic", Models: []catalog.Model{{
		ID: "claude-test", InputModalities: []string{"text"}, OutputModalities: []string{"text"}, Provenance: provenance,
		Prices: []catalog.Price{{Operation: "generation", InputPerMillion: &three, OutputPerMillion: &three, Unrepresentable: []catalog.Unrepresentable{}, Provenance: provenance}},
	}}}}})
	if err != nil {
		t.Fatal(err)
	}
	signature, err := signing.Sign(nil, "test-release", private, document)
	if err != nil {
		t.Fatal(err)
	}
	return document, signature, signing.MustKeyring(signing.Key{ID: "test-release", Public: public}), private
}

func problemCode(err error) string {
	var problem *access.Problem
	if errors.As(err, &problem) {
		return problem.Code
	}
	return ""
}

// TestFetchedCatalogsVerifyBeforeAnyPriceIsRead covers source refresh: a
// tampered document, a tampered or missing signature and a catalog older
// than the one this release ships are refused; any media type is accepted,
// because the signature establishes what the document is.
func TestFetchedCatalogsVerifyBeforeAnyPriceIsRead(t *testing.T) {
	published := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	document, signature, keys, _ := signedCatalog(t, published)
	upstream := &catalogUpstream{document: document, signature: signature, contentType: "application/octet-stream"}
	server := httptest.NewServer(upstream)
	defer server.Close()
	policy := &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}
	s := &Server{Egress: policy, VendorKind: vendors.Kind, CatalogKeys: keys}
	address := server.URL + "/catalog.json"

	fetched, err := s.fetchCatalog(context.Background(), &address)
	if err != nil {
		t.Fatal(err)
	}
	if fetched.Catalog == nil || fetched.Catalog.KeyID != "test-release" || len(fetched.Document.Prices) != 1 || !fetched.Catalog.Catalog.PublishedAt.Equal(published) {
		t.Fatalf("fetched = %+v", fetched)
	}

	upstream.document = []byte(strings.Replace(string(document), `"3"`, `"0.3"`, 1))
	if _, err = s.fetchCatalog(context.Background(), &address); problemCode(err) != "catalog_signature_invalid" {
		t.Fatalf("tampered catalog: %v", err)
	}
	upstream.document = document

	var file signing.File
	_ = json.Unmarshal(signature, &file)
	file.Signatures[0].Signature[3] ^= 1
	upstream.signature, _ = json.Marshal(file)
	if _, err = s.fetchCatalog(context.Background(), &address); problemCode(err) != "catalog_signature_invalid" {
		t.Fatalf("tampered signature: %v", err)
	}

	upstream.signature = nil
	if _, err = s.fetchCatalog(context.Background(), &address); problemCode(err) != "source_fetch_failed" {
		t.Fatalf("missing signature: %v", err)
	}
	upstream.signature = signature

	newer, newerSignature, newerKeys, _ := signedCatalog(t, published.Add(time.Hour))
	bundled, err := catalog.Load(newer, newerSignature, newerKeys)
	if err != nil {
		t.Fatal(err)
	}
	s.Catalog = bundled
	if _, err = s.fetchCatalog(context.Background(), &address); problemCode(err) != "catalog_outdated" {
		t.Fatalf("outdated catalog: %v", err)
	}
	if fetched, err = s.fetchCatalog(context.Background(), nil); err != nil || fetched.Catalog.SHA256 != bundled.SHA256 {
		t.Fatalf("bundled catalog: %+v %v", fetched.Catalog, err)
	}
}
