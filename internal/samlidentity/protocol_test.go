package samlidentity

import (
	"strings"
	"testing"
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
