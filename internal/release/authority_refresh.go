package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/keyauthority"
	"github.com/tyk-swe/olp/internal/limits/ledger"
)

func (m *Manager) refreshAuthority(ctx context.Context) error {
	m.authorityRefresh.Lock()
	defer m.authorityRefresh.Unlock()
	start, err := m.authorityReadTime(ctx)
	if err != nil {
		return fmt.Errorf("authority replica freshness: %w", err)
	}
	tx, err := m.runtimeReadPool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	defer tx.Rollback(ctx)
	var id, installation string
	var sequence int64
	if err = tx.QueryRow(ctx, "SELECT id::text,COALESCE(authority_id::text,''),authority_sequence FROM olp.installation WHERE singleton").Scan(&installation, &id, &sequence); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	if installation != m.installation {
		return errors.New("runtime read database belongs to another installation")
	}
	m.mu.Lock()
	if m.authority.loaded && m.authority.id == id && sequence < m.authority.sequence {
		m.mu.Unlock()
		return ErrStaleAuthority
	}
	unchanged := m.authority.loaded && m.authority.id == id && m.authority.sequence == sequence
	if unchanged {
		m.authority.readAt = start
	}
	m.mu.Unlock()
	if unchanged {
		return nil
	}
	state := authorityState{loaded: true, readAt: start, id: id, sequence: sequence, keys: map[string]*keyRecord{}}
	if err = loadAuthorityBudgets(ctx, tx, &state); err != nil {
		return err
	}
	if err = m.loadAuthorityKeys(ctx, tx, &state); err != nil {
		return err
	}

	if err = m.loadWorkloadAuthority(ctx, tx, &state); err != nil {
		return err
	}
	if err = bindAuthorityRouteGroups(ctx, tx, &state); err != nil {
		return err
	}
	state.workloadPrincipals = map[string]keyauthority.Authority{}
	for _, record := range state.keys {
		if record.authority.WorkloadIssuerID != nil {
			state.workloadPrincipals[record.authority.WorkloadDigest] = record.authority
		}
	}
	if err = m.loadKeyOverlaps(ctx, tx, &state); err != nil {
		return fmt.Errorf("authority overlaps: %w", err)
	}
	if state.ineligible, err = ReadIneligible(ctx, tx, nil); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	state.agentPrincipals = map[string]keyauthority.Authority{}
	for _, record := range state.keys {
		state.agentPrincipals[record.authority.ID] = record.authority
	}
	if state.agentResources, err = loadAgentResourceAuthority(ctx, tx); err != nil {
		return err
	}
	if state.mcpTools, err = loadMCPToolAuthority(ctx, tx); err != nil {
		return err
	}
	m.mu.Lock()
	m.authority = state
	m.mu.Unlock()
	m.log.Info("authority refreshed", "sequence", sequence, "keys", len(state.keys), "ineligible_credentials", len(state.ineligible))
	return nil
}

func loadAuthorityBudgets(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	var err error
	if err = tx.QueryRow(ctx, "SELECT budget_policy FROM olp.installation WHERE singleton").Scan(&state.installationBudget); err != nil {
		return err
	}
	state.budgetIncreases, err = ledger.LoadBudgetIncreases(ctx, tx)
	if err != nil {
		return err
	}
	budgetRows, err := tx.Query(ctx, "SELECT p.id::text,p.budget_policy,p.attribution_budgets,p.organization_id::text,o.budget_policy FROM olp.projects p LEFT JOIN olp.organizations o ON o.id=p.organization_id WHERE p.organization_id IS NOT NULL OR p.budget_policy IS NOT NULL OR p.attribution_budgets<>'{}'::jsonb")
	if err != nil {
		return err
	}
	state.projectOrganizations = map[string]string{}
	state.organizationBudgets = map[string]*keyauthority.BudgetPolicy{}
	state.projectBudgets = map[string]*keyauthority.BudgetPolicy{}
	state.projectAttributionBudgets = map[string]keyauthority.AttributionBudgets{}
	for budgetRows.Next() {
		var project string
		var policy *keyauthority.BudgetPolicy
		var labels keyauthority.AttributionBudgets
		var organization *string
		var organizationPolicy *keyauthority.BudgetPolicy
		if err = budgetRows.Scan(&project, &policy, &labels, &organization, &organizationPolicy); err != nil {
			budgetRows.Close()
			return err
		}
		if organization != nil {
			state.projectOrganizations[project] = *organization
			state.organizationBudgets[*organization] = organizationPolicy
		}
		state.projectBudgets[project] = policy
		state.projectAttributionBudgets[project] = labels
	}
	budgetRows.Close()
	if err = budgetRows.Err(); err != nil {
		return err
	}
	return nil
}

func (m *Manager) loadAuthorityKeys(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	rows, err := tx.Query(ctx, "SELECT k.id::text,k.lookup_id,k.created_by::text,k.project_id::text,k.digest,k.policy,k.expires_at,k.revoked_at,k.budget_group_id::text,g.daily_cost_limit::text,g.monthly_cost_limit::text,g.weekly_cost_limit::text,p.end_user_policy,COALESCE(p.attribution_policy,'{}'::jsonb),COALESCE(k.limit_lookup_id,k.lookup_id),g.limit_template,g.requests_per_minute,g.tokens_per_minute,g.max_concurrency,COALESCE(g.cost_basis,'cost'),k.workload_issuer_id::text,COALESCE(k.workload_digest,''),COALESCE(k.workload_mapping,''),k.paused_at FROM olp.api_keys k LEFT JOIN olp.budget_groups g ON g.id=k.budget_group_id LEFT JOIN olp.projects p ON p.id=k.project_id")
	if err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	for rows.Next() {
		var policy []byte
		record := keyRecord{}
		if err = rows.Scan(&record.authority.ID, &record.authority.LookupID, &record.authority.Issuer, &record.authority.ProjectID, &record.digest, &policy, &record.authority.ExpiresAt, &record.authority.RevokedAt, &record.authority.BudgetGroupID, &record.authority.BudgetGroupDailyCostLimit, &record.authority.BudgetGroupMonthlyCostLimit, &record.authority.BudgetGroupWeeklyCostLimit, &record.authority.ProjectEndUserPolicy, &record.authority.ProjectAttributionPolicy, &record.authority.LimitLookupID, &record.authority.BudgetGroupTemplate, &record.authority.BudgetGroupRPM, &record.authority.BudgetGroupTPM, &record.authority.BudgetGroupConcurrency, &record.authority.BudgetGroupCostBasis, &record.authority.WorkloadIssuerID, &record.authority.WorkloadDigest, &record.authority.WorkloadMapping, &record.authority.PausedAt); err != nil {
			rows.Close()
			return fmt.Errorf("authority: %w", err)
		}
		if err = json.Unmarshal(policy, &record.authority.Policy); err != nil {
			rows.Close()
			return fmt.Errorf("authority: key %s policy: %w", record.authority.ID, err)
		}
		record.authority.SnapshotID = state.id
		record.authority.Policy = record.authority.Policy.InRegion(m.region)
		record.authority.BudgetIncreases = state.budgetIncreases
		record.authority.InstallationID = m.installation
		record.authority.InstallationBudget = state.installationBudget
		if record.authority.ProjectID != nil {
			if org, ok := state.projectOrganizations[*record.authority.ProjectID]; ok {
				record.authority.OrganizationID = &org
				record.authority.OrganizationBudget = state.organizationBudgets[org]
			}
			record.authority.ProjectBudget = state.projectBudgets[*record.authority.ProjectID]
			record.authority.ProjectAttributionBudgets = state.projectAttributionBudgets[*record.authority.ProjectID]
		}
		state.keys[record.authority.LookupID] = &record
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("authority: %w", err)
	}
	return nil
}

func bindAuthorityRouteGroups(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	// Fetch each needed project's group document once, rather than copying all
	// groups into every key row (including keys which never reference a group).
	var projectIDs []string
	var needed map[string]bool
	for _, record := range state.keys {
		a := record.authority
		if a.WorkloadIssuerID != nil {
			continue
		} // Workload bindings are compiled from current issuer policy.
		if (len(a.Policy.AllowedRouteGroups) == 0 && !a.UsesLimitTemplates()) || a.ProjectID == nil {
			continue
		}
		if needed == nil {
			needed = map[string]bool{}
		}
		if !needed[*a.ProjectID] {
			needed[*a.ProjectID] = true
			projectIDs = append(projectIDs, *a.ProjectID)
		}
	}
	if len(projectIDs) > 0 {
		groupRows, e := tx.Query(ctx, `SELECT id::text,route_groups,limit_templates FROM olp.projects WHERE id=ANY($1::uuid[])`, projectIDs)
		if e != nil {
			return fmt.Errorf("authority route groups: %w", e)
		}
		byProject := map[string]keyauthority.RouteGroups{}
		templatesByProject := map[string]keyauthority.LimitTemplates{}
		for groupRows.Next() {
			var id string
			var groups keyauthority.RouteGroups
			var templates keyauthority.LimitTemplates
			if e = groupRows.Scan(&id, &groups, &templates); e != nil {
				groupRows.Close()
				return e
			}
			byProject[id] = groups
			templatesByProject[id] = templates
		}
		groupRows.Close()
		if e = groupRows.Err(); e != nil {
			return e
		}
		for _, record := range state.keys {
			if record.authority.ProjectID == nil || record.authority.WorkloadIssuerID != nil {
				continue
			}
			if e = record.authority.BindRouteGroups(byProject[*record.authority.ProjectID]); e != nil {
				return fmt.Errorf("authority route groups: %w", e)
			}
			// A revoked key never admits, and its template may since have gone.
			if record.authority.RevokedAt != nil {
				continue
			}
			if e = record.authority.BindLimitTemplates(templatesByProject[*record.authority.ProjectID]); e != nil {
				return e
			}
		}
	}
	return nil
}

func (m *Manager) loadKeyOverlaps(ctx context.Context, tx pgx.Tx, state *authorityState) error {
	rows, err := tx.Query(ctx, "SELECT api_key_id::text,lookup_id,digest,expires_at FROM olp.api_key_overlaps WHERE expires_at>now()")
	if err != nil {
		return err
	}
	defer rows.Close()
	var byID map[string]*keyRecord
	for rows.Next() {
		var id, lookup string
		var digest []byte
		var until time.Time
		if err = rows.Scan(&id, &lookup, &digest, &until); err != nil {
			return err
		}
		if byID == nil {
			byID = make(map[string]*keyRecord, len(state.keys))
			for _, r := range state.keys {
				byID[r.authority.ID] = r
			}
		}
		if record, ok := byID[id]; ok {
			overlap := *record
			overlap.digest = digest
			overlap.expiresAt = &until
			overlap.authority.SecretOverlap = &keyauthority.SecretOverlap{LookupID: lookup, ExpiresAt: until}
			state.keys[lookup] = &overlap
		}
	}
	return rows.Err()
}
