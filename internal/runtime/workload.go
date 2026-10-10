package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/attribution"
	"github.com/tyk-swe/olp/internal/secrets"
	"github.com/tyk-swe/olp/internal/workload"
)

type workloadIssuer struct {
	ID, Creator, Revision string
	Config                workload.Config
	bindings              map[string]access.Authority
}

func (m *Manager) loadWorkloadAuthority(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	rows, e := tx.Query(ctx, "SELECT id::text,created_by::text,document,etag::text FROM olp.workload_issuers")
	if e != nil {
		return e
	}
	var issuers []workloadIssuer
	for rows.Next() {
		var issuer workloadIssuer
		var doc []byte
		if e = rows.Scan(&issuer.ID, &issuer.Creator, &doc, &issuer.Revision); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(doc, &issuer.Config); e != nil {
			rows.Close()
			return e
		}
		if e = issuer.Config.Validate(); e != nil {
			rows.Close()
			return e
		}
		issuers = append(issuers, issuer)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return e
	}
	state.workloadIssuers = map[string]workloadIssuer{}
	type projectPolicy struct {
		templates   access.LimitTemplates
		groups      access.RouteGroups
		users       *access.EndUserPolicy
		attribution attribution.Policy
	}
	projects := map[string]projectPolicy{}
	needed := map[string]bool{}
	ids := []string{}
	for _, issuer := range issuers {
		if !issuer.Config.Enabled {
			continue
		}
		for _, mapping := range issuer.Config.Mappings {
			if !needed[mapping.ProjectID] {
				needed[mapping.ProjectID] = true
				ids = append(ids, mapping.ProjectID)
			}
		}
	}
	if len(ids) > 0 {
		rows, err := tx.Query(ctx, "SELECT id::text,limit_templates,route_groups,end_user_policy,COALESCE(attribution_policy,'{}'::jsonb) FROM olp.projects WHERE id=ANY($1::uuid[])", ids)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			var p projectPolicy
			if err = rows.Scan(&id, &p.templates, &p.groups, &p.users, &p.attribution); err != nil {
				rows.Close()
				return err
			}
			projects[id] = p
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return err
		}
	}

	for _, issuer := range issuers {
		// Disabled trust must remain revocable even when its serving references
		// are unavailable. Only enabled issuers need compiled mapping authority.
		if !issuer.Config.Enabled {
			state.workloadIssuers[issuer.Config.Issuer] = issuer
			continue
		}
		issuer.bindings = map[string]access.Authority{}
		for _, mapping := range issuer.Config.Mappings {
			project := mapping.ProjectID
			a := access.Authority{Issuer: issuer.Creator, ProjectID: &project, InstallationID: m.installation, InstallationBudget: state.installationBudget, ProjectBudget: state.projectBudgets[project], ProjectAttributionBudgets: state.projectAttributionBudgets[project], BudgetIncreases: state.budgetIncreases, Policy: access.WorkloadKeyPolicy(mapping)}
			a.Policy = a.Policy.InRegion(m.Region)
			if org, ok := state.projectOrganizations[project]; ok {
				a.OrganizationID = &org
				a.OrganizationBudget = state.organizationBudgets[org]
			}
			p, exists := projects[project]
			if !exists {
				return fmt.Errorf("workload mapping project is unavailable")
			}
			a.ProjectEndUserPolicy = p.users
			a.ProjectAttributionPolicy = p.attribution
			if e = a.BindRouteGroups(p.groups); e != nil {
				return e
			}
			if e = a.BindLimitTemplates(p.templates); e != nil {
				return e
			}

			issuer.bindings[mapping.Name] = a
		}
		state.workloadIssuers[issuer.Config.Issuer] = issuer
	}
	return nil
}

func (m *Manager) authenticateWorkload(raw string) (*access.Authority, error) {
	token, e := workload.Parse(raw)
	if e != nil {
		return nil, ErrInvalidKey
	}
	m.mu.RLock()
	snapshot := m.authority
	m.mu.RUnlock()
	state := &snapshot
	issuer, ok := state.workloadIssuers[token.Issuer()]
	if !ok || !issuer.Config.Enabled {
		return nil, ErrInvalidKey
	}
	// Reject disabled algorithms/key IDs before any refresh request.
	if !token.Allowed(issuer.Config, time.Now()) {
		return nil, ErrInvalidKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	keys, e := m.workloadKeys.Keys(ctx, issuer.ID, issuer.Config.JWKSURL, token.KeyID(), time.Now())
	if e != nil {
		return nil, ErrStaleAuthority
	}
	identity, e := token.Verify(issuer.Config, keys, time.Now())
	if e != nil {
		return nil, ErrInvalidKey
	}
	pair, _ := json.Marshal([2]string{issuer.Config.Issuer, identity.Subject})
	digest := hex.EncodeToString(m.auth.Digest(secrets.WorkloadDigest, string(pair)))
	base, ok := issuer.bindings[identity.Mapping.Name]
	if !ok {
		return nil, ErrInvalidKey
	}
	m.mu.RLock()
	principal, known := state.workloadPrincipals[digest]
	m.mu.RUnlock()
	if !known {
		principal, e = m.registerWorkload(ctx, state, issuer, identity.Mapping, digest)
		if e != nil {
			return nil, e
		}
	}
	if principal.ProjectID == nil || *principal.ProjectID != identity.Mapping.ProjectID || principal.WorkloadMapping != identity.Mapping.Name || principal.WorkloadIssuerID == nil || *principal.WorkloadIssuerID != issuer.ID {
		return nil, ErrInvalidKey
	}
	base.ID = principal.ID
	base.LookupID = principal.LookupID
	base.LimitLookupID = principal.LimitLookupID
	base.RevokedAt = principal.RevokedAt
	base.WorkloadIssuerID = principal.WorkloadIssuerID
	base.WorkloadDigest = digest
	base.WorkloadMapping = identity.Mapping.Name
	base.WorkloadRevision = issuer.Revision
	base.ExpiresAt = &identity.ExpiresAt
	if identity.Mapping.EndUserClaim != nil {
		if !access.ValidEndUserIdentifier(identity.EndUser) {
			return nil, ErrInvalidKey
		}
		base.EndUserDigest = m.EndUserDigest(base.ProjectID, identity.EndUser)
	}
	if !base.AllowsEndUser(base.EndUserDigest) {
		return nil, ErrInvalidKey
	}
	m.mu.RLock()
	current := m.authority.id == state.id && time.Since(m.authority.readAt) <= AuthorityStaleAfter
	m.mu.RUnlock()
	if !current {
		return nil, ErrStaleAuthority
	}
	return &base, nil
}

// First sight of a verified subject registers only its digest and stable owner.
// The installation lock serializes enrollment with issuer/policy revocation.
func (m *Manager) registerWorkload(ctx context.Context, state *authorityState, issuer workloadIssuer, mapping workload.Mapping, digest string) (access.Authority, error) {
	m.workloadRegistration.Lock()
	defer m.workloadRegistration.Unlock()
	tx, e := m.pool.Begin(ctx)
	if e != nil {
		return access.Authority{}, ErrStaleAuthority
	}
	defer tx.Rollback(ctx)
	var current string
	if e = tx.QueryRow(ctx, "SELECT authority_id::text FROM olp.installation WHERE singleton FOR UPDATE").Scan(&current); e != nil || current != state.id {
		return access.Authority{}, ErrStaleAuthority
	}
	id := uuid.NewSHA1(uuid.MustParse("f6f67f12-d053-5d5a-8073-0b0b95eb61ce"), []byte(digest)).String()
	lookup := "wi_" + digest[:32]
	policy, _ := json.Marshal(access.WorkloadKeyPolicy(mapping))
	_, e = tx.Exec(ctx, `INSERT INTO olp.api_keys(id,lookup_id,digest,name,created_by,policy,etag,project_id,workload_issuer_id,workload_digest,workload_mapping)
 VALUES($1,$2,$3,$4,$5,$6,$1,$7,$8,$9,$10) ON CONFLICT(workload_digest) WHERE workload_digest IS NOT NULL DO NOTHING`, id, lookup, make([]byte, 32), "Workload "+digest[:12], issuer.Creator, policy, mapping.ProjectID, issuer.ID, digest, mapping.Name)
	if e != nil {
		return access.Authority{}, ErrStaleAuthority
	}
	var a access.Authority
	e = tx.QueryRow(ctx, "SELECT id::text,lookup_id,project_id::text,workload_issuer_id::text,workload_digest,workload_mapping,revoked_at FROM olp.api_keys WHERE workload_digest=$1", digest).Scan(&a.ID, &a.LookupID, &a.ProjectID, &a.WorkloadIssuerID, &a.WorkloadDigest, &a.WorkloadMapping, &a.RevokedAt)
	if e != nil {
		return access.Authority{}, ErrStaleAuthority
	}
	if e = tx.Commit(ctx); e != nil {
		return access.Authority{}, ErrStaleAuthority
	}
	// This cache entry carries revocation metadata only; permissions always come
	// from the current issuer mapping. It is discarded at authority refresh.
	m.mu.Lock()
	if m.authority.id == state.id {
		m.authority.workloadPrincipals[digest] = a
	}
	m.mu.Unlock()
	return a, nil
}
