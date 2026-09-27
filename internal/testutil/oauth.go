package testutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// An OAuthIdentity is an upstream account that signs in to an OAuthServer.
type OAuthIdentity struct {
	// Subject is what the server's userinfo endpoint reports, the principal a
	// grant observes.
	Subject string
	// Account is what the server's token response names, a grant fact.
	Account string
}

// OAuthServer is a fake OAuth 2.0 authorization server, the reference
// plugin's authority in tests. It runs the authorization code flow with PKCE
// (RFC 7636, S256 only): /authorize signs an account in at once and redirects
// to the client's redirect URI with a code and the request's state; /token
// exchanges each code once, for the verifier that matches its challenge;
// /userinfo names the subject of an access token.
//
// It also runs the device authorization grant (RFC 8628): /device/code issues a
// device code and a user code, which the operator approves or denies by posting
// the user code and a decision to the verification URI, /device; meanwhile
// /token answers polls for the device code with authorization_pending, or
// slow_down when polled again within DeviceInterval, then issues the approved
// account's tokens once.
type OAuthServer struct {
	*httptest.Server
	mu sync.Mutex
	// signIn is the account that signs in at the next authorization.
	signIn  OAuthIdentity
	codes   map[string]oauthCode
	devices map[string]*oauthDevice
	polls   int
	tokens  map[string]OAuthIdentity
	issued  []string
}

// DeviceInterval is the polling interval an OAuthServer's device
// authorizations ask for, long enough that a test's consecutive polls of a
// pending device are always told to slow down.
const DeviceInterval = 30 * time.Second

type oauthCode struct {
	identity                      OAuthIdentity
	clientID, redirect, challenge string
}

// An oauthDevice is a device authorization, pending until the operator
// approves it as an account or denies it.
type oauthDevice struct {
	clientID, userCode string
	approved           *OAuthIdentity
	denied             bool
	expires, polled    time.Time
}

// NewOAuthServer starts a fake authorization server that signs in
// operator@reference.example with account acct-reference.
func NewOAuthServer(t testing.TB) *OAuthServer {
	t.Helper()
	s := &OAuthServer{
		signIn: OAuthIdentity{Subject: "operator@reference.example", Account: "acct-reference"},
		codes:  map[string]oauthCode{}, devices: map[string]*oauthDevice{}, tokens: map[string]OAuthIdentity{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", s.authorize)
	mux.HandleFunc("POST /device/code", s.deviceCode)
	mux.HandleFunc("POST /device", s.verifyDevice)
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /userinfo", s.userinfo)
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// SignInAs makes identity the account that signs in at later authorizations.
func (s *OAuthServer) SignInAs(identity OAuthIdentity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.signIn = identity
}

// Authorized reports the account whose access token authorizes a request's
// Authorization: Bearer header.
func (s *OAuthServer) Authorized(r *http.Request) (OAuthIdentity, bool) {
	token, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	identity, ok := s.tokens[token]
	return identity, found && ok
}

// DevicePolls counts the token requests that polled for a device's grant.
func (s *OAuthServer) DevicePolls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.polls
}

// ExpireDevices ends every device authorization's lifetime, so later polls for
// them are told expired_token.
func (s *OAuthServer) ExpireDevices() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, device := range s.devices {
		device.expires = time.Now()
	}
}

// Issued returns the access and refresh tokens the server has issued.
func (s *OAuthServer) Issued() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.issued...)
}

func (s *OAuthServer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	redirect, err := url.Parse(q.Get("redirect_uri"))
	if err != nil || redirect.Scheme == "" || q.Get("response_type") != "code" || q.Get("client_id") == "" ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" {
		http.Error(w, "invalid authorization request", http.StatusBadRequest)
		return
	}
	code := oauthToken()
	s.mu.Lock()
	s.codes[code] = oauthCode{identity: s.signIn, clientID: q.Get("client_id"), redirect: redirect.String(), challenge: q.Get("code_challenge")}
	s.mu.Unlock()
	callback := redirect.Query()
	callback.Set("code", code)
	callback.Set("state", q.Get("state"))
	redirect.RawQuery = callback.Encode()
	http.Redirect(w, r, redirect.String(), http.StatusFound)
}

func (s *OAuthServer) deviceCode(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil || r.PostForm.Get("client_id") == "" {
		oauthError(w, "invalid_request", "Name the client.")
		return
	}
	var b [4]byte
	rand.Read(b[:])
	device := &oauthDevice{clientID: r.PostForm.Get("client_id"), userCode: fmt.Sprintf("%X-%X", b[:2], b[2:]), expires: time.Now().Add(10 * time.Minute)}
	code := oauthToken()
	s.mu.Lock()
	s.devices[code] = device
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"device_code": code, "user_code": device.userCode, "verification_uri": s.URL + "/device",
		"expires_in": 600, "interval": int(DeviceInterval / time.Second),
	})
}

// verifyDevice is the verification URI's form: the operator enters the user
// code and approves, signing in as the server's current account, or denies.
func (s *OAuthServer) verifyDevice(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, device := range s.devices {
		if device.userCode != r.PostForm.Get("user_code") {
			continue
		}
		switch r.PostForm.Get("decision") {
		case "approve":
			device.approved = new(s.signIn)
		case "deny":
			device.denied = true
		default:
			http.Error(w, "approve or deny", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Error(w, "unknown user code", http.StatusNotFound)
}

// DecideDevice decides a device authorization as the operator does at its
// verification URI: it enters the user code and approves or denies, with
// decision "approve" or "deny".
func DecideDevice(t testing.TB, verificationURL, userCode, decision string) {
	t.Helper()
	response, err := http.PostForm(verificationURL, url.Values{"user_code": {userCode}, "decision": {decision}})
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("deciding device %s answered %d", userCode, response.StatusCode)
	}
}

func (s *OAuthServer) token(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil {
		oauthError(w, "invalid_request", "Send a form.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		s.exchangeCode(w, r)
	case "urn:ietf:params:oauth:grant-type:device_code":
		s.pollDevice(w, r)
	default:
		oauthError(w, "unsupported_grant_type", "Only authorization codes and device codes are exchanged.")
	}
}

func (s *OAuthServer) exchangeCode(w http.ResponseWriter, r *http.Request) {
	code, found := s.codes[r.PostForm.Get("code")]
	// A code is spent by its first exchange, whatever the outcome.
	delete(s.codes, r.PostForm.Get("code"))
	verified := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || code.clientID != r.PostForm.Get("client_id") || code.redirect != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(verified[:]) != code.challenge {
		oauthError(w, "invalid_grant", "The authorization code is unknown, spent or not this client's.")
		return
	}
	s.issue(w, code.identity)
}

func (s *OAuthServer) pollDevice(w http.ResponseWriter, r *http.Request) {
	s.polls++
	device, found := s.devices[r.PostForm.Get("device_code")]
	now := time.Now()
	switch {
	case !found || device.clientID != r.PostForm.Get("client_id"):
		oauthError(w, "invalid_grant", "The device code is unknown, spent or not this client's.")
	case !now.Before(device.expires):
		oauthError(w, "expired_token", "The device code expired.")
	case device.denied:
		oauthError(w, "access_denied", "The operator denied the device.")
	case device.approved != nil:
		delete(s.devices, r.PostForm.Get("device_code"))
		s.issue(w, *device.approved)
	case now.Sub(device.polled) < DeviceInterval:
		device.polled = now
		oauthError(w, "slow_down", "Poll less often.")
	default:
		device.polled = now
		oauthError(w, "authorization_pending", "The operator has not approved the device yet.")
	}
}

// issue grants an account an access token and a refresh token.
func (s *OAuthServer) issue(w http.ResponseWriter, identity OAuthIdentity) {
	access, refresh := oauthToken(), oauthToken()
	s.tokens[access] = identity
	s.issued = append(s.issued, access, refresh)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "account": identity.Account})
}

func (s *OAuthServer) userinfo(w http.ResponseWriter, r *http.Request) {
	identity, ok := s.Authorized(r)
	if !ok {
		w.Header().Set("WWW-Authenticate", "Bearer")
		oauthErrorStatus(w, http.StatusUnauthorized, "invalid_token", "The access token is unknown.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"sub": identity.Subject})
}

func oauthError(w http.ResponseWriter, code, description string) {
	oauthErrorStatus(w, http.StatusBadRequest, code, description)
}

func oauthErrorStatus(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": description})
}

func oauthToken() string {
	var b [24]byte
	rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}
