// Package samlidentity implements OLP's bounded, solicited SAML POST profile.
// It never fetches assertion-controlled URLs or exposes verification errors.
package samlidentity

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

const MaxXML = 1 << 20
const assertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
const protocolNS = "urn:oasis:names:tc:SAML:2.0:protocol"
const metadataNS = "urn:oasis:names:tc:SAML:2.0:metadata"
const signatureNS = "http://www.w3.org/2000/09/xmldsig#"
const Skew = 90 * time.Second

var ErrDocument = errors.New("invalid SAML document")

// BoundedXML rejects directives, entities, duplicate IDs/attributes, excessive
// depth and ambiguous singleton structures before signature parsing.
func BoundedXML(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxXML {
		return ErrDocument
	}
	d := xml.NewDecoder(bytes.NewReader(raw))
	depth, nodes, roots := 0, 0, 0
	ids := map[string]bool{}
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrDocument
		}
		switch v := token.(type) {
		case xml.Directive:
			return ErrDocument
		case xml.ProcInst:
			if v.Target != "xml" || nodes != 0 {
				return ErrDocument
			}
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			nodes++
			if depth > 48 || nodes > 16384 || roots > 1 {
				return ErrDocument
			}
			attrs := map[xml.Name]bool{}
			for _, a := range v.Attr {
				if attrs[a.Name] {
					return ErrDocument
				}
				attrs[a.Name] = true
				if a.Name.Local == "ID" || a.Name.Local == "Id" || a.Name.Local == "id" {
					if a.Value == "" || ids[a.Value] {
						return ErrDocument
					}
					ids[a.Value] = true
				}
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && strings.TrimSpace(string(v)) != "" {
				return ErrDocument
			}
		}
	}
	if depth != 0 || roots != 1 {
		return ErrDocument
	}
	return nil
}

type Metadata struct {
	Entity       *saml.EntityDescriptor
	Certificates []*x509.Certificate
	RedirectURL  string
}

func ParseMetadata(raw []byte) (Metadata, error) {
	var out Metadata
	if BoundedXML(raw) != nil {
		return out, ErrDocument
	}
	var root struct{ XMLName xml.Name }
	if xml.Unmarshal(raw, &root) != nil || root.XMLName.Space != metadataNS || root.XMLName.Local != "EntityDescriptor" {
		return out, ErrDocument
	}
	var m saml.EntityDescriptor
	if xml.Unmarshal(raw, &m) != nil || m.EntityID == "" || len(m.EntityID) > 2048 || len(m.IDPSSODescriptors) != 1 {
		return out, ErrDocument
	}
	role := m.IDPSSODescriptors[0]
	if !strings.Contains(role.ProtocolSupportEnumeration, protocolNS) {
		return out, ErrDocument
	}
	for _, ep := range role.SingleSignOnServices {
		if ep.Binding == saml.HTTPRedirectBinding {
			if out.RedirectURL != "" && out.RedirectURL != ep.Location {
				return out, ErrDocument
			}
			out.RedirectURL = ep.Location
		}
	}
	for _, kd := range role.KeyDescriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, v := range kd.KeyInfo.X509Data.X509Certificates {
			b, e := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(v.Data), ""))
			if e != nil {
				return out, ErrDocument
			}
			c, e := x509.ParseCertificate(b)
			if e != nil {
				return out, ErrDocument
			}
			switch k := c.PublicKey.(type) {
			case *rsa.PublicKey:
				if k.N.BitLen() < 2048 || k.N.BitLen() > 8192 {
					return out, ErrDocument
				}
			case *ecdsa.PublicKey:
				if k.Curve.Params().BitSize < 256 {
					return out, ErrDocument
				}
			default:
				return out, ErrDocument
			}
			out.Certificates = append(out.Certificates, c)
		}
	}
	if out.RedirectURL == "" || len(out.Certificates) == 0 || len(out.Certificates) > 8 {
		return out, ErrDocument
	}
	out.Entity = &m
	return out, nil
}

func child(e *etree.Element, ns, name string) []*etree.Element {
	var result []*etree.Element
	for _, v := range e.ChildElements() {
		if v.Tag == name && v.NamespaceURI() == ns {
			result = append(result, v)
		}
	}
	return result
}

func strongSignatures(e *etree.Element) bool {
	for _, n := range e.ChildElements() {
		if n.NamespaceURI() == signatureNS {
			switch n.Tag {
			case "SignatureMethod":
				switch n.SelectAttrValue("Algorithm", "") {
				case "http://www.w3.org/2001/04/xmldsig-more#rsa-sha256", "http://www.w3.org/2001/04/xmldsig-more#rsa-sha384", "http://www.w3.org/2001/04/xmldsig-more#rsa-sha512", "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha256", "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha384", "http://www.w3.org/2001/04/xmldsig-more#ecdsa-sha512":
				default:
					return false
				}
			case "DigestMethod":
				a := n.SelectAttrValue("Algorithm", "")
				if a != "http://www.w3.org/2001/04/xmlenc#sha256" && a != "http://www.w3.org/2001/04/xmlenc#sha512" && a != "http://www.w3.org/2001/04/xmldsig-more#sha384" {
					return false
				}
			}
		}
		if !strongSignatures(n) {
			return false
		}
	}
	return true
}

// Verify requires exactly one separately signed, unencrypted assertion. Outer
// response signatures are verified too; they never substitute for this proof.
func Verify(sp *saml.ServiceProvider, metadata Metadata, raw []byte, requestID string, now time.Time) (*saml.Assertion, error) {
	if BoundedXML(raw) != nil {
		return nil, ErrDocument
	}
	doc := etree.NewDocument()
	if doc.ReadFromBytes(raw) != nil {
		return nil, ErrDocument
	}
	root := doc.Root()
	if root.Tag != "Response" || root.NamespaceURI() != protocolNS || root.SelectAttrValue("Version", "") != "2.0" || root.SelectAttrValue("Destination", "") != sp.AcsURL.String() || !strongSignatures(root) {
		return nil, ErrDocument
	}
	assertions := child(root, assertionNS, "Assertion")
	if len(assertions) != 1 || len(child(root, assertionNS, "EncryptedAssertion")) != 0 {
		return nil, ErrDocument
	}
	assertion := assertions[0]
	for _, name := range []string{"Issuer", "Subject", "Conditions", "AuthnStatement", "Signature"} {
		ns := assertionNS
		if name == "Signature" {
			ns = signatureNS
		}
		if len(child(assertion, ns, name)) != 1 {
			return nil, ErrDocument
		}
	}
	ctx := dsig.NewDefaultValidationContext(&dsig.MemoryX509CertificateStore{Roots: metadata.Certificates})
	ctx.IdAttribute = "ID"
	if _, err := ctx.Validate(assertion); err != nil {
		return nil, ErrDocument
	}
	var preview saml.Response
	if xml.Unmarshal(raw, &preview) != nil || preview.Assertion == nil || preview.Assertion.Subject == nil || preview.Assertion.Conditions == nil || preview.IssueInstant.After(now.Add(Skew)) {
		return nil, ErrDocument
	}
	for _, confirmation := range preview.Assertion.Subject.SubjectConfirmations {
		if confirmation.SubjectConfirmationData == nil {
			return nil, ErrDocument
		}
	}
	a, err := sp.ParseXMLResponse(raw, []string{requestID}, sp.AcsURL)
	if err != nil {
		return nil, ErrDocument
	}
	if a.ID == "" || len(a.ID) > 256 || a.Version != "2.0" || a.Subject == nil || a.Subject.NameID == nil || a.Conditions == nil || a.IssueInstant.IsZero() || a.IssueInstant.After(now.Add(Skew)) {
		return nil, ErrDocument
	}
	if len(a.AuthnStatements) != 1 || a.AuthnStatements[0].AuthnInstant.IsZero() || a.AuthnStatements[0].AuthnInstant.After(now.Add(Skew)) {
		return nil, ErrDocument
	}
	if end := a.AuthnStatements[0].SessionNotOnOrAfter; end != nil && !now.Before(end.Add(Skew)) {
		return nil, ErrDocument
	}
	n := a.Subject.NameID
	if n.NameQualifier != "" && n.NameQualifier != metadata.Entity.EntityID || n.SPNameQualifier != "" && n.SPNameQualifier != sp.EntityID {
		return nil, ErrDocument
	}
	if n.Value == "" || len(n.Value) > 1024 || strings.ContainsAny(n.Value, "\x00\r\n") || n.Format == string(saml.TransientNameIDFormat) || n.NameQualifier != "" && n.NameQualifier != metadata.Entity.EntityID || n.SPNameQualifier != "" && n.SPNameQualifier != sp.EntityID {
		return nil, ErrDocument
	}
	c := a.Conditions
	if c.NotBefore.IsZero() || c.NotOnOrAfter.IsZero() || !c.NotOnOrAfter.After(c.NotBefore) || c.NotOnOrAfter.Sub(a.IssueInstant) > 10*time.Minute || now.Before(c.NotBefore.Add(-Skew)) || !now.Before(c.NotOnOrAfter.Add(Skew)) || len(c.AudienceRestrictions) == 0 {
		return nil, ErrDocument
	}
	for _, restriction := range c.AudienceRestrictions {
		if restriction.Audience.Value != sp.EntityID {
			return nil, ErrDocument
		}
	}
	if len(a.Subject.SubjectConfirmations) != 1 {
		return nil, ErrDocument
	}
	confirmation := a.Subject.SubjectConfirmations[0]
	data := confirmation.SubjectConfirmationData
	if confirmation.Method != "urn:oasis:names:tc:SAML:2.0:cm:bearer" || data == nil || data.InResponseTo != requestID || data.Recipient != sp.AcsURL.String() || data.NotOnOrAfter.IsZero() || !data.NotBefore.IsZero() && now.Before(data.NotBefore.Add(-Skew)) || !now.Before(data.NotOnOrAfter.Add(Skew)) {
		return nil, ErrDocument
	}
	return a, nil
}

func Attributes(a *saml.Assertion) (map[string][]string, error) {
	out := map[string][]string{}
	for _, statement := range a.AttributeStatements {
		for _, attribute := range statement.Attributes {
			if attribute.Name == "" || len(attribute.Name) > 256 || len(out) >= 128 || len(attribute.Values) > 256 {
				return nil, ErrDocument
			}
			if _, ok := out[attribute.Name]; ok {
				return nil, ErrDocument
			}
			values := []string{}
			for _, v := range attribute.Values {
				if v.NameID != nil || len(v.Value) > 2048 || strings.ContainsRune(v.Value, '\x00') {
					return nil, ErrDocument
				}
				values = append(values, v.Value)
			}
			out[attribute.Name] = values
		}
	}
	return out, nil
}
