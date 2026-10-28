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
}

func principalOf(authority access.Authority) principalIdentity {
	return principalIdentity{
		id: authority.ID, issuer: authority.Issuer,
		lookupID: authority.LookupID, workloadDigest: authority.WorkloadDigest,
	}
}

func (p principalIdentity) scope() string {
	digest := sha256.New()
	for _, part := range []string{p.id, p.issuer, p.lookupID, p.workloadDigest} {
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
