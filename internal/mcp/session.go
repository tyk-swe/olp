package mcp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"

	"github.com/tyk-swe/olp/internal/access"
)

const (
	sessionIdleTimeout  = 15 * time.Minute
	maxSessionsPerKey   = 64
	sessionIDRandomSize = 32
)

type principalIdentity struct {
	id             string
	issuer         string
	lookupID       string
	workloadDigest string
	endUserDigest  string
}

func principalOf(authority access.Authority) principalIdentity {
	return principalIdentity{
		id: authority.ID, issuer: authority.Issuer,
		lookupID: authority.LookupID, workloadDigest: authority.WorkloadDigest, endUserDigest: verifiedEndUser(authority),
	}
}

func verifiedEndUser(a access.Authority) string {
	if a.WorkloadIssuerID == nil {
		return ""
	}
	return a.EndUserDigest
}
func (p principalIdentity) capacityScope() string {
	return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest)
}
func (p principalIdentity) scope() string {
	if p.endUserDigest == "" {
		return p.capacityScope()
	}
	return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest, p.endUserDigest)
}
func principalScope(parts ...string) string {
	digest := sha256.New()
	for _, part := range parts {
		var size [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(size[:], uint64(len(part)))
		digest.Write(size[:n])
		digest.Write([]byte(part))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

type sessionBinding struct {
	installationID string
	projectID      string
	principal      principalIdentity
}

func bindingOf(authority access.Authority) sessionBinding {
	binding := sessionBinding{installationID: authority.InstallationID, principal: principalOf(authority)}
	if authority.ProjectID != nil {
		binding.projectID = *authority.ProjectID
	}
	return binding
}

func (b sessionBinding) scope() string { return b.principal.scope() }
func (b sessionBinding) member(id string) string {
	if b.principal.endUserDigest == "" {
		return id
	}
	return b.scope() + ":" + id
}

type certificationState string

const (
	certificationCertified certificationState = "certified"
	certificationDrifted   certificationState = "drifted"
	certificationInvalid   certificationState = "invalid"
)

type sessionRecord struct {
	id                string
	binding           sessionBinding
	protocol          string
	toolsetID         string
	toolsetRevisionID string
	releaseID         string
	certifications    map[string]certificationState
	createdAt         time.Time
	lastActiveAt      time.Time
	initialized       bool
}

func (r sessionRecord) certification(serverID string) certificationState {
	if r.certifications == nil {
		return certificationInvalid
	}
	state, ok := r.certifications[serverID]
	if !ok {
		return certificationInvalid
	}
	return state
}

func newSessionID() (string, error) {
	raw := make([]byte, sessionIDRandomSize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
