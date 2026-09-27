package testutil

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// An OAuthIdentity is an upstream account that signs in to an OAuthServer.
type OAuthIdentity struct {
	// Subject is what the server's userinfo endpoint reports, the principal a
	// grant observes.
	Subject string
	// Account is what the server's token response names, a grant fact.
	Account string
	// APIBase, when set, is the base URL of the upstream API that serves the
	// account, which the server's token response names as api_base, another
	// grant fact.
	APIBase string
}

// OAuthServer is a fake OAuth 2.0 authorization server, the reference
// plugin's authority in tests. It runs the authorization code flow with PKCE
// (RFC 7636, S256 only): /authorize signs an account in at once and redirects
// to the client's redirect URI with a code and the request's state; /token
// exchanges each code once, for the verifier that matches its challenge,
// naming the account and any API base URL it has; /userinfo names the subject
// of an access token.
type OAuthServer struct {
	*httptest.Server
	mu sync.Mutex
	// signIn is the account that signs in at the next authorization.
	signIn OAuthIdentity
	codes  map[string]oauthCode
	tokens map[string]OAuthIdentity
	issued []string
}

type oauthCode struct {
	identity                      OAuthIdentity
	clientID, redirect, challenge string
}

// NewOAuthServer starts a fake authorization server that signs in
// operator@reference.example with account acct-reference.
func NewOAuthServer(t testing.TB) *OAuthServer {
	t.Helper()
	s := &OAuthServer{
		signIn: OAuthIdentity{Subject: "operator@reference.example", Account: "acct-reference"},
		codes:  map[string]oauthCode{}, tokens: map[string]OAuthIdentity{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", s.authorize)
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

func (s *OAuthServer) token(w http.ResponseWriter, r *http.Request) {
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		oauthError(w, "unsupported_grant_type", "Only authorization codes are exchanged.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	code, found := s.codes[r.PostForm.Get("code")]
	// A code is spent by its first exchange, whatever the outcome.
	delete(s.codes, r.PostForm.Get("code"))
	verified := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || code.clientID != r.PostForm.Get("client_id") || code.redirect != r.PostForm.Get("redirect_uri") ||
		base64.RawURLEncoding.EncodeToString(verified[:]) != code.challenge {
		oauthError(w, "invalid_grant", "The authorization code is unknown, spent or not this client's.")
		return
	}
	access, refresh := oauthToken(), oauthToken()
	s.tokens[access] = code.identity
	s.issued = append(s.issued, access, refresh)
	issued := map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600, "account": code.identity.Account}
	if code.identity.APIBase != "" {
		issued["api_base"] = code.identity.APIBase
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(issued)
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
