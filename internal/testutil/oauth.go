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
	// account, which the server's token response to an authorization code
	// names as api_base, another grant fact. A refresh doesn't name it again.
	APIBase string
}

// OAuthServer is a fake OAuth 2.0 authorization server, the reference
// plugin's authority in tests. It runs the authorization code flow with PKCE
// (RFC 7636, S256 only): /authorize signs an account in at once and redirects
// to the client's redirect URI with a code and the request's state; /token
// exchanges each code once, for the verifier that matches its challenge,
// naming the account and any API base URL it has, and each refresh token once,
// rotating it and naming the account; /userinfo names the subject of an access
// token.
type OAuthServer struct {
	*httptest.Server
	mu sync.Mutex
	// signIn is the account that signs in at the next authorization.
	signIn OAuthIdentity
	// lifetime is how many seconds the access tokens it issues last.
	lifetime int64
	codes    map[string]oauthCode
	tokens   map[string]OAuthIdentity
	// refreshes holds the refresh tokens not yet spent or revoked, and spent
	// those spent.
	refreshes map[string]OAuthIdentity
	spent     map[string]bool
	issued    []string
	reused    []string
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
		signIn:   OAuthIdentity{Subject: "operator@reference.example", Account: "acct-reference"},
		lifetime: 3600,
		codes:    map[string]oauthCode{}, tokens: map[string]OAuthIdentity{},
		refreshes: map[string]OAuthIdentity{}, spent: map[string]bool{},
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

// IssueFor makes the access tokens the server issues later last seconds.
func (s *OAuthServer) IssueFor(seconds int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifetime = seconds
}

// RevokeAccessTokens revokes every access token issued so far, as an
// upstream that ends sessions early does. Refresh tokens stay valid.
func (s *OAuthServer) RevokeAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.tokens)
}

// RevokeRefreshTokens revokes every refresh token not yet spent, so no grant
// the server issued can be refreshed again.
func (s *OAuthServer) RevokeRefreshTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.refreshes)
}

// Reused returns the spent refresh tokens a client presented again, each
// time it did. The server refused them.
func (s *OAuthServer) Reused() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.reused...)
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

// Issued returns the access and refresh tokens the server has issued, in
// pairs.
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
	if r.ParseForm() != nil {
		oauthError(w, "invalid_request", "The token request is not a form.")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		code, found := s.codes[r.PostForm.Get("code")]
		// A code is spent by its first exchange, whatever the outcome.
		delete(s.codes, r.PostForm.Get("code"))
		verified := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !found || code.clientID != r.PostForm.Get("client_id") || code.redirect != r.PostForm.Get("redirect_uri") ||
			base64.RawURLEncoding.EncodeToString(verified[:]) != code.challenge {
			oauthError(w, "invalid_grant", "The authorization code is unknown, spent or not this client's.")
			return
		}
		s.issue(w, code.identity, code.identity.APIBase)
	case "refresh_token":
		// A refresh token is spent by its first use, which issues the next.
		refresh := r.PostForm.Get("refresh_token")
		identity, found := s.refreshes[refresh]
		if !found {
			if s.spent[refresh] {
				s.reused = append(s.reused, refresh)
			}
			oauthError(w, "invalid_grant", "The refresh token is unknown, spent or revoked.")
			return
		}
		delete(s.refreshes, refresh)
		s.spent[refresh] = true
		s.issue(w, identity, "")
	default:
		oauthError(w, "unsupported_grant_type", "Only authorization codes and refresh tokens are exchanged.")
	}
}

// issue answers a token request with a new access token and refresh token
// for identity, naming apiBase unless it is empty. The caller holds the lock.
func (s *OAuthServer) issue(w http.ResponseWriter, identity OAuthIdentity, apiBase string) {
	access, refresh := oauthToken(), oauthToken()
	s.tokens[access], s.refreshes[refresh] = identity, identity
	s.issued = append(s.issued, access, refresh)
	issued := map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": s.lifetime, "account": identity.Account}
	if apiBase != "" {
		issued["api_base"] = apiBase
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
