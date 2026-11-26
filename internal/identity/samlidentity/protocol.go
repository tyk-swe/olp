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
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	MaxXML      = 1 << 20
	assertionNS = "urn:oasis:names:tc:SAML:2.0:assertion"
	protocolNS  = "urn:oasis:names:tc:SAML:2.0:protocol"
	metadataNS  = "urn:oasis:names:tc:SAML:2.0:metadata"
	signatureNS = "http://www.w3.org/2000/09/xmldsig#"
	Skew        = 90 * time.Second
)

var ErrDocument = errors.New("invalid SAML document")

type xmlBoundsState struct {
	depth int
	nodes int
	roots int
	ids   map[string]bool
}

func (s *xmlBoundsState) acceptStart(el xml.StartElement) error {
	if s.depth == 0 {
		s.roots++
	}
	s.depth++
	s.nodes++
	if s.depth > 48 || s.nodes > 16384 || s.roots > 1 {
		return ErrDocument
	}
	attrs := make(map[xml.Name]bool, len(el.Attr))
	for _, a := range el.Attr {
		if attrs[a.Name] {
			return ErrDocument
		}
		attrs[a.Name] = true
		if a.Name.Local == "ID" || a.Name.Local == "Id" || a.Name.Local == "id" {
			if a.Value == "" || s.ids[a.Value] {
				return ErrDocument
			}
			s.ids[a.Value] = true
		}
	}
	return nil
}

func (s *xmlBoundsState) accept(token xml.Token) error {
	switch v := token.(type) {
	case xml.Directive:
		return ErrDocument
	case xml.ProcInst:
		if v.Target != "xml" || s.nodes != 0 {
			return ErrDocument
		}
	case xml.StartElement:
		return s.acceptStart(v)
	case xml.EndElement:
		s.depth--
	case xml.CharData:
		if s.depth == 0 && strings.TrimSpace(string(v)) != "" {
			return ErrDocument
		}
	}
	return nil
}

// BoundedXML rejects directives, entities, duplicate IDs/attributes, excessive
// depth and ambiguous singleton structures before signature parsing.
func BoundedXML(raw []byte) error {
	if len(raw) == 0 || len(raw) > MaxXML {
		return ErrDocument
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	state := xmlBoundsState{ids: map[string]bool{}}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ErrDocument
		}
		if err := state.accept(token); err != nil {
			return err
		}
	}
	if state.depth != 0 || state.roots != 1 {
		return ErrDocument
	}
	return nil
}

type Metadata struct {
	Entity       *saml.EntityDescriptor
	Certificates []*x509.Certificate
	RedirectURL  string
}

func parseRedirectSSOURL(services []saml.Endpoint) (string, error) {
	redirectURL := ""
	for _, ep := range services {
		if ep.Binding != saml.HTTPRedirectBinding {
			continue
		}
		if redirectURL != "" && redirectURL != ep.Location {
			return "", ErrDocument
		}
		redirectURL = ep.Location
	}
	if redirectURL == "" {
		return "", ErrDocument
	}
	return redirectURL, nil
}

func parseSigningCertificate(encoded string) (*x509.Certificate, error) {
	der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		return nil, ErrDocument
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, ErrDocument
	}
	switch k := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if k.N.BitLen() < 2048 || k.N.BitLen() > 8192 {
			return nil, ErrDocument
		}
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return nil, ErrDocument
		}
	default:
		return nil, ErrDocument
	}
	return cert, nil
}

func parseSigningCertificates(descriptors []saml.KeyDescriptor) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for _, kd := range descriptors {
		if kd.Use != "" && kd.Use != "signing" {
			continue
		}
		for _, v := range kd.KeyInfo.X509Data.X509Certificates {
			cert, err := parseSigningCertificate(v.Data)
			if err != nil {
				return nil, ErrDocument
			}
			certs = append(certs, cert)
		}
	}
	if len(certs) == 0 || len(certs) > 8 {
		return nil, ErrDocument
	}
	return certs, nil
}

func ParseMetadata(raw []byte) (Metadata, error) {
	if BoundedXML(raw) != nil {
		return Metadata{}, ErrDocument
	}
	var root struct{ XMLName xml.Name }
	if xml.Unmarshal(raw, &root) != nil || root.XMLName.Space != metadataNS || root.XMLName.Local != "EntityDescriptor" {
		return Metadata{}, ErrDocument
	}
	var m saml.EntityDescriptor
	if xml.Unmarshal(raw, &m) != nil || m.EntityID == "" || len(m.EntityID) > 2048 || len(m.IDPSSODescriptors) != 1 {
		return Metadata{}, ErrDocument
	}
	role := m.IDPSSODescriptors[0]
	if !strings.Contains(role.ProtocolSupportEnumeration, protocolNS) {
		return Metadata{}, ErrDocument
	}
	redirectURL, err := parseRedirectSSOURL(role.SingleSignOnServices)
	if err != nil {
		return Metadata{}, err
	}
	certs, err := parseSigningCertificates(role.KeyDescriptors)
	if err != nil {
		return Metadata{}, err
	}
	return Metadata{Entity: &m, Certificates: certs, RedirectURL: redirectURL}, nil
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
	inSignature := e.NamespaceURI() == signatureNS
	for _, n := range e.ChildElements() {
		if inSignature && n.NamespaceURI() == signatureNS {
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

func docError(reason string) error {
	return fmt.Errorf("%w: %s", ErrDocument, reason)
}

func unmarshalElement(el *etree.Element, target any) error {
	doc := etree.NewDocument()
	doc.SetRoot(el.Copy())
	raw, err := doc.WriteToBytes()
	if err != nil {
		return err
	}
	return xml.Unmarshal(raw, target)
}

func validateResponseEnvelope(root *etree.Element, acsURL string) (*etree.Element, error) {
	if root == nil || root.Tag != "Response" || root.NamespaceURI() != protocolNS {
		return nil, docError("root element must be a SAML 2.0 Response")
	}
	if root.SelectAttrValue("Version", "") != "2.0" {
		return nil, docError("unsupported response version")
	}
	if root.SelectAttrValue("Destination", "") != acsURL {
		return nil, docError("response destination mismatch")
	}
	if !strongSignatures(root) {
		return nil, docError("weak signature or digest algorithm")
	}
	if len(child(root, signatureNS, "Signature")) > 1 {
		return nil, docError("multiple response signatures")
	}
	if len(child(root, assertionNS, "EncryptedAssertion")) != 0 {
		return nil, docError("encrypted assertions are not supported")
	}
	assertions := child(root, assertionNS, "Assertion")
	if len(assertions) != 1 {
		return nil, docError("response must carry exactly one assertion")
	}
	return assertions[0], nil
}

func validateAssertionShape(assertion *etree.Element) error {
	if len(child(assertion, signatureNS, "Signature")) != 1 {
		return docError("assertion must carry exactly one signature")
	}
	for _, name := range []string{"Issuer", "Subject", "Conditions", "AuthnStatement"} {
		if len(child(assertion, assertionNS, name)) != 1 {
			return docError("assertion missing singleton " + name)
		}
	}
	subjectEl := child(assertion, assertionNS, "Subject")[0]
	var nameIDs []*etree.Element
	for _, elem := range subjectEl.ChildElements() {
		if elem.Tag == "NameID" {
			nameIDs = append(nameIDs, elem)
		}
	}
	if len(nameIDs) != 1 || nameIDs[0].NamespaceURI() != assertionNS {
		return docError("subject must carry one SAML NameID")
	}
	return nil
}

func validateResponseHeader(root *etree.Element, sp *saml.ServiceProvider, metadata Metadata, requestID string, now time.Time) error {
	var resp saml.Response
	if err := unmarshalElement(root, &resp); err != nil {
		return docError("malformed response XML")
	}
	if resp.Version != "2.0" || resp.Destination != sp.AcsURL.String() {
		return docError("response version or destination mismatch")
	}
	if resp.InResponseTo != requestID {
		return docError("response InResponseTo mismatch")
	}
	if resp.IssueInstant.IsZero() || resp.IssueInstant.After(now.Add(Skew)) || now.After(resp.IssueInstant.Add(Skew)) {
		return docError("response IssueInstant outside allowed skew")
	}
	if resp.Issuer != nil && resp.Issuer.Value != metadata.Entity.EntityID {
		return docError("response issuer mismatch")
	}
	if resp.Status.StatusCode.Value != saml.StatusSuccess {
		return docError("response status is not success")
	}
	return nil
}

func validateAssertionCore(a *saml.Assertion, metadata Metadata, now time.Time) error {
	if a.ID == "" || len(a.ID) > 256 || a.Version != "2.0" {
		return docError("invalid assertion ID or version")
	}
	if a.Issuer.Value != metadata.Entity.EntityID {
		return docError("assertion issuer mismatch")
	}
	if a.IssueInstant.IsZero() || a.IssueInstant.After(now.Add(Skew)) {
		return docError("invalid assertion IssueInstant")
	}
	if len(a.AuthnStatements) != 1 {
		return docError("assertion must carry one AuthnStatement")
	}
	stmt := a.AuthnStatements[0]
	if stmt.AuthnInstant.IsZero() || stmt.AuthnInstant.After(now.Add(Skew)) {
		return docError("invalid AuthnInstant")
	}
	if stmt.SessionNotOnOrAfter != nil && !now.Before(stmt.SessionNotOnOrAfter.Add(Skew)) {
		return docError("session expired")
	}
	return nil
}

func validateNameID(n *saml.NameID, spEntityID, idpEntityID string) error {
	if n == nil {
		return docError("missing subject or NameID")
	}
	if n.Value == "" || len(n.Value) > 1024 || strings.ContainsAny(n.Value, "\x00\r\n") || n.Format == string(saml.TransientNameIDFormat) {
		return docError("invalid NameID")
	}
	if n.NameQualifier != "" && n.NameQualifier != idpEntityID {
		return docError("NameQualifier mismatch")
	}
	if n.SPNameQualifier != "" && n.SPNameQualifier != spEntityID {
		return docError("SPNameQualifier mismatch")
	}
	return nil
}

func validateSubjectConfirmation(confirmations []saml.SubjectConfirmation, acsURL, requestID string, now time.Time) error {
	if len(confirmations) != 1 {
		return docError("subject must carry one SubjectConfirmation")
	}
	confirmation := confirmations[0]
	data := confirmation.SubjectConfirmationData
	if confirmation.Method != "urn:oasis:names:tc:SAML:2.0:cm:bearer" || data == nil {
		return docError("missing bearer SubjectConfirmationData")
	}
	if data.InResponseTo != requestID || data.Recipient != acsURL {
		return docError("SubjectConfirmationData InResponseTo or Recipient mismatch")
	}
	if data.NotOnOrAfter.IsZero() || !now.Before(data.NotOnOrAfter.Add(Skew)) {
		return docError("SubjectConfirmationData expired")
	}
	if !data.NotBefore.IsZero() && now.Before(data.NotBefore.Add(-Skew)) {
		return docError("SubjectConfirmationData not yet valid")
	}
	return nil
}

func validateSubject(a *saml.Assertion, sp *saml.ServiceProvider, metadata Metadata, requestID string, now time.Time) error {
	if a.Subject == nil {
		return docError("missing subject or NameID")
	}
	if err := validateNameID(a.Subject.NameID, sp.EntityID, metadata.Entity.EntityID); err != nil {
		return err
	}
	return validateSubjectConfirmation(a.Subject.SubjectConfirmations, sp.AcsURL.String(), requestID, now)
}

func validateConditions(a *saml.Assertion, sp *saml.ServiceProvider, now time.Time) error {
	c := a.Conditions
	if c == nil || c.NotBefore.IsZero() || c.NotOnOrAfter.IsZero() || !c.NotOnOrAfter.After(c.NotBefore) {
		return docError("invalid assertion conditions interval")
	}
	if c.NotOnOrAfter.Sub(a.IssueInstant) > 10*time.Minute {
		return docError("assertion lifetime exceeds 10 minutes")
	}
	if now.Before(c.NotBefore.Add(-Skew)) || !now.Before(c.NotOnOrAfter.Add(Skew)) {
		return docError("assertion outside validity window")
	}
	if len(c.AudienceRestrictions) == 0 {
		return docError("missing audience restriction")
	}
	for _, restriction := range c.AudienceRestrictions {
		if restriction.Audience.Value != sp.EntityID {
			return docError("audience mismatch")
		}
	}
	return nil
}

// Verify requires exactly one separately signed, unencrypted assertion. Outer
// response signatures are verified too; they never substitute for this proof.
func Verify(sp *saml.ServiceProvider, metadata Metadata, raw []byte, requestID string, now time.Time) (*saml.Assertion, error) {
	if BoundedXML(raw) != nil {
		return nil, docError("XML bounds check failed")
	}
	doc := etree.NewDocument()
	if doc.ReadFromBytes(raw) != nil {
		return nil, docError("XML parse failed")
	}
	assertionEl, err := validateResponseEnvelope(doc.Root(), sp.AcsURL.String())
	if err != nil {
		return nil, err
	}
	if err := validateAssertionShape(assertionEl); err != nil {
		return nil, err
	}
	if err := validateResponseHeader(doc.Root(), sp, metadata, requestID, now); err != nil {
		return nil, err
	}
	verifier := *sp
	verifier.Clock = dsig.NewFakeClockAt(now)
	verifier.IDPMetadata = metadata.Entity
	a, err := verifier.ParseXMLResponse(raw, []string{requestID}, verifier.AcsURL)
	if err != nil {
		return nil, docError("response or assertion rejected by SAML verifier")
	}
	if err := validateAssertionCore(a, metadata, now); err != nil {
		return nil, err
	}
	if err := validateSubject(a, sp, metadata, requestID, now); err != nil {
		return nil, err
	}
	if err := validateConditions(a, sp, now); err != nil {
		return nil, err
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
