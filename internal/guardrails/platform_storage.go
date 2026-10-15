package guardrails

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/tyk-swe/olp/internal/access"
)

// ReadRegistry runs only at publication/inspection. Serving receives the
// serialized result and never calls this function.
func ReadRegistry(ctx context.Context, q access.Queryer) (*Registry, error) {
	var unactivated bool
	if err := q.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM olp.guardrail_attachments a LEFT JOIN olp.guardrail_policy_revision_activations v ON v.revision_id=a.policy_revision_id WHERE v.revision_id IS NULL
 UNION ALL
 SELECT 1 FROM olp.guardrail_draft_attachments d LEFT JOIN olp.guardrail_policy_revision_activations v ON v.revision_id=d.policy_revision_id WHERE v.revision_id IS NULL
 UNION ALL
 SELECT 1 FROM olp.guardrail_policy_revisions p CROSS JOIN LATERAL jsonb_array_elements(p.entries) e LEFT JOIN olp.guardrail_revision_activations a ON a.revision_id=(e->>'revision_id')::uuid
 WHERE a.revision_id IS NULL AND (p.id IN(SELECT active_revision_id FROM olp.guardrail_policies) OR p.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))
 )`).Scan(&unactivated); err != nil {
		return nil, err
	}
	if unactivated {
		return nil, errors.New("activate referenced guardrail and policy revisions before publication")
	}
	r := &Registry{Definitions: map[string]Revision{}, Policies: map[string]PolicyRevision{}, ProjectOrganizations: map[string]string{}}
	rows, err := q.Query(ctx, `SELECT v.id::text,g.id::text,COALESCE(g.project_id::text,'installation'),g.name,g.type,v.configuration,v.timeout_ms,v.on_error,CASE WHEN g.managed_route_revision_id IS NOT NULL THEN v.configuration->'rules'->0->>'id' ELSE '' END
 FROM olp.guardrails g JOIN olp.guardrail_revisions v ON v.guardrail_id=g.id
 WHERE v.id=g.active_revision_id OR v.id IN (SELECT (entry->>'revision_id')::uuid FROM olp.guardrail_policy_revisions p,
 jsonb_array_elements(p.entries) entry WHERE p.id IN (SELECT active_revision_id FROM olp.guardrail_policies UNION SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments)
 OR p.policy_id IN (SELECT id FROM olp.guardrail_policies WHERE managed_route_revision_id IN (SELECT latest_revision_id FROM olp.routes)))`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var d Revision
		if err = rows.Scan(&d.ID, &d.GuardrailID, &d.ProjectID, &d.Name, &d.Type, &d.Configuration, &d.TimeoutMS, &d.OnError, &d.LegacyRuleID); err != nil {
			rows.Close()
			return nil, err
		}
		r.Definitions[d.ID] = d
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	for _, d := range r.Definitions {
		if d.Type != "plugin" {
			continue
		}
		var c ExternalConfig
		_ = json.Unmarshal(d.Configuration, &c)
		var approved bool
		if err = q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM olp.plugins WHERE digest=$1 AND approved_at IS NOT NULL AND module IS NOT NULL AND manifest->>'name'=$2 AND manifest->>'guardrail'='true')", c.PluginVersion, c.PluginID).Scan(&approved); err != nil {
			return nil, err
		}
		if !approved {
			return nil, errors.New("guardrail requires its approved confined plugin build")
		}
	}
	rows, err = q.Query(ctx, `SELECT v.id::text,p.id::text,COALESCE(p.project_id::text,'installation'),p.name,v.revision,v.entries FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE v.id=p.active_revision_id OR v.id IN (SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments)`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p PolicyRevision
		if err = rows.Scan(&p.ID, &p.PolicyID, &p.ProjectID, &p.Name, &p.Revision, &p.Entries); err != nil {
			rows.Close()
			return nil, err
		}
		r.Policies[p.ID] = p
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, `SELECT id::text,scope,COALESCE(scope_id::text,'installation'),policy_revision_id::text,position FROM olp.guardrail_attachments ORDER BY position,id`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Attachment
		if err = rows.Scan(&a.ID, &a.Scope, &a.ScopeID, &a.PolicyRevisionID, &a.Order); err != nil {
			rows.Close()
			return nil, err
		}
		r.Attachments = append(r.Attachments, a)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	// Converted route policies follow the route revision, rather than a mutable
	// attachment, so publishing or retiring a route cannot revive an old rule.
	rows, err = q.Query(ctx, `SELECT p.id::text,r.id::text,p.active_revision_id::text FROM olp.guardrail_policies p JOIN olp.routes r ON r.latest_revision_id=p.managed_route_revision_id WHERE r.state='active'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var a Attachment
		a.Scope = "route"
		if err = rows.Scan(&a.ID, &a.ScopeID, &a.PolicyRevisionID); err != nil {
			rows.Close()
			return nil, err
		}
		r.Attachments = append(r.Attachments, a)
		if r.MigratedRoutes == nil {
			r.MigratedRoutes = map[string]bool{}
		}
		r.MigratedRoutes[a.ScopeID] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows, err = q.Query(ctx, "SELECT id::text,organization_id::text FROM olp.projects WHERE organization_id IS NOT NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p, o string
		if err = rows.Scan(&p, &o); err != nil {
			rows.Close()
			return nil, err
		}
		r.ProjectOrganizations[p] = o
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	var raw string
	if err = q.QueryRow(ctx, "SELECT COALESCE((SELECT value FROM olp.settings WHERE key='guardrails.stream_window_bytes'),'65536')").Scan(&raw); err != nil {
		return nil, err
	}
	var number int
	if json.Unmarshal([]byte(raw), &number) == nil {
		r.WindowBytes = number
	} else {
		r.WindowBytes, _ = strconv.Atoi(raw)
	}
	if len(r.Definitions) == 0 && len(r.Policies) == 0 && len(r.Attachments) == 0 {
		return nil, nil
	}
	return r, r.Validate()
}
