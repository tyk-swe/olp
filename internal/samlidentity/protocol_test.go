package samlidentity

import (
	"bytes"
	"cmp"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	dsig "github.com/russellhaering/goxmldsig"
)

const (
	providerEntity = "https://idp.example/metadata"
	providerSignOn = "https://idp.example/sso"
	serviceEntity  = "https://olp.example/api/v1/saml/metadata"
	serviceConsume = "https://olp.example/api/v1/saml/acs"
	requestID      = "id-request-1"
	subject        = "ada@example.com"
)

var instant = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

type signer struct {
	key         crypto.Signer
	certificate *x509.Certificate
}

func certify(t *testing.T, public crypto.PublicKey, issuer crypto.Signer, from, until time.Time) *x509.Certificate {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "identity provider"}, NotBefore: from, NotAfter: until}
	encoded, err := x509.CreateCertificate(rand.Reader, template, template, public, issuer)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(encoded)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func newSigner(t *testing.T, from, until time.Time) *signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key: key, certificate: certify(t, key.Public(), key, from, until)}
}

func (s *signer) sign(t *testing.T, element *etree.Element, algorithm string) *etree.Element {
	t.Helper()
	if s == nil {
		return nil
	}
	context, err := dsig.NewSigningContext(s.key, [][]byte{s.certificate.Raw})
	if err != nil {
		t.Fatal(err)
	}
	context.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err := context.SetSignatureMethod(cmp.Or(algorithm, dsig.ECDSASHA256SignatureMethod)); err != nil {
		t.Fatal(err)
	}
	signed, err := context.SignEnveloped(element)
	if err != nil {
		t.Fatal(err)
	}
	children := signed.ChildElements()
	return children[len(children)-1]
}

func keyDescriptor(use string, certificate *x509.Certificate) string {
	return `<KeyDescriptor use="` + use + `"><KeyInfo xmlns="` + signatureNS + `"><X509Data><X509Certificate>` +
		base64.StdEncoding.EncodeToString(certificate.Raw) + `</X509Certificate></X509Data></KeyInfo></KeyDescriptor>`
}

func signOn(binding, location string) string {
	return `<SingleSignOnService Binding="` + binding + `" Location="` + location + `"/>`
}

func descriptor(children ...string) string {
	return `<EntityDescriptor xmlns="` + metadataNS + `" entityID="` + providerEntity + `"><IDPSSODescriptor protocolSupportEnumeration="` + protocolNS + `">` +
		strings.Join(children, "") + `</IDPSSODescriptor></EntityDescriptor>`
}

func trust(t *testing.T, keys string) (*saml.ServiceProvider, Metadata) {
	t.Helper()
	metadata, err := ParseMetadata([]byte(descriptor(keys, signOn(saml.HTTPRedirectBinding, providerSignOn))))
	if err != nil {
		t.Fatal(err)
	}
	consume, err := url.Parse(serviceConsume)
	if err != nil {
		t.Fatal(err)
	}
	return &saml.ServiceProvider{EntityID: serviceEntity, AcsURL: *consume, IDPMetadata: metadata.Entity, Clock: dsig.NewFakeClockAt(instant)}, metadata
}

type issue struct {
	assertionID, destination, audience, recipient, inResponseTo, nameIDFormat, algorithm string
	age, lifetime                                                                        time.Duration
}

func respond(t *testing.T, assertionSigner, responseSigner *signer, i issue) []byte {
	t.Helper()
	issued := instant.Add(-i.age)
	expires := issued.Add(cmp.Or(i.lifetime, 5*time.Minute))
	assert := func(signature *etree.Element) *saml.Assertion {
		return &saml.Assertion{
			ID: cmp.Or(i.assertionID, "id-assertion-1"), IssueInstant: issued, Version: "2.0",
			Issuer:    saml.Issuer{Value: providerEntity},
			Signature: signature,
			Subject: &saml.Subject{
				NameID: &saml.NameID{Format: cmp.Or(i.nameIDFormat, string(saml.PersistentNameIDFormat)), Value: subject},
				SubjectConfirmations: []saml.SubjectConfirmation{{
					Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer",
					SubjectConfirmationData: &saml.SubjectConfirmationData{
						InResponseTo: cmp.Or(i.inResponseTo, requestID), NotOnOrAfter: expires, Recipient: cmp.Or(i.recipient, serviceConsume),
					},
				}},
			},
			Conditions: &saml.Conditions{
				NotBefore: issued, NotOnOrAfter: expires,
				AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: cmp.Or(i.audience, serviceEntity)}}},
			},
			AuthnStatements: []saml.AuthnStatement{{
				AuthnInstant: issued, SessionIndex: "session-1",
				AuthnContext: saml.AuthnContext{AuthnContextClassRef: &saml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}},
			}},
		}
	}
	assertion := assert(assertionSigner.sign(t, assert(nil).Element(), i.algorithm))
	response := func(signature *etree.Element) *etree.Element {
		return (&saml.Response{
			ID: "id-response-1", InResponseTo: cmp.Or(i.inResponseTo, requestID), Version: "2.0", IssueInstant: issued,
			Destination: cmp.Or(i.destination, serviceConsume), Issuer: &saml.Issuer{Value: providerEntity},
			Status:    saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}},
			Signature: signature, Assertion: assertion,
		}).Element()
	}
	document := etree.NewDocument()
	document.SetRoot(response(responseSigner.sign(t, response(nil), i.algorithm)))
	raw, err := document.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertionIn(t *testing.T, response []byte) []byte {
	t.Helper()
	const opening, closing = "<saml:Assertion", "</saml:Assertion>"
	start, end := bytes.Index(response, []byte(opening)), bytes.LastIndex(response, []byte(closing))
	if start < 0 || end < start {
		t.Fatalf("no assertion in %s", response)
	}
	return response[start : end+len(closing)]
}

func TestVerifyAcceptsOnlyTheSolicitedSignedAssertion(t *testing.T) {
	provider := newSigner(t, instant.Add(-time.Hour), instant.Add(time.Hour))
	retired := newSigner(t, instant.Add(-48*time.Hour), instant.Add(-24*time.Hour))
	stranger := newSigner(t, instant.Add(-time.Hour), instant.Add(time.Hour))
	service, metadata := trust(t, keyDescriptor("signing", retired.certificate)+keyDescriptor("signing", provider.certificate))

	solicited := respond(t, provider, nil, issue{})
	assertion, err := Verify(service, metadata, solicited, requestID, instant)
	if err != nil {
		t.Fatalf("the solicited response was refused: %v", err)
	}
	if assertion.Subject.NameID.Value != subject {
		t.Errorf("subject %q, want %q", assertion.Subject.NameID.Value, subject)
	}
	if _, err := Verify(service, metadata, respond(t, provider, provider, issue{}), requestID, instant); err != nil {
		t.Errorf("a response signed as a whole and per assertion was refused: %v", err)
	}

	forged := assertionIn(t, respond(t, nil, nil, issue{assertionID: "id-assertion-2"}))
	for name, raw := range map[string][]byte{
		"an unsigned assertion":                        respond(t, nil, nil, issue{}),
		"a signature on the response only":             respond(t, nil, provider, issue{}),
		"a signature from an unregistered key":         respond(t, stranger, nil, issue{}),
		"a signature from a retired certificate":       respond(t, retired, nil, issue{}),
		"a SHA-1 signature":                            respond(t, provider, nil, issue{algorithm: dsig.ECDSASHA1SignatureMethod}),
		"a subject changed after signing":              bytes.Replace(solicited, []byte(subject), []byte("eve@example.com"), 1),
		"a forged assertion beside the signed one":     bytes.Replace(solicited, []byte("<saml:Assertion"), slices.Concat(forged, []byte("<saml:Assertion")), 1),
		"a response addressed to another service":      respond(t, provider, nil, issue{destination: "https://other.example/acs"}),
		"an assertion for another audience":            respond(t, provider, nil, issue{audience: "https://other.example/metadata"}),
		"an assertion for another recipient":           respond(t, provider, nil, issue{recipient: "https://other.example/acs"}),
		"an answer to another request":                 respond(t, provider, nil, issue{inResponseTo: "id-request-2"}),
		"a transient subject":                          respond(t, provider, nil, issue{nameIDFormat: string(saml.TransientNameIDFormat)}),
		"an expired assertion":                         respond(t, provider, nil, issue{age: 10 * time.Minute}),
		"an assertion from the future":                 respond(t, provider, nil, issue{age: -10 * time.Minute}),
		"an assertion valid for more than ten minutes": respond(t, provider, nil, issue{lifetime: 11 * time.Minute}),
	} {
		if _, err := Verify(service, metadata, raw, requestID, instant); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: got %v, want %v", name, err, ErrDocument)
		}
	}
}

func TestParseMetadataKeepsOnlyUsableSigningKeys(t *testing.T) {
	provider := newSigner(t, instant.Add(-time.Hour), instant.Add(time.Hour))
	signing := keyDescriptor("signing", provider.certificate)
	redirect := signOn(saml.HTTPRedirectBinding, providerSignOn)

	metadata, err := ParseMetadata([]byte(descriptor(signing, keyDescriptor("encryption", provider.certificate), signOn(saml.HTTPPostBinding, "https://idp.example/post"), redirect)))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.RedirectURL != providerSignOn || metadata.Entity.EntityID != providerEntity {
		t.Errorf("redirect %q for entity %q", metadata.RedirectURL, metadata.Entity.EntityID)
	}
	if len(metadata.Certificates) != 1 || !metadata.Certificates[0].Equal(provider.certificate) {
		t.Errorf("kept %d certificates, want only the signing one", len(metadata.Certificates))
	}

	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	weakCertificate := certify(t, weak.Public(), provider.key, instant.Add(-time.Hour), instant.Add(time.Hour))
	for name, raw := range map[string]string{
		"no signing key":              descriptor(keyDescriptor("encryption", provider.certificate), redirect),
		"a 1024 bit RSA key":          descriptor(keyDescriptor("signing", weakCertificate), redirect),
		"more than eight keys":        descriptor(strings.Repeat(signing, 9), redirect),
		"an unreadable certificate":   descriptor(strings.Replace(signing, "<X509Certificate>", "<X509Certificate>AAAA", 1), redirect),
		"no redirect endpoint":        descriptor(signing, signOn(saml.HTTPPostBinding, providerSignOn)),
		"two redirect endpoints":      descriptor(signing, redirect, signOn(saml.HTTPRedirectBinding, "https://other.example/sso")),
		"a role for another protocol": strings.Replace(descriptor(signing, redirect), protocolNS, "urn:oasis:names:tc:SAML:1.1:protocol", 1),
		"a federation document":       `<EntitiesDescriptor xmlns="` + metadataNS + `">` + descriptor(signing, redirect) + `</EntitiesDescriptor>`,
		"a document type declaration": `<!DOCTYPE EntityDescriptor>` + descriptor(signing, redirect),
	} {
		if _, err := ParseMetadata([]byte(raw)); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: got %v, want %v", name, err, ErrDocument)
		}
	}
}

func TestBoundedXMLRefusesAmbiguousDocuments(t *testing.T) {
	nested := func(depth int) string {
		return strings.Repeat("<a>", depth) + strings.Repeat("</a>", depth)
	}
	for raw, accepted := range map[string]bool{
		`<?xml version="1.0"?><a ID="one"><b ID="two"/></a>`: true,
		nested(48):                        true,
		nested(49):                        false,
		`<a ID="one"><b ID="one"/></a>`:   false,
		`<a ID="one"><b Id="one"/></a>`:   false,
		`<a ID=""/>`:                      false,
		`<a b="1" b="2"/>`:                false,
		`<a/><b/>`:                        false,
		`<a/>trailing`:                    false,
		`<a/><?xml-stylesheet href="x"?>`: false,
		`<!DOCTYPE a [<!ENTITY b "c">]><a>&b;</a>`:     false,
		`<a>` + strings.Repeat("b", MaxXML) + `</a>`:   false,
		`<a>` + strings.Repeat("<b/>", 16384) + `</a>`: false,
		`<a>`: false,
		``:    false,
	} {
		if err := BoundedXML([]byte(raw)); (err == nil) != accepted {
			t.Errorf("%.48s: accepted=%v, want %v", raw, err == nil, accepted)
		}
	}
}

func TestAttributesRefusesAmbiguousStatements(t *testing.T) {
	statement := func(attributes ...saml.Attribute) *saml.Assertion {
		return &saml.Assertion{AttributeStatements: []saml.AttributeStatement{{Attributes: attributes}}}
	}
	values := func(name string, values ...saml.AttributeValue) saml.Attribute {
		return saml.Attribute{Name: name, Values: values}
	}
	got, err := Attributes(statement(values("groups", saml.AttributeValue{Value: "admins"}, saml.AttributeValue{Value: "billing"}), values("empty")))
	if want := map[string][]string{"groups": {"admins", "billing"}, "empty": {}}; err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, %v, want %v", got, err, want)
	}
	for name, assertion := range map[string]*saml.Assertion{
		"a repeated attribute":    statement(values("groups", saml.AttributeValue{Value: "viewers"}), values("groups", saml.AttributeValue{Value: "admins"})),
		"an unnamed attribute":    statement(values("", saml.AttributeValue{Value: "admins"})),
		"a NUL inside a value":    statement(values("groups", saml.AttributeValue{Value: "admins\x00"})),
		"a name identifier value": statement(values("groups", saml.AttributeValue{NameID: &saml.NameID{Value: "admins"}})),
	} {
		if _, err := Attributes(assertion); !errors.Is(err, ErrDocument) {
			t.Errorf("%s: got %v, want %v", name, err, ErrDocument)
		}
	}
}
