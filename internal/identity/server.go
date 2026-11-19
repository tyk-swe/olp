package identity

import (
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/secrets"
)

type Config struct {
	Bootstrap                   func() (string, bool)
	ManagementNetworkRestricted bool
	ResponseCacheConfigured     bool
}

// Server serves sign-in, sessions, members and the identity provider protocols.
type Server struct {
	access                      *access.Server
	bootstrap                   func() (string, bool)
	managementNetworkRestricted bool
	responseCacheConfigured     bool
	passwordSlots               chan struct{}
	dummyPassword               string
}

func New(server *access.Server, cfg Config) *Server {
	bootstrap := cfg.Bootstrap
	if bootstrap == nil {
		bootstrap = func() (string, bool) { return "", false }
	}
	return &Server{
		access: server, bootstrap: bootstrap,
		managementNetworkRestricted: cfg.ManagementNetworkRestricted, responseCacheConfigured: cfg.ResponseCacheConfigured,
		passwordSlots: make(chan struct{}, 4), dummyPassword: secrets.HashPassword(secrets.Token()),
	}
}
