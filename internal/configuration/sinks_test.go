package configuration

import (
	"net/netip"
	"testing"

	"github.com/tyk-swe/olp/internal/egress"
	"github.com/tyk-swe/olp/internal/secretstore"
)

func TestSinkPromotionUsesUnambiguousLogicalBindingsAndRefusesExternalValues(t *testing.T) {
	projectA, projectB := "A/B", "A"
	a := SinkEntry{Name: "C", Project: &projectA, Type: "https", Destination: "http://127.0.0.1/export", Streams: []string{"requests"}, Enabled: true}
	b := a
	b.Name = "B/C"
	b.Project = &projectB
	if sinkReference(a) == sinkReference(b) {
		t.Fatal("scope/name tuples collided")
	}
	ref := sinkReference(a)
	a.CredentialRef = &ref
	doc := &Document{Sinks: []SinkEntry{a, b}}
	s := &Server{Egress: &egress.Policy{AllowedNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, PlainHTTPHosts: []string{"127.0.0.1"}}}
	if err := s.validateSinks(doc); err != nil {
		t.Fatal(err)
	}
	if err := validateBindings(doc, plainBindings(map[string]string{ref: "signing-material"})); err != nil {
		t.Fatal(err)
	}
	if err := validateBindings(doc, plainBindings(map[string]string{ref: ""})); err == nil {
		t.Fatal("accepted empty signing credential")
	}
	if err := validateBindings(doc, bindingSet{ref: {reference: &secretstore.Reference{Store: "vault"}}}); err == nil {
		t.Fatal("accepted external store binding for HMAC material")
	}
	doc.Sinks = append(doc.Sinks, a)
	if s.validateSinks(doc) == nil {
		t.Fatal("accepted duplicate scoped name")
	}
}
