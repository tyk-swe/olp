//go:build integration && oidctest

package integration_test

import (
	"bytes"
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/google/uuid"
	dsig "github.com/russellhaering/goxmldsig"
	"github.com/tyk-swe/olp/internal/samlidentity"
)

type samlFixture struct {
	key      *rsa.PrivateKey
	cert     []byte
	server   *httptest.Server
	metadata string
}

func newSAMLFixture(t *testing.T) *samlFixture {
	t.Helper()
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test SAML"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, e := x509.CreateCertificate(rand.Reader, c, c, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	f := &samlFixture{key: key, cert: der}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metadata" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		io.WriteString(w, f.metadata)
	}))
	t.Cleanup(f.server.Close)
	f.metadata = fmt.Sprintf(`<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="%s"><md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s/sign-in"/></md:IDPSSODescriptor></md:EntityDescriptor>`, f.server.URL, base64.StdEncoding.EncodeToString(der), f.server.URL)
	if _, err := samlidentity.ParseMetadata([]byte(f.metadata)); err != nil {
		t.Fatal("invalid fixture metadata", err)
	}
	return f
}
func (f *samlFixture) configure(t *testing.T, h *accessHarness, owner *browser) map[string]any {
	t.Helper()
	input := map[string]any{"enabled": true, "metadata_xml": f.metadata, "email_attribute": "email", "groups_attribute": "groups", "name_attribute": "name", "default_role": nil, "email_role_mappings": []any{}, "group_role_mappings": []any{map[string]any{"claim_value": "developers", "role": "developer"}}}
	imported := h.want(owner, "POST", "/api/v1/saml/import", map[string]any{"url": f.server.URL + "/metadata"}, nil, 200)
	if imported["metadata_xml"] != f.metadata {
		t.Fatal("metadata import")
	}
	return h.want(owner, "PUT", "/api/v1/saml/configuration", input, nil, 200)
}
func samlBegin(t *testing.T, h *accessHarness, b *browser, path string, body map[string]any) (saml.AuthnRequest, string) {
	t.Helper()
	out := h.want(b, "POST", path, body, nil, 200)
	u, e := url.Parse(out["authorization_url"].(string))
	if e != nil {
		t.Fatal(e)
	}
	if u.Query().Get("Signature") == "" || u.Query().Get("SigAlg") != dsig.RSASHA256SignatureMethod {
		t.Fatal("unsigned request")
	}
	raw, e := base64.StdEncoding.DecodeString(u.Query().Get("SAMLRequest"))
	if e != nil {
		t.Fatal(e)
	}
	reader := flate.NewReader(bytes.NewReader(raw))
	defer reader.Close()
	data, e := io.ReadAll(reader)
	if e != nil {
		t.Fatal(e)
	}
	var req saml.AuthnRequest
	if e = xml.Unmarshal(data, &req); e != nil {
		t.Fatal(e)
	}
	if req.ProtocolBinding != saml.HTTPPostBinding {
		t.Fatal("binding")
	}
	return req, u.Query().Get("RelayState")
}
func (f *samlFixture) response(t *testing.T, request saml.AuthnRequest, subject, email string, groups []string, edit func(*saml.Assertion)) []byte {
	t.Helper()
	now := time.Now().UTC()
	values := []saml.AttributeValue{}
	for _, g := range groups {
		values = append(values, saml.AttributeValue{Value: g})
	}
	a := &saml.Assertion{ID: "_" + uuid.NewString(), Version: "2.0", IssueInstant: now, Issuer: saml.Issuer{Value: f.server.URL}, Subject: &saml.Subject{NameID: &saml.NameID{Format: string(saml.PersistentNameIDFormat), Value: subject}, SubjectConfirmations: []saml.SubjectConfirmation{{Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer", SubjectConfirmationData: &saml.SubjectConfirmationData{Recipient: request.AssertionConsumerServiceURL, InResponseTo: request.ID, NotOnOrAfter: now.Add(2 * time.Minute)}}}}, Conditions: &saml.Conditions{NotBefore: now.Add(-time.Second), NotOnOrAfter: now.Add(2 * time.Minute), AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: request.Issuer.Value}}}}, AuthnStatements: []saml.AuthnStatement{{AuthnInstant: now, AuthnContext: saml.AuthnContext{AuthnContextClassRef: &saml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}}}}, AttributeStatements: []saml.AttributeStatement{{Attributes: []saml.Attribute{{Name: "email", Values: []saml.AttributeValue{{Value: email}}}, {Name: "groups", Values: values}, {Name: "name", Values: []saml.AttributeValue{{Value: "SAML Member"}}}}}}}
	if edit != nil {
		edit(a)
	}
	ctx, e := dsig.NewSigningContext(f.key, [][]byte{f.cert})
	if e != nil {
		t.Fatal(e)
	}
	ctx.IdAttribute = "ID"
	ctx.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	signed, e := ctx.SignEnveloped(a.Element())
	if e != nil {
		t.Fatal(e)
	}
	r := saml.Response{ID: "_" + uuid.NewString(), InResponseTo: request.ID, Version: "2.0", IssueInstant: now, Destination: request.AssertionConsumerServiceURL, Issuer: &saml.Issuer{Value: f.server.URL}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}}
	root := r.Element()
	root.AddChild(signed)
	d := etree.NewDocument()
	d.SetRoot(root)
	raw, e := d.WriteToBytes()
	if e != nil {
		t.Fatal(e)
	}
	return raw
}
func samlReceive(t *testing.T, h *accessHarness, state string, raw []byte, want int) string {
	t.Helper()
	form := url.Values{"RelayState": {state}, "SAMLResponse": {base64.StdEncoding.EncodeToString(raw)}}
	req, e := http.NewRequest("POST", h.HTTP.URL+"/api/v1/saml/acs", strings.NewReader(form.Encode()))
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://idp.test")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, e := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("SAML receive: %d want %d: %s", resp.StatusCode, want, body)
	}
	if len(resp.Cookies()) != 0 {
		t.Fatal("POST receiver created browser authority")
	}
	return resp.Header.Get("Location")
}

// Concurrent completions of one flow mint a single session.

// Email never links an existing owner without a recent authenticated flow.

// Signed loss of mapped access deauthorizes managed users and their sessions.

func TestSAMLRefusesMalformedUnsignedAndMisboundAssertions(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newSAMLFixture(t)
	f.configure(t, h, owner)
	for _, tc := range []struct {
		name   string
		edit   func(*saml.Assertion)
		tamper func([]byte) []byte
	}{
		{"audience", func(a *saml.Assertion) { a.Conditions.AudienceRestrictions[0].Audience.Value = "https://other.test" }, nil},
		{"missing_audience", func(a *saml.Assertion) { a.Conditions.AudienceRestrictions = nil }, nil},
		{"recipient", func(a *saml.Assertion) {
			a.Subject.SubjectConfirmations[0].SubjectConfirmationData.Recipient = "https://other.test"
		}, nil},
		{"request", func(a *saml.Assertion) {
			a.Subject.SubjectConfirmations[0].SubjectConfirmationData.InResponseTo = "different"
		}, nil},
		{"expired", func(a *saml.Assertion) {
			a.IssueInstant = time.Now().Add(-time.Hour)
			a.Conditions.NotBefore = time.Now().Add(-time.Hour)
			a.Conditions.NotOnOrAfter = time.Now().Add(-30 * time.Minute)
		}, nil},
		{"unsigned", nil, func(b []byte) []byte {
			d := etree.NewDocument()
			_ = d.ReadFromBytes(b)
			a := d.Root().FindElement("./Assertion")
			if a == nil {
				a = d.Root().ChildElements()[len(d.Root().ChildElements())-1]
			}
			for _, v := range a.ChildElements() {
				if v.Tag == "Signature" {
					a.RemoveChild(v)
				}
			}
			raw, _ := d.WriteToBytes()
			return raw
		}},
		{"duplicate_assertion", nil, func(b []byte) []byte {
			d := etree.NewDocument()
			_ = d.ReadFromBytes(b)
			root := d.Root()
			root.AddChild(root.ChildElements()[len(root.ChildElements())-1].Copy())
			raw, _ := d.WriteToBytes()
			return raw
		}},
		{"missing_subject", func(a *saml.Assertion) { a.Subject = nil }, nil},
		{"missing_conditions", func(a *saml.Assertion) { a.Conditions = nil }, nil},
		{"missing_confirmation", func(a *saml.Assertion) { a.Subject.SubjectConfirmations[0].SubjectConfirmationData = nil }, nil},
		{"future_issue", func(a *saml.Assertion) { a.IssueInstant = time.Now().Add(time.Hour) }, nil},
		{"future_authentication", func(a *saml.Assertion) { a.AuthnStatements[0].AuthnInstant = time.Now().Add(time.Hour) }, nil},
		{"tampered", nil, func(b []byte) []byte {
			return bytes.Replace(b, []byte("saml@example.com"), []byte("attacker@example.com"), 1)
		}},
		{"directive", nil, func(b []byte) []byte {
			return append([]byte(`<!DOCTYPE x [<!ENTITY exploit SYSTEM "file:///etc/passwd">]>`), b...)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &browser{}
			request, state := samlBegin(t, h, b, "/api/v1/saml/login", map[string]any{})
			raw := f.response(t, request, "subject", "saml@example.com", []string{"developers"}, tc.edit)
			if tc.tamper != nil {
				raw = tc.tamper(raw)
			}
			samlReceive(t, h, state, raw, 403)
			h.want(b, "GET", "/api/v1/profile", nil, nil, 401)
		})
	}
}

// A verified SAML identity can be the recovery path while local sign-in is off.

// A new SP signing key keeps the IdP's role evidence, so the owner stays usable.

// A password does not free a SAML-managed account from the IdP's authority.
