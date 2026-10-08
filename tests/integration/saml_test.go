//go:build integration && oidctest

package integration_test

import (
	"bytes"
	"compress/flate"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
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
func TestSAMLLoginMappingLinkingAndReplay(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newSAMLFixture(t)
	config := f.configure(t, h, owner)
	b := &browser{}
	req, state := samlBegin(t, h, b, "/api/v1/saml/login", map[string]any{"return_to": "/models"})
	raw := f.response(t, req, "persistent-worker", "saml@example.com", []string{"developers"}, nil)
	next := samlReceive(t, h, state, raw, 303)
	h.want(&browser{}, "GET", next, nil, nil, 403)
	h.want(b, "GET", next, nil, nil, 303)
	profile := h.want(b, "GET", "/api/v1/profile", nil, nil, 200)
	if profile["role"] != "developer" {
		t.Fatal(profile)
	}
	h.want(b, "GET", next, nil, nil, 403)
	samlReceive(t, h, state, raw, 403)
	req, state = samlBegin(t, h, &browser{}, "/api/v1/saml/login", map[string]any{})
	samlReceive(t, h, state, raw, 403)
	// Email never links an existing owner without a recent authenticated flow.
	collision := &browser{}
	req, state = samlBegin(t, h, collision, "/api/v1/saml/login", map[string]any{})
	next = samlReceive(t, h, state, f.response(t, req, "owner-subject", "owner@example.com", []string{"developers"}, nil), 303)
	h.want(collision, "GET", next, nil, nil, 409)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "saml_link"}, nil, 204)
	req, state = samlBegin(t, h, owner, "/api/v1/profile/saml/link", map[string]any{"return_to": "/settings/profile"})
	next = samlReceive(t, h, state, f.response(t, req, "owner-subject", "owner@example.com", []string{"developers"}, nil), 303)
	h.want(owner, "GET", next, nil, nil, 303)
	linked := h.want(owner, "GET", "/api/v1/profile/saml-identities", nil, nil, 200)
	if len(linked["items"].([]any)) != 1 {
		t.Fatal(linked)
	}
	if h.want(owner, "GET", "/api/v1/profile", nil, nil, 200)["role"] != "owner" {
		t.Fatal("link replaced local ownership")
	}
	// Signed loss of mapped access deauthorizes managed users and their sessions.
	req, state = samlBegin(t, h, b, "/api/v1/saml/login", map[string]any{})
	next = samlReceive(t, h, state, f.response(t, req, "persistent-worker", "saml@example.com", []string{"removed"}, nil), 303)
	h.want(b, "GET", next, nil, nil, 403)
	h.want(b, "GET", "/api/v1/profile", nil, nil, 401)
	var leaked bool
	if e := h.Pool.QueryRow(context.Background(), `SELECT EXISTS(SELECT 1 FROM olp.audit WHERE to_jsonb(audit)::text LIKE '%persistent-worker%') OR EXISTS(SELECT 1 FROM olp.saml_flows WHERE to_jsonb(saml_flows)::text LIKE '%persistent-worker%')`).Scan(&leaked); e != nil || leaked {
		t.Fatal(e, "identity leaked outside bounded identity/sealed storage")
	}
	if config["signing_certificate"] == "" {
		t.Fatal("no public signing certificate")
	}
	var purpose string
	if e := h.Pool.QueryRow(context.Background(), "SELECT purpose FROM olp.secrets WHERE id=$1", config["id"]).Scan(&purpose); e != nil || purpose != "saml_key" {
		t.Fatal(e, purpose)
	}
}
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

func TestSAMLPromotionKeepsKeysAndIdentitiesLocal(t *testing.T) {
	source := newAccessHarness(t)
	owner := source.owner()
	f := newSAMLFixture(t)
	sourceConfig := f.configure(t, source, owner)
	b := &browser{}
	req, state := samlBegin(t, source, b, "/api/v1/saml/login", map[string]any{})
	next := samlReceive(t, source, state, f.response(t, req, "nonportable-subject", "nonportable@example.com", []string{"developers"}, nil), 303)
	source.want(b, "GET", next, nil, nil, 303)
	exported := source.want(owner, "GET", "/api/v1/configuration/export", nil, nil, 200)
	document := exported["document"].(map[string]any)
	raw, _ := json.Marshal(document)
	for _, private := range []string{sourceConfig["id"].(string), sourceConfig["signing_certificate"].(string), "nonportable-subject", "nonportable@example.com", "saml_key", "private_key"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("export retained local identity or signing state")
		}
	}
	destination := newAccessHarness(t)
	targetOwner := destination.owner()
	token := destination.want(targetOwner, "POST", "/api/v1/management-tokens", map[string]any{"name": "configuration", "scopes": []string{"configure"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, idem("saml-token"), 201)["secret"].(string)
	destination.machineWant(token, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 403)
	destination.machineWant(token, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("saml-denied"), 403)
	destination.want(targetOwner, "POST", "/api/v1/configuration/plan", map[string]any{"document": document}, nil, 200)
	destination.want(targetOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("saml-apply"), 200)
	target := destination.want(targetOwner, "GET", "/api/v1/saml/configuration", nil, nil, 200)
	if target["id"] == sourceConfig["id"] || target["signing_certificate"] == sourceConfig["signing_certificate"] {
		t.Fatal("promoted installation-local signing material")
	}
	destination.machineWant(token, "GET", "/api/v1/configuration/export", nil, nil, 403)
	destination.machineWant(token, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("saml-apply"), 403)
	destination.want(targetOwner, "POST", "/api/v1/configuration/apply", map[string]any{"document": document}, idem("saml-unchanged"), 200)
	if destination.want(targetOwner, "GET", "/api/v1/saml/configuration", nil, nil, 200)["etag"] != target["etag"] {
		t.Fatal("unchanged promotion replaced trust")
	}
	var identities int
	if err := destination.Pool.QueryRow(context.Background(), "SELECT count(*) FROM olp.saml_identities").Scan(&identities); err != nil || identities != 0 {
		t.Fatal(identities, err)
	}
}
func TestSAMLRecentProofUnlinkAndOwnerProtection(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newSAMLFixture(t)
	config := f.configure(t, h, owner)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "saml_link"}, nil, 204)
	req, state := samlBegin(t, h, owner, "/api/v1/profile/saml/link", map[string]any{})
	next := samlReceive(t, h, state, f.response(t, req, "saml-owner", "owner@example.com", []string{}, nil), 303)
	h.want(owner, "GET", next, nil, nil, 303)
	identity := h.want(owner, "GET", "/api/v1/profile/saml-identities", nil, nil, 200)["items"].([]any)[0].(map[string]any)["id"].(string)
	h.want(owner, "DELETE", "/api/v1/profile/saml-identities/"+identity, nil, nil, 428)
	req, state = samlBegin(t, h, owner, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "saml_unlink", "resource_id": identity})
	next = samlReceive(t, h, state, f.response(t, req, "saml-owner", "owner@example.com", nil, func(a *saml.Assertion) { a.AuthnStatements[0].AuthnInstant = time.Now().Add(-time.Hour) }), 403)
	_ = next
	req, state = samlBegin(t, h, owner, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "saml_unlink", "resource_id": identity})
	next = samlReceive(t, h, state, f.response(t, req, "saml-owner", "owner@example.com", nil, nil), 303)
	h.want(owner, "GET", next, nil, nil, 303)
	// A verified SAML identity can be the recovery path while local sign-in is off.
	setting := h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "false"}, etagHeader(setting), 200)
	input := map[string]any{}
	for _, field := range []string{"enabled", "metadata_xml", "email_attribute", "groups_attribute", "name_attribute", "default_role", "email_role_mappings", "group_role_mappings"} {
		input[field] = config[field]
	}
	input["enabled"] = false
	h.want(owner, "PUT", "/api/v1/saml/configuration", input, etagHeader(config), 409)
	h.want(owner, "DELETE", "/api/v1/profile/saml-identities/"+identity, nil, nil, 409)
	// A new SP signing key keeps the IdP's role evidence, so the owner stays usable.
	input["enabled"] = true
	input["rotate_signing_key"] = true
	config = h.want(owner, "PUT", "/api/v1/saml/configuration", input, etagHeader(config), 200)
	var evidence bool
	if err := h.Pool.QueryRow(t.Context(), "SELECT role_claims IS NOT NULL FROM olp.saml_identities WHERE id=$1", identity).Scan(&evidence); err != nil || !evidence {
		t.Fatalf("role evidence=%v err=%v", evidence, err)
	}
	setting = h.want(owner, "GET", "/api/v1/settings/auth.local_login_enabled", nil, nil, 200)
	h.want(owner, "PUT", "/api/v1/settings/auth.local_login_enabled", map[string]any{"value": "true"}, etagHeader(setting), 200)
	h.want(owner, "DELETE", "/api/v1/profile/saml-identities/"+identity, nil, nil, 204)
	if len(h.want(owner, "GET", "/api/v1/profile/saml-identities", nil, nil, 200)["items"].([]any)) != 0 {
		t.Fatal("identity survived unlink")
	}
}

func TestLinkedSAMLDoesNotReplaceEnrolledMFA(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	f := newSAMLFixture(t)
	f.configure(t, h, owner)
	h.want(owner, "POST", "/api/v1/profile/reauthenticate", map[string]any{"current_password": accessPassword, "purpose": "saml_link"}, nil, 204)
	req, state := samlBegin(t, h, owner, "/api/v1/profile/saml/link", map[string]any{})
	next := samlReceive(t, h, state, f.response(t, req, "mfa-owner", "owner@example.com", nil, nil), 303)
	h.want(owner, "GET", next, nil, nil, 303)
	mfaEnrollTOTP(t, h, owner)
	req, state = samlBegin(t, h, owner, "/api/v1/profile/saml/reauthenticate", map[string]any{"purpose": "mfa_manage"})
	next = samlReceive(t, h, state, f.response(t, req, "mfa-owner", "owner@example.com", nil, nil), 303)
	h.want(owner, "GET", next, nil, nil, 403)
	status := h.want(owner, "GET", "/api/v1/profile/mfa", nil, nil, 200)
	h.want(owner, "POST", "/api/v1/profile/mfa/recovery-codes", map[string]any{}, etagHeader(status), 428)
}
