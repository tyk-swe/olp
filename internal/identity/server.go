package identity

import (
	"net/http"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/access/httpkit"
	"github.com/tyk-swe/olp/internal/platform/secrets"
)

type Config struct {
	Bootstrap                   func() (string, bool)
	ManagementNetworkRestricted bool
	ResponseCacheConfigured     bool
}

type passwordGate struct {
	slots     chan struct{}
	dummyHash string
}

func newPasswordGate() *passwordGate {
	return &passwordGate{
		slots:     make(chan struct{}, 4),
		dummyHash: secrets.HashPassword(secrets.Token()),
	}
}

func (g *passwordGate) run(r *http.Request, work func()) error {
	select {
	case g.slots <- struct{}{}:
		defer func() { <-g.slots }()
		work()
		return r.Context().Err()
	default:
		return httpkit.Fail(429, "authentication_busy", "Authentication is busy. Try again shortly.")
	}
}

type accountService struct {
	access                      *access.Server
	bootstrap                   func() (string, bool)
	managementNetworkRestricted bool
	responseCacheConfigured     bool
	passwords                   *passwordGate
	sessions                    *sessionService
	mfa                         *mfaService
}

type sessionService struct {
	access    *access.Server
	passwords *passwordGate
}

type mfaService struct {
	access   *access.Server
	sessions *sessionService
}

type oidcService struct {
	access   *access.Server
	sessions *sessionService
}

type samlService struct {
	access   *access.Server
	sessions *sessionService
}

type scimService struct {
	access *access.Server
}

type Server struct {
	accounts *accountService
	sessions *sessionService
	mfa      *mfaService
	oidc     *oidcService
	saml     *samlService
	scim     *scimService
}

func New(server *access.Server, cfg Config) *Server {
	bootstrap := cfg.Bootstrap
	if bootstrap == nil {
		bootstrap = func() (string, bool) { return "", false }
	}
	passwords := newPasswordGate()
	sessions := &sessionService{access: server, passwords: passwords}
	mfa := &mfaService{access: server, sessions: sessions}
	oidc := &oidcService{access: server, sessions: sessions}
	saml := &samlService{access: server, sessions: sessions}
	scim := &scimService{access: server}
	accounts := &accountService{
		access:                      server,
		bootstrap:                   bootstrap,
		managementNetworkRestricted: cfg.ManagementNetworkRestricted,
		responseCacheConfigured:     cfg.ResponseCacheConfigured,
		passwords:                   passwords,
		sessions:                    sessions,
		mfa:                         mfa,
	}
	return &Server{
		accounts: accounts,
		sessions: sessions,
		mfa:      mfa,
		oidc:     oidc,
		saml:     saml,
		scim:     scim,
	}
}

func (s *Server) Register(mux *http.ServeMux) {
	s.scim.register(mux)
	s.saml.register(mux)
	s.oidc.register(mux)
	s.mfa.register(mux)
	s.sessions.register(mux)
	s.accounts.register(mux)
}
