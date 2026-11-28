//go:build integration

package integration_test

import (
	"compress/flate"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/beevik/etree"
	"github.com/crewjam/saml"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	dsig "github.com/russellhaering/goxmldsig"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/identity"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestAuthenticatorSecretsLastOnlyAsLongAsTheirEnrollmentOrFactor(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	userID, address := identityOwner(t, pool)
	owner := identityHTTP(t, pool, userID)

	enrollAuthenticator(t, owner)
	abandoned := storedSecrets(t, pool, secrets.MFATOTP)
	mustExec(t, pool, `UPDATE olp.mfa_challenges SET expires_at=now()-interval '1 second'`)
	challenge, seed := enrollAuthenticator(t, owner)
	pending := storedSecrets(t, pool, secrets.MFATOTP)
	if len(abandoned) != 1 || len(pending) != 1 || slices.Equal(abandoned, pending) {
		t.Fatalf("expired enrollment secrets: abandoned=%v pending=%v", abandoned, pending)
	}

	verifyAuthenticator(t, owner, challenge, seed)
	var permanent bool
	if err := pool.QueryRow(t.Context(), `SELECT expires_at IS NULL FROM olp.secrets WHERE id=$1`, pending[0]).Scan(&permanent); err != nil || !permanent {
		t.Fatalf("enrolled factor secret permanent=%t err=%v", permanent, err)
	}

	enrolled := verifiedSession(t, owner, userID)
	mustExec(t, pool, `INSERT INTO olp.mfa_challenges(id,digest,user_id,user_etag,mfa_revision,purpose,data,secret_id,expires_at)
		SELECT $2,sha256('leftover'),id,etag,mfa_revision,'enroll_totp','{}',$3,now()-interval '1 second' FROM olp.users WHERE id=$1`, userID, uuid.NewString(), pending[0])
	expectStatus(t, "management challenge", enrolled.call(t, http.MethodPost, "/api/v1/profile/mfa/challenge", "", ""), http.StatusAccepted)
	if !secretStored(t, pool, pending[0]) {
		t.Fatal("pruning an expired challenge removed an enrolled factor secret")
	}

	status := enrolled.call(t, http.MethodGet, "/api/v1/profile/mfa", "", "")
	removal := confirmIdentity(t, enrolled, "mfa_manage").call(t, http.MethodDelete, "/api/v1/profile/mfa/factors/"+pending[0], status.etag, "")
	expectStatus(t, "remove factor", removal, http.StatusNoContent)
	if secretStored(t, pool, pending[0]) {
		t.Fatal("removed factor kept its secret")
	}

	replacement := verifiedSession(t, owner, userID)
	challenge, seed = enrollAuthenticator(t, replacement)
	verifyAuthenticator(t, replacement, challenge, seed)
	if _, err := identity.RecoverPassword(t.Context(), pool, address, "a recovered owner password", true); err != nil {
		t.Fatal(err)
	}
	if left := storedSecrets(t, pool, secrets.MFATOTP); len(left) != 0 {
		t.Fatalf("authenticator recovery kept secrets %v", left)
	}
}

func TestOIDCSecretsAreDiscardedWhenTheirSignInOrConfigurationEnds(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	userID, _ := identityOwner(t, pool)
	issuer := "https://1.1.1.1"
	provider := identityTransport(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusBadRequest, `{"error":"invalid_grant"}`
		if r.URL.Path == "/.well-known/openid-configuration" {
			status, body = http.StatusOK, `{"issuer":"`+issuer+`","authorization_endpoint":"`+issuer+`/authorize","token_endpoint":"`+issuer+`/token","jwks_uri":"`+issuer+`/jwks"}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	owner := identityHTTP(t, pool, userID, func(config access.Config) access.Config {
		config.OIDCClient = &http.Client{Transport: provider}
		return config
	})
	configuration := func(extra string) string {
		return `{"discovery_url":"` + issuer + `/.well-known/openid-configuration","issuer":"` + issuer + `","client_id":"olp"` + extra + `}`
	}
	saved := owner.call(t, http.MethodPut, "/api/v1/oidc/configuration", "", configuration(`,"client_secret":"first-secret"`))
	expectStatus(t, "configure OIDC", saved, http.StatusOK)

	first := beginSignIn(t, owner, "/api/v1/oidc/login")
	stale := expiredSecret(t, pool)
	beginSignIn(t, owner, "/api/v1/oidc/login")
	if secretStored(t, pool, stale) {
		t.Fatal("starting an OIDC sign-in kept an expired secret")
	}
	browse(t, owner, http.MethodGet, "/api/v1/oidc/callback?code=refused&state="+url.QueryEscape(first.location.Query().Get("state")), "", "", first.cookies...)
	if flows := storedSecrets(t, pool, secrets.OIDCFlow); len(flows) != 1 {
		t.Fatalf("OIDC flow secrets after one callback=%v", flows)
	}

	reconfigured := owner.call(t, http.MethodPut, "/api/v1/oidc/configuration", saved.etag, configuration(""))
	expectStatus(t, "reconfigure OIDC", reconfigured, http.StatusOK)
	if flows := storedSecrets(t, pool, secrets.OIDCFlow); len(flows) != 0 {
		t.Fatalf("reconfiguring OIDC kept flow secrets %v", flows)
	}
	if kept := storedSecrets(t, pool, secrets.OIDCClientSecret); len(kept) != 1 {
		t.Fatalf("client secrets after an unrelated change=%v", kept)
	}
	expectStatus(t, "remove client secret", owner.call(t, http.MethodPut, "/api/v1/oidc/configuration", reconfigured.etag, configuration(`,"client_secret":""`)), http.StatusOK)
	if kept := storedSecrets(t, pool, secrets.OIDCClientSecret); len(kept) != 0 {
		t.Fatalf("removing the client secret kept %v", kept)
	}
}

func TestSAMLFlowSecretsAreDiscardedWhenTheirSignInOrConfigurationEnds(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	userID, _ := identityOwner(t, pool)
	owner := identityHTTP(t, pool, userID)
	provider := newSAMLProvider(t)
	definition, err := json.Marshal(map[string]any{
		"enabled": true, "metadata_xml": provider.metadata(), "email_attribute": "email", "groups_attribute": "", "name_attribute": "",
		"default_role": "viewer", "email_role_mappings": []any{}, "group_role_mappings": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, pool, `UPDATE olp.installation SET setup_complete=true`)
	saved := owner.call(t, http.MethodPut, "/api/v1/saml/configuration", "", string(definition))
	expectStatus(t, "configure SAML", saved, http.StatusOK)

	first := beginSignIn(t, owner, "/api/v1/saml/login")
	stale := expiredSecret(t, pool)
	beginSignIn(t, owner, "/api/v1/saml/login")
	if secretStored(t, pool, stale) {
		t.Fatal("starting a SAML sign-in kept an expired secret")
	}
	state := first.location.Query().Get("RelayState")
	response := provider.respond(t, owner.access.Origin, samlRequestID(t, first.location))
	form := url.Values{"RelayState": {state}, "SAMLResponse": {base64.StdEncoding.EncodeToString(response)}}.Encode()
	if received := browse(t, owner, http.MethodPost, "/api/v1/saml/acs", "application/x-www-form-urlencoded", form); received.status != http.StatusSeeOther {
		t.Fatalf("SAML response status=%d body=%s", received.status, received.body)
	}
	if completed := browse(t, owner, http.MethodGet, "/api/v1/saml/complete?state="+url.QueryEscape(state), "", "", first.cookies...); completed.status >= http.StatusBadRequest {
		t.Fatalf("SAML completion status=%d body=%s", completed.status, completed.body)
	}
	if flows := storedSecrets(t, pool, secrets.SAMLFlow); len(flows) != 1 {
		t.Fatalf("SAML flow secrets after one completion=%v", flows)
	}

	expectStatus(t, "reconfigure SAML", owner.call(t, http.MethodPut, "/api/v1/saml/configuration", saved.etag, string(definition)), http.StatusOK)
	if flows := storedSecrets(t, pool, secrets.SAMLFlow); len(flows) != 0 {
		t.Fatalf("reconfiguring SAML kept flow secrets %v", flows)
	}
}

func TestSetupNamesTheInstallationAndAdmitsOneOwner(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	client := managementHTTP(t, pool, func(mux *http.ServeMux, server *access.Server) {
		identity.New(server, identity.Config{Bootstrap: func() (string, bool) { return "bootstrap-token", true }}).Register(mux)
	})
	claim := func(email, installationName string) browserReply {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, client.server.URL+"/api/v1/setup", strings.NewReader(
			fmt.Sprintf(`{"email":%q,"password":"a first owner password","display_name":"Owner","installation_name":%q}`, email, installationName)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", client.access.Origin)
		request.Header.Set("X-OLP-Setup-Token", "bootstrap-token")
		response, err := client.server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return browserReply{status: response.StatusCode, body: body}
	}
	installed := func(stage string, setupRequired bool, name string) {
		t.Helper()
		reply := expectStatus(t, stage, client.call(t, http.MethodGet, "/api/v1/setup/status", "", ""), http.StatusOK)
		var status struct {
			SetupRequired bool `json:"setup_required"`
		}
		if err := json.Unmarshal(reply.body, &status); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := pool.QueryRow(t.Context(), `SELECT name FROM olp.installation WHERE singleton`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if status.SetupRequired != setupRequired || name != "" && stored != name {
			t.Fatalf("%s: setup required=%v name=%q, want %v and %q", stage, status.SetupRequired, stored, setupRequired, name)
		}
	}
	installed("before setup", true, "")
	if first := claim("owner@example.com", "Renamed proxy"); first.status != http.StatusCreated {
		t.Fatalf("setup = %d %s, want 201", first.status, first.body)
	}
	installed("after setup", false, "Renamed proxy")
	if second := claim("second@example.com", "Another proxy"); second.status != http.StatusConflict || !strings.Contains(string(second.body), "/setup_complete") {
		t.Fatalf("second setup = %d %s, want 409 setup_complete", second.status, second.body)
	}
	installed("after the refused setup", false, "Renamed proxy")
}

func identityOwner(t *testing.T, pool *pgxpool.Pool) (string, string) {
	t.Helper()
	userID := uuid.NewString()
	address := userID + "@example.com"
	mustExec(t, pool, `INSERT INTO olp.users(id,email,display_name,role,etag,password_hash) VALUES($1,$2,'Identity owner','owner',$3,$4)`,
		userID, address, uuid.NewString(), secrets.HashPassword("an original owner password"))
	return userID, address
}

func identityHTTP(t *testing.T, pool *pgxpool.Pool, userID string, configure ...func(access.Config) access.Config) *managementClient {
	t.Helper()
	return managementHTTPAs(t, pool, userID, func(mux *http.ServeMux, server *access.Server) {
		identity.New(server, identity.Config{}).Register(mux)
	}, configure...)
}

func verifiedSession(t *testing.T, c *managementClient, userID string) *managementClient {
	t.Helper()
	session := c.as(t, userID)
	mustExec(t, c.pool, `UPDATE olp.sessions SET mfa_verified=true WHERE id=$1`, session.sessionID)
	return session
}

func confirmIdentity(t *testing.T, c *managementClient, purpose string) *managementClient {
	t.Helper()
	token := uuid.NewString()
	mustExec(t, c.pool, `INSERT INTO olp.recent_auth(digest,session_id,purpose,expires_at) VALUES($1,$2,$3,now()+interval '5 minutes')`,
		c.auth.Digest(secrets.RecentAuthDigest, token), c.sessionID, purpose)
	confirmed := *c
	confirmed.cookies = []*http.Cookie{{Name: access.RecentCookie, Value: token}}
	return &confirmed
}

func enrollAuthenticator(t *testing.T, c *managementClient) (string, string) {
	t.Helper()
	started := confirmIdentity(t, c, "mfa_manage").call(t, http.MethodPost, "/api/v1/profile/mfa/enroll", "", `{"kind":"totp","name":"phone"}`)
	expectStatus(t, "enroll authenticator", started, http.StatusOK)
	var enrollment struct {
		Challenge string `json:"challenge"`
		Secret    string `json:"secret"`
	}
	if err := json.Unmarshal(started.body, &enrollment); err != nil {
		t.Fatal(err)
	}
	return enrollment.Challenge, enrollment.Secret
}

func verifyAuthenticator(t *testing.T, c *managementClient, challenge, seed string) {
	t.Helper()
	code, err := totp.GenerateCode(seed, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"challenge": challenge, "method": "totp", "code": code})
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, "verify authenticator", c.call(t, http.MethodPost, "/api/v1/auth/mfa/verify", "", string(body)), http.StatusOK)
}

func expiredSecret(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := placeholderSecret(t, pool, secrets.MutationReplay)
	mustExec(t, pool, `UPDATE olp.secrets SET expires_at=now()-interval '1 second' WHERE id=$1`, id)
	return id
}

type identityTransport func(*http.Request) (*http.Response, error)

func (f identityTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type browserReply struct {
	status  int
	body    []byte
	cookies []*http.Cookie
}

func browse(t *testing.T, c *managementClient, method, path, contentType, body string, cookies ...*http.Cookie) browserReply {
	t.Helper()
	r, err := http.NewRequestWithContext(t.Context(), method, c.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", contentType)
	r.Header.Set("Origin", c.access.Origin)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	client := &http.Client{Transport: c.server.Client().Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return browserReply{status: response.StatusCode, body: raw, cookies: response.Cookies()}
}

type signIn struct {
	location *url.URL
	cookies  []*http.Cookie
}

func beginSignIn(t *testing.T, c *managementClient, path string) signIn {
	t.Helper()
	started := browse(t, c, http.MethodPost, path, "application/json", "")
	var reply struct {
		AuthorizationURL string `json:"authorization_url"`
	}
	if err := json.Unmarshal(started.body, &reply); err != nil || started.status != http.StatusOK {
		t.Fatalf("begin sign-in status=%d body=%s err=%v", started.status, started.body, err)
	}
	location, err := url.Parse(reply.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	return signIn{location: location, cookies: started.cookies}
}

const samlProviderEntity = "https://idp.example/metadata"

type samlProvider struct {
	key         crypto.Signer
	certificate []byte
}

func newSAMLProvider(t *testing.T) samlProvider {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "identity provider"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return samlProvider{key: key, certificate: certificate}
}

func (p samlProvider) metadata() string {
	return `<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="` + samlProviderEntity + `"><IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">` +
		`<KeyDescriptor use="signing"><KeyInfo xmlns="http://www.w3.org/2000/09/xmldsig#"><X509Data><X509Certificate>` + base64.StdEncoding.EncodeToString(p.certificate) + `</X509Certificate></X509Data></KeyInfo></KeyDescriptor>` +
		`<SingleSignOnService Binding="` + saml.HTTPRedirectBinding + `" Location="https://idp.example/sso"/></IDPSSODescriptor></EntityDescriptor>`
}

func (p samlProvider) sign(t *testing.T, element *etree.Element) *etree.Element {
	t.Helper()
	signing, err := dsig.NewSigningContext(p.key, [][]byte{p.certificate})
	if err != nil {
		t.Fatal(err)
	}
	signing.Canonicalizer = dsig.MakeC14N10ExclusiveCanonicalizerWithPrefixList("")
	if err := signing.SetSignatureMethod(dsig.ECDSASHA256SignatureMethod); err != nil {
		t.Fatal(err)
	}
	signed, err := signing.SignEnveloped(element)
	if err != nil {
		t.Fatal(err)
	}
	children := signed.ChildElements()
	return children[len(children)-1]
}

func (p samlProvider) respond(t *testing.T, origin, requestID string) []byte {
	t.Helper()
	issued := time.Now().UTC()
	expires := issued.Add(5 * time.Minute)
	consume := origin + "/api/v1/saml/acs"
	assertion := func(signature *etree.Element) *etree.Element {
		return (&saml.Assertion{
			ID: "id-assertion", IssueInstant: issued, Version: "2.0", Issuer: saml.Issuer{Value: samlProviderEntity}, Signature: signature,
			Subject: &saml.Subject{
				NameID: &saml.NameID{Format: string(saml.PersistentNameIDFormat), Value: "member"},
				SubjectConfirmations: []saml.SubjectConfirmation{{
					Method:                  "urn:oasis:names:tc:SAML:2.0:cm:bearer",
					SubjectConfirmationData: &saml.SubjectConfirmationData{InResponseTo: requestID, NotOnOrAfter: expires, Recipient: consume},
				}},
			},
			Conditions: &saml.Conditions{
				NotBefore: issued, NotOnOrAfter: expires,
				AudienceRestrictions: []saml.AudienceRestriction{{Audience: saml.Audience{Value: origin + "/api/v1/saml/metadata"}}},
			},
			AuthnStatements: []saml.AuthnStatement{{
				AuthnInstant: issued, SessionIndex: "session",
				AuthnContext: saml.AuthnContext{AuthnContextClassRef: &saml.AuthnContextClassRef{Value: "urn:oasis:names:tc:SAML:2.0:ac:classes:PasswordProtectedTransport"}},
			}},
			AttributeStatements: []saml.AttributeStatement{{Attributes: []saml.Attribute{{Name: "email", Values: []saml.AttributeValue{{Type: "xs:string", Value: "member@example.com"}}}}}},
		}).Element()
	}
	signedAssertion := assertion(p.sign(t, assertion(nil)))
	response := func(signature *etree.Element) *etree.Element {
		element := (&saml.Response{
			ID: "id-response", InResponseTo: requestID, Version: "2.0", IssueInstant: issued, Destination: consume,
			Issuer: &saml.Issuer{Value: samlProviderEntity}, Status: saml.Status{StatusCode: saml.StatusCode{Value: saml.StatusSuccess}}, Signature: signature,
		}).Element()
		element.AddChild(signedAssertion.Copy())
		return element
	}
	document := etree.NewDocument()
	document.SetRoot(response(p.sign(t, response(nil))))
	raw, err := document.WriteToBytes()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func samlRequestID(t *testing.T, location *url.URL) string {
	t.Helper()
	compressed, err := base64.StdEncoding.DecodeString(location.Query().Get("SAMLRequest"))
	if err != nil {
		t.Fatal(err)
	}
	document := etree.NewDocument()
	if _, err := document.ReadFrom(flate.NewReader(strings.NewReader(string(compressed)))); err != nil {
		t.Fatal(err)
	}
	return document.Root().SelectAttrValue("ID", "")
}
