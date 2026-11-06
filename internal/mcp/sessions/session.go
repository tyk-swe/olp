package sessions

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"

	keyauthority "github.com/tyk-swe/olp/internal/access/authority"
)

const (
	IdleTimeout         = 15 * time.Minute
	MaxPerKey           = 64
	sessionIDRandomSize = 32
)

type principalIdentity struct {
	id              string
	issuer          string
	lookupID        string
	workloadDigest  string
	endUserDigest   string
	delegationAgent string
	delegationToken string
	taskCapability  string
}

func principalOf(authority keyauthority.Authority) principalIdentity {
	capability := ""
	if authority.TaskCapability != nil {
		capability = authority.TaskCapability.Digest
	}
	return principalIdentity{
		id: authority.ID, issuer: authority.Issuer,
		lookupID: authority.LookupID, workloadDigest: authority.WorkloadDigest, endUserDigest: VerifiedEndUser(authority),
		delegationAgent: authority.DelegationAgentID, delegationToken: authority.DelegationTokenDigest,
		taskCapability: capability,
	}
}

func VerifiedEndUser(a keyauthority.Authority) string {
	if a.WorkloadIssuerID == nil && a.TaskCapability == nil {
		return ""
	}
	return a.EndUserDigest
}
func (p principalIdentity) capacityScope() string {
	return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest)
}
func (p principalIdentity) scope() string {
	if p.taskCapability != "" {
		return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest, p.endUserDigest, p.delegationAgent, p.delegationToken, p.taskCapability)
	}
	if p.endUserDigest == "" && p.delegationToken == "" {
		return p.capacityScope()
	}
	if p.delegationToken == "" {
		return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest, p.endUserDigest)
	}
	return principalScope(p.id, p.issuer, p.lookupID, p.workloadDigest, p.endUserDigest, p.delegationAgent, p.delegationToken)
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

type Binding struct {
	installationID string
	projectID      string
	principal      principalIdentity
}

func BindingOf(authority keyauthority.Authority) Binding {
	binding := Binding{installationID: authority.InstallationID, principal: principalOf(authority)}
	if authority.ProjectID != nil {
		binding.projectID = *authority.ProjectID
	}
	return binding
}

func (b Binding) scope() string { return b.principal.scope() }
func (b Binding) member(id string) string {
	if b.principal.endUserDigest == "" && b.principal.delegationToken == "" && b.principal.taskCapability == "" {
		return id
	}
	return b.scope() + ":" + id
}

type Certification string

const (
	Certified Certification = "certified"
	Drifted   Certification = "drifted"
	Invalid   Certification = "invalid"
)

type Record struct {
	ID                string
	Binding           Binding
	Protocol          string
	ToolsetID         string
	ToolsetRevisionID string
	ReleaseID         string
	Certifications    map[string]Certification
	CreatedAt         time.Time
	LastActiveAt      time.Time
	Initialized       bool
}

func (r Record) Certification(serverID string) Certification {
	if r.Certifications == nil {
		return Invalid
	}
	state, ok := r.Certifications[serverID]
	if !ok {
		return Invalid
	}
	return state
}

func NewID() (string, error) {
	raw := make([]byte, sessionIDRandomSize)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func (b Binding) ProjectID() string { return b.projectID }
