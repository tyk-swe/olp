package samlidentity

import (
	"crypto/x509"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestXMLRejectsAmbiguousOrUnboundedStructures(t *testing.T) {
	for _, s := range []string{"", `<a/><b/>`, `<!DOCTYPE a [<!ENTITY x "value">]><a>&x;</a>`, `<a ID="x"><b Id="x"/></a>`, `<a x="1" x="2"/>`, `<a xmlns:p="urn:x" xmlns:q="urn:x" p:v="1" q:v="2"/>`, `<a><?work here?></a>`, strings.Repeat("<a>", 49) + strings.Repeat("</a>", 49), "<a>" + strings.Repeat("x", MaxXML) + "</a>"} {
		if BoundedXML([]byte(s)) == nil {
			t.Errorf("accepted ambiguous/bounded XML: %.80s", s)
		}
	}
	for _, s := range []string{`<a/>`, `<?xml version="1.0"?><a xmlns="urn:x" ID="one"><b ID="two">&amp;</b></a>`} {
		if err := BoundedXML([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
}

func FuzzMetadataNeverPanics(f *testing.F) {
	for _, s := range []string{"", `<EntityDescriptor/>`, `<a ID="x"><b ID="x"/></a>`} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = ParseMetadata(b) })
}

func TestOnlyCurrentCertificatesVouchForAssertions(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	retired := &x509.Certificate{NotBefore: now.AddDate(-2, 0, 0), NotAfter: now.AddDate(0, 0, -1)}
	current := &x509.Certificate{NotBefore: now.AddDate(-1, 0, 0), NotAfter: now.AddDate(1, 0, 0)}
	future := &x509.Certificate{NotBefore: now.AddDate(0, 0, 1), NotAfter: now.AddDate(2, 0, 0)}
	if got := currentCertificates([]*x509.Certificate{retired, current, future}, now); !slices.Equal(got, []*x509.Certificate{current}) {
		t.Fatalf("trusted %d certificates, want only the current one", len(got))
	}
	if got := currentCertificates([]*x509.Certificate{retired, future}, now); len(got) != 0 {
		t.Fatalf("trusted %d certificates outside their validity", len(got))
	}
}
