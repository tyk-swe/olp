// The loopback-only SAML identity provider used by Chromium journeys. It holds
// its ephemeral signing key in memory and accepts only declared test callbacks.
package main

import (
	"compress/flate"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/google/uuid"
	dsig "github.com/russellhaering/goxmldsig"
)

const origin = "http://localhost:4196"

var form = template.Must(template.New("response").Parse(`<!doctype html><html lang="en"><head><title>Test SAML identity</title></head><body><h1>Test SAML identity</h1><form method="post" action="{{.ACS}}"><input type="hidden" name="SAMLResponse" value="{{.Response}}"><input type="hidden" name="RelayState" value="{{.State}}"><button>Continue to console</button></form></body></html>`))

func main() {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Chromium SAML"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		log.Fatal(err)
	}
	metadata := fmt.Sprintf(`<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata" xmlns:ds="http://www.w3.org/2000/09/xmldsig#" entityID="%s"><md:IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol"><md:KeyDescriptor use="signing"><ds:KeyInfo><ds:X509Data><ds:X509Certificate>%s</ds:X509Certificate></ds:X509Data></ds:KeyInfo></md:KeyDescriptor><md:SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="%s/sign-in"/></md:IDPSSODescriptor></md:EntityDescriptor>`, origin, base64.StdEncoding.EncodeToString(der), origin)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metadata", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		io.WriteString(w, metadata)
	})
	mux.HandleFunc("GET /sign-in", func(w http.ResponseWriter, r *http.Request) {
		if len(r.URL.RawQuery) > 128<<10 {
			http.Error(w, "request", 400)
			return
		}
		data, err := base64.StdEncoding.DecodeString(r.URL.Query().Get("SAMLRequest"))
		if err != nil {
			http.Error(w, "request", 400)
			return
		}
		reader := flate.NewReader(strings.NewReader(string(data)))
		defer reader.Close()
		data, err = io.ReadAll(io.LimitReader(reader, 65537))
		if err != nil || len(data) > 65536 {
			http.Error(w, "request", 400)
			return
		}
		var req saml.AuthnRequest
		if xml.Unmarshal(data, &req) != nil || req.ID == "" || req.ProtocolBinding != saml.HTTPPostBinding {
			http.Error(w, "request", 400)
			return
		}
		if req.AssertionConsumerServiceURL != "http://127.0.0.1:4182/api/v1/saml/acs" && req.AssertionConsumerServiceURL != "http://localhost:4183/api/v1/saml/acs" {
			http.Error(w, "callback", 400)
			return
		}
		now := time.Now().UTC()
		a := &saml.Assertion{ID: "_" + uuid.NewString(), Version: "2.0", IssueInstant: now, Issuer: saml.Issuer{Value: origin}, Subject: &saml.Subject{NameID: &saml.NameID{Format: string(saml.PersistentNameIDFormat), Value: "browser-member"}, SubjectConfirmations: []saml.SubjectConfirmation{{Method: "urn:oasis:names:tc:SAML:2.0:cm:bearer", SubjectConfirmationData: &saml.SubjectConfirmationData{Recipient: req.AssertionConsumerServiceURL, InResponseTo: req.ID, NotOnOrAfter: now.Add(2 * time.Minute)}}}}, Conditions: &saml.Conditions{NotBefore: now.Add(-time.Second), NotOnOrAfter: now.Add(2 * time.Minute), AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: req.Issuer.Value}}}}, AuthnStatements: []saml.AuthnStatement{{AuthnInstant: now, AuthnContext: saml.AuthnContext{AuthnContextClassRef: &saml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}}}}, AttributeStatements: []saml.AttributeStatement{{Attributes: []saml.Attribute{{Name: "email", Values: []saml.AttributeValue{{Value: "saml-browser@example.com"}}}, {Name: "groups", Values: []saml.AttributeValue{{Value: "developers"}}}, {Name: "name", Values: []saml.AttributeValue{{Value: "SAML Browser"}}}}}}}
		signing, err := dsig.NewSigningContext(key, [][]byte{der})
		if err != nil {
			http.Error(w, "sign", 500)
			return
		}
		signing.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
		assertion, err := signing.SignEnveloped(a.Element())
		if err != nil {
			http.Error(w, "sign", 500)
			return
		}
		response := &saml.Response{ID: "_" + uuid.NewString(), InResponseTo: req.ID, Version: "2.0", IssueInstant: now, Destination: req.AssertionConsumerServiceURL, Issuer: &saml.Issuer{Value: origin}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}}
		root := response.Element()
		root.AddChild(assertion)
		document := etree.NewDocument()
		document.SetRoot(root)
		raw, err := document.WriteToBytes()
		if err != nil {
			http.Error(w, "response", 500)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = form.Execute(w, map[string]string{"ACS": req.AssertionConsumerServiceURL, "State": r.URL.Query().Get("RelayState"), "Response": base64.StdEncoding.EncodeToString(raw)})
	})
	server := &http.Server{Addr: "127.0.0.1:4196", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(server.ListenAndServe())
}
