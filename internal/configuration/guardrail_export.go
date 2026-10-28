package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails"
)

func exportGuardrailPlatform(ctx context.Context, q access.Queryer, doc *Document) error {
	registry, err := guardrails.ReadRegistry(ctx, q)
	if err != nil {
		return err
	}
	if registry == nil {
		return normalizeLegacyGuardrails(doc)
	}
	projects := map[string]string{"installation": ""}
	rows, err := q.Query(ctx, "SELECT id::text,name FROM olp.projects")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		projects[id] = name
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	definitions := map[string]string{}
	rows, err = q.Query(ctx, "SELECT active_revision_id::text,name FROM olp.guardrails WHERE retired_at IS NULL AND managed_route_revision_id IS NULL AND active_revision_id IS NOT NULL")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		definitions[id] = name
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	policies := map[string]string{}
	rows, err = q.Query(ctx, `SELECT v.id::text,p.name,(v.id=p.active_revision_id) FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.policy_id=p.id
 WHERE p.managed_route_revision_id IS NULL AND (v.id=p.active_revision_id OR v.id IN(SELECT policy_revision_id FROM olp.guardrail_attachments UNION SELECT policy_revision_id FROM olp.guardrail_draft_attachments))`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id, name string
		var active bool
		if err = rows.Scan(&id, &name, &active); err != nil {
			rows.Close()
			return err
		}
		if !active {
			sum := sha256.Sum256(policyPortableContent(registry, id))
			name = boundedName(name + ".revision." + hex.EncodeToString(sum[:6]))
		}
		policies[id] = name
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for id, name := range policies {
		p := registry.Policies[id]
		entry := GuardrailPolicyEntry{Name: name, Project: projects[p.ProjectID], Entries: []GuardrailPolicyRule{}}
		for _, e := range p.Entries {
			d := registry.Definitions[e.RevisionID]
			definition, ok := definitions[e.RevisionID]
			if !ok {
				sum := sha256.Sum256(append([]byte(d.Type+d.OnError+strconv.FormatInt(d.TimeoutMS, 10)), d.Configuration...))
				definition = boundedName(d.Name + ".revision." + hex.EncodeToString(sum[:6]))
				definitions[d.ID] = definition
				doc.Guardrails = append(doc.Guardrails, GuardrailEntry{Name: definition, Project: projects[d.ProjectID], Type: d.Type, Configuration: d.Configuration, TimeoutMS: d.TimeoutMS, OnError: d.OnError})
			}
			entry.Entries = append(entry.Entries, GuardrailPolicyRule{Guardrail: definition, Phase: e.Phase, Action: e.Action, Monitor: e.Monitor, Mandatory: e.Mandatory})
		}
		doc.GuardrailPolicies = append(doc.GuardrailPolicies, entry)
	}
	for _, a := range registry.Attachments {
		name, ok := policies[a.PolicyRevisionID]
		if !ok {
			continue
		}
		p := registry.Policies[a.PolicyRevisionID]
		entry := GuardrailAttachmentEntry{Scope: a.Scope, Policy: name, PolicyProject: projects[p.ProjectID], Order: a.Order}
		switch a.Scope {
		case "installation":
		case "project":
			entry.Name = projects[a.ScopeID]
		case "organization":
			err = q.QueryRow(ctx, "SELECT name FROM olp.organizations WHERE id=$1", a.ScopeID).Scan(&entry.Name)
		case "route":
			err = q.QueryRow(ctx, "SELECT slug FROM olp.routes WHERE id=$1", a.ScopeID).Scan(&entry.Name)
		case "toolset":
			err = q.QueryRow(ctx, "SELECT name FROM olp.agent_publications WHERE id=$1 AND kind='toolset'", a.ScopeID).Scan(&entry.Name)
		case "api-key":
			var project string
			err = q.QueryRow(ctx, "SELECT name,COALESCE(project_id::text,'installation') FROM olp.api_keys WHERE id=$1", a.ScopeID).Scan(&entry.Name, &project)
			entry.Project = projects[project]
		}
		if err != nil {
			return err
		}
		doc.GuardrailAttachments = append(doc.GuardrailAttachments, entry)
	}
	// Staged route policy revisions supersede the same exported route slot.
	// They stay staged at the destination until its explicit route publication.
	rows, err = q.Query(ctx, "SELECT d.slug,a.policy_revision_id::text,a.position FROM olp.guardrail_draft_attachments a JOIN olp.route_drafts d ON d.id=a.draft_id ORDER BY d.updated_at,a.position")
	if err != nil {
		return err
	}
	for rows.Next() {
		var slug, revision string
		var position int
		if err = rows.Scan(&slug, &revision, &position); err != nil {
			rows.Close()
			return err
		}
		name, ok := policies[revision]
		if !ok {
			rows.Close()
			return access.Invalid("guardrails", "A staged policy is unavailable.")
		}
		p := registry.Policies[revision]
		entry := GuardrailAttachmentEntry{Scope: "route", Name: slug, Policy: name, PolicyProject: projects[p.ProjectID], Order: position}
		found := false
		for i, current := range doc.GuardrailAttachments {
			if current.Scope == "route" && current.Name == slug && current.Order == position {
				doc.GuardrailAttachments[i] = entry
				found = true
				break
			}
		}
		if !found {
			doc.GuardrailAttachments = append(doc.GuardrailAttachments, entry)
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for i := range doc.Guardrails {
		d := &doc.Guardrails[i]
		if strings.HasPrefix(d.Type, "builtin.") {
			continue
		}
		var c guardrails.ExternalConfig
		if json.Unmarshal(d.Configuration, &c) != nil {
			return access.Invalid("guardrails", "Invalid published configuration.")
		}
		refs := []*string{&c.CredentialRef}
		if c.Network != nil {
			refs = append(refs, &c.Network.CredentialID)
		}
		for _, ref := range refs {
			if *ref != "" {
				var name string
				if err = q.QueryRow(ctx, "SELECT name FROM olp.guardrail_credentials WHERE id=$1", *ref).Scan(&name); err != nil {
					return err
				}
				*ref = name
			}
		}
		d.Configuration, _ = json.Marshal(c)
	}
	return normalizeLegacyGuardrails(doc)
}

func policyPortableContent(registry *guardrails.Registry, id string) []byte {
	type portableRule struct {
		Type                   string
		Config                 json.RawMessage
		Timeout                int64
		OnError, Phase, Action string
		Monitor, Mandatory     bool
	}
	values := []portableRule{}
	for _, entry := range registry.Policies[id].Entries {
		d := registry.Definitions[entry.RevisionID]
		values = append(values, portableRule{d.Type, d.Configuration, d.TimeoutMS, d.OnError, entry.Phase, entry.Action, entry.Monitor, entry.Mandatory})
	}
	raw, _ := json.Marshal(values)
	return raw
}
