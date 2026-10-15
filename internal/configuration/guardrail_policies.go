package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/contentpolicy"
	"github.com/tyk-swe/olp/internal/guardrails"
	"github.com/tyk-swe/olp/internal/routing"
	"github.com/tyk-swe/olp/internal/secrets"
)

type GuardrailPolicyRule struct {
	Guardrail string `json:"guardrail"`
	Phase     string `json:"phase"`
	Action    string `json:"action"`
	Monitor   bool   `json:"monitor,omitempty"`
	Mandatory bool   `json:"mandatory,omitempty"`
}
type GuardrailPolicyEntry struct {
	Name    string                `json:"name"`
	Project string                `json:"project"`
	Entries []GuardrailPolicyRule `json:"entries"`
}
type GuardrailAttachmentEntry struct {
	Scope         string `json:"scope"`
	Name          string `json:"name,omitempty"`
	Project       string `json:"project,omitempty"`
	Policy        string `json:"policy"`
	PolicyProject string `json:"policy_project"`
	Order         int    `json:"order"`
}

func portableKey(project, name string) [2]string {
	return [2]string{strings.ToLower(project), strings.ToLower(name)}
}
func boundedName(prefix string) string {
	if len(prefix) <= 85 {
		return prefix
	}
	sum := sha256.Sum256([]byte(prefix))
	return prefix[:64] + "-" + hex.EncodeToString(sum[:8])
}

func normalizeGuardrailVersion(doc *Document) error {
	if doc.APIVersion == LegacyAPIVersion {
		if len(doc.GuardrailPolicies) > 0 || len(doc.GuardrailAttachments) > 0 {
			return access.Invalid("guardrails", "Version 1 cannot mix new guardrail policies with legacy content policies.")
		}
		for _, d := range doc.Guardrails {
			if len(d.Configuration) > 0 {
				return access.Invalid("guardrails", "Version 1 cannot contain typed guardrail configuration.")
			}
		}
		if err := normalizeLegacyGuardrails(doc); err != nil {
			return err
		}
		doc.APIVersion = APIVersion
		return nil
	}
	for _, route := range doc.Routes {
		if len(route.ContentPolicy) > 0 && string(route.ContentPolicy) != "null" {
			return access.Invalid("content_policy", "Use version 2 guardrail definitions, policies and attachments.")
		}
	}
	for _, d := range doc.Guardrails {
		if d.Policy != nil {
			return access.Invalid("guardrails", "Use typed configuration in version 2.")
		}
	}
	return nil
}
func normalizeLegacyGuardrails(doc *Document) error {
	for i := range doc.Guardrails {
		d := &doc.Guardrails[i]
		if d.Policy == nil {
			continue
		}
		if err := contentpolicy.Validate(d.Policy); err != nil {
			return err
		}
		patterns := []map[string]any{}
		for _, rule := range d.Policy.Rules {
			patterns = append(patterns, map[string]any{"id": rule.ID, "pattern": rule.Pattern, "replacement": rule.Replacement})
		}
		d.Configuration, _ = json.Marshal(map[string]any{"rules": patterns})
		d.Policy = nil
		d.TimeoutMS = 1000
		d.OnError = guardrails.FailClosed
	}
	for i := range doc.Routes {
		route := &doc.Routes[i]
		if len(route.ContentPolicy) == 0 || string(route.ContentPolicy) == "null" {
			route.ContentPolicy = nil
			continue
		}
		policy, err := contentpolicy.Decode(route.ContentPolicy)
		if err != nil {
			return access.Invalid("content_policy", "Invalid legacy content policy.")
		}
		project := ""
		if route.Project != nil {
			project = *route.Project
		}
		name := boundedName("route." + route.Slug + ".content")
		p := GuardrailPolicyEntry{Name: name, Project: project, Entries: []GuardrailPolicyRule{}}
		for _, rule := range policy.Rules {
			definition := boundedName(name + "." + rule.ID)
			config, _ := json.Marshal(map[string]any{"rules": []any{map[string]any{"id": rule.ID, "pattern": rule.Pattern, "replacement": rule.Replacement}}})
			doc.Guardrails = append(doc.Guardrails, GuardrailEntry{Name: definition, Project: project, Type: "builtin.regex", Configuration: config, TimeoutMS: int64(min(1000, route.OverallTimeoutMS)), OnError: guardrails.FailClosed})
			p.Entries = append(p.Entries, GuardrailPolicyRule{Guardrail: definition, Phase: rule.Phase, Action: rule.Action, Mandatory: true})
		}
		if len(p.Entries) > 0 {
			doc.GuardrailPolicies = append(doc.GuardrailPolicies, p)
			doc.GuardrailAttachments = append(doc.GuardrailAttachments, GuardrailAttachmentEntry{Scope: "route", Name: route.Slug, Project: project, Policy: p.Name, PolicyProject: project, Order: 0})
		}
		route.ContentPolicy = nil
	}
	return nil
}

func validateGuardrailPolicies(doc *Document) error {
	definitions := map[[2]string]GuardrailEntry{}
	for _, d := range doc.Guardrails {
		definitions[portableKey(d.Project, d.Name)] = d
	}
	policies := map[[2]string]GuardrailPolicyEntry{}
	if len(doc.GuardrailPolicies) > 1000 || len(doc.GuardrailAttachments) > 10000 {
		return access.Invalid("guardrail_policies", "Too many policies or attachments.")
	}
	for _, p := range doc.GuardrailPolicies {
		key := portableKey(p.Project, p.Name)
		if _, ok := policies[key]; ok || !guardrailLabel(p.Name) || len(p.Entries) > guardrails.MaxEntries {
			return access.Invalid("guardrail_policies", "Use unique bounded policy names and ordered entries.")
		}
		entries := []guardrails.Entry{}
		revisions := map[string]guardrails.Revision{}
		for _, entry := range p.Entries {
			d, ok := definitions[portableKey(p.Project, entry.Guardrail)]
			if !ok {
				return access.Invalid("guardrail_policies", "Declare every referenced guardrail in the policy's project.")
			}
			rid := d.Name
			revisions[rid] = guardrails.Revision{ID: rid, GuardrailID: rid, Type: d.Type, Configuration: d.Configuration, TimeoutMS: d.TimeoutMS, OnError: d.OnError}
			entries = append(entries, guardrails.Entry{GuardrailID: rid, RevisionID: rid, Phase: entry.Phase, Action: entry.Action, Monitor: entry.Monitor, Mandatory: entry.Mandatory})
		}
		engine, err := guardrails.Compile(entries, revisions, nil)
		if err != nil {
			return access.Invalid("guardrail_policies", "Invalid guardrail entries.")
		}
		for _, a := range doc.GuardrailAttachments {
			if portableKey(a.PolicyProject, a.Policy) != key {
				continue
			}
			for _, route := range doc.Routes {
				project := ""
				if route.Project != nil {
					project = *route.Project
				}
				applies := a.Scope == "installation" || (a.Scope == "project" && strings.EqualFold(a.Name, project)) || (a.Scope == "route" && a.Name == route.Slug && strings.EqualFold(a.PolicyProject, project)) || (a.Scope == "api-key" && strings.EqualFold(a.Project, project))
				if a.Scope == "organization" {
					for _, declared := range doc.Projects {
						if strings.EqualFold(declared.Name, project) && declared.Organization != nil && strings.EqualFold(*declared.Organization, a.Name) {
							applies = true
						}
					}
				}
				if applies {
					f, fidelityErr := routing.DecodeFidelity(route.Fidelity)
					if fidelityErr != nil {
						continue // Route validation reports the invalid fidelity field.
					}
					if err = engine.ValidateRoute(f.Strict(), int64(route.OverallTimeoutMS)); err != nil {
						return access.Invalid("guardrail_policies", "Guardrails are incompatible with an attached route's fidelity or deadline.")
					}
				}
			}
		}
		policies[key] = p
	}
	for _, a := range doc.GuardrailAttachments {
		if _, ok := policies[portableKey(a.PolicyProject, a.Policy)]; !ok || a.Order < 0 || !slices.Contains([]string{"installation", "organization", "project", "route", "api-key"}, a.Scope) {
			return access.Invalid("guardrail_attachments", "Choose a declared policy and a valid additive attachment.")
		}
	}
	return nil
}

func planGuardrailPolicies(ctx context.Context, q access.Queryer, doc *Document, state *stateView, result *planResult) error {
	if len(doc.GuardrailPolicies) == 0 && len(doc.GuardrailAttachments) == 0 {
		return nil
	}
	current := &Document{}
	var err error
	current.Guardrails, err = exportGuardrails(ctx, q)
	if err != nil {
		return err
	}
	if err = exportGuardrailPlatform(ctx, q, current); err != nil {
		return err
	}
	definitions := map[[2]string]GuardrailEntry{}
	for _, d := range current.Guardrails {
		definitions[portableKey(d.Project, d.Name)] = d
	}
	changed := map[[2]string]bool{}
	for _, p := range doc.GuardrailPolicies {
		key := portableKey(p.Project, p.Name)
		action := "create"
		for _, old := range current.GuardrailPolicies {
			if portableKey(old.Project, old.Name) != key {
				continue
			}
			action = "reuse"
			if !reflect.DeepEqual(old.Entries, p.Entries) {
				action = "revision"
			}
			for _, e := range p.Entries {
				for _, d := range doc.Guardrails {
					if portableKey(d.Project, d.Name) != portableKey(p.Project, e.Guardrail) {
						continue
					}
					old, ok := definitions[portableKey(d.Project, d.Name)]
					if !ok || old.Type != d.Type || !guardrailEqual(&guardrails.Definition{Configuration: old.Configuration, TimeoutMS: old.TimeoutMS, OnError: old.OnError}, d) {
						action = "revision"
					}
				}
			}
			break
		}
		changed[key] = action != "reuse"
		result.item("guardrail_policy", p.Project+"/"+p.Name, action, "Pin the ordered entries to immutable definition revisions.")
	}
	for _, a := range doc.GuardrailAttachments {
		action := "create"
		for _, old := range current.GuardrailAttachments {
			if old.Scope != a.Scope || !strings.EqualFold(old.Name, a.Name) || !strings.EqualFold(old.Project, a.Project) || old.Order != a.Order {
				continue
			}
			if portableKey(old.PolicyProject, old.Policy) == portableKey(a.PolicyProject, a.Policy) {
				action = "reuse"
				if changed[portableKey(a.PolicyProject, a.Policy)] {
					action = "revision"
				}
			} else {
				result.conflict("guardrail_attachment", a.Scope+"/"+a.Name, "An additive attachment cannot replace a different policy at this position.")
				action = ""
			}
			break
		}
		if action != "" {
			result.item("guardrail_attachment", a.Scope+"/"+a.Name+"/"+a.Policy, action, "Add this policy without removing inherited guardrails.")
		}
	}
	return nil
}

func projectIDByName(ctx context.Context, q access.Queryer, name string) (*string, error) {
	if name == "" {
		return nil, nil
	}
	var id string
	err := q.QueryRow(ctx, "SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)", name).Scan(&id)
	return &id, err
}
func applyGuardrailPolicies(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document) error {
	for _, policy := range doc.GuardrailPolicies {
		project, err := projectIDByName(ctx, tx, policy.Project)
		if err != nil {
			return err
		}
		if err = p.Project(project, access.Change); err != nil {
			return err
		}
		entries := []guardrails.Entry{}
		for _, entry := range policy.Entries {
			var gid, rid string
			err = tx.QueryRow(ctx, "SELECT id::text,active_revision_id::text FROM olp.guardrails WHERE project_id IS NOT DISTINCT FROM $1::uuid AND lower(name)=lower($2) AND retired_at IS NULL", project, entry.Guardrail).Scan(&gid, &rid)
			if err != nil {
				return err
			}
			entries = append(entries, guardrails.Entry{GuardrailID: gid, RevisionID: rid, Phase: entry.Phase, Action: entry.Action, Monitor: entry.Monitor, Mandatory: entry.Mandatory})
		}
		var id, rid string
		var active *string
		var version int64
		var current []guardrails.Entry
		err = tx.QueryRow(ctx, `SELECT p.id::text,v.id::text,v.revision,v.entries,p.active_revision_id::text FROM olp.guardrail_policies p JOIN olp.guardrail_policy_revisions v ON v.id=p.latest_revision_id WHERE p.project_id IS NOT DISTINCT FROM $1::uuid AND lower(p.name)=lower($2) AND p.retired_at IS NULL`, project, policy.Name).Scan(&id, &rid, &version, &current, &active)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err != nil {
			id, rid, version = access.NewID(), access.NewID(), 1
			_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policies(id,project_id,name,latest_revision_id,active_revision_id,etag,created_by) VALUES($1,$2,$3,$4,$4,$5,$6)", id, project, policy.Name, rid, access.NewID(), p.UserID())
		} else if reflect.DeepEqual(entries, current) {
			if active != nil && *active == rid {
				continue
			}
			if _, err = tx.Exec(ctx, "UPDATE olp.guardrail_policies SET active_revision_id=$2,etag=$3 WHERE id=$1", id, rid, access.NewID()); err != nil {
				return err
			}
			if _, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", rid, p.UserID()); err != nil {
				return err
			}
			continue
		} else {
			rid = access.NewID()
			version++
			_, err = tx.Exec(ctx, "UPDATE olp.guardrail_policies SET latest_revision_id=$2,active_revision_id=$2,etag=$3 WHERE id=$1", id, rid, access.NewID())
		}
		if err != nil {
			return err
		}
		raw, _ := json.Marshal(entries)
		if _, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revisions(id,policy_id,revision,entries,created_by) VALUES($1,$2,$3,$4,$5)", rid, id, version, raw, p.UserID()); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_policy_revision_activations(revision_id,activated_by) VALUES($1,$2) ON CONFLICT DO NOTHING", rid, p.UserID()); err != nil {
			return err
		}
	}
	for _, a := range doc.GuardrailAttachments {
		project, err := projectIDByName(ctx, tx, a.PolicyProject)
		if err != nil {
			return err
		}
		var rid string
		if err = tx.QueryRow(ctx, "SELECT active_revision_id::text FROM olp.guardrail_policies WHERE project_id IS NOT DISTINCT FROM $1::uuid AND lower(name)=lower($2) AND retired_at IS NULL", project, a.Policy).Scan(&rid); err != nil {
			return err
		}
		var scopeID *string
		draftAttachment := false
		switch a.Scope {
		case "installation":
			if !p.AllProjects {
				return access.Forbidden()
			}
		case "organization":
			var id string
			if err = tx.QueryRow(ctx, "SELECT id::text FROM olp.organizations WHERE lower(name)=lower($1)", a.Name).Scan(&id); err != nil {
				return err
			}
			if err = p.Organization(id, access.Change); err != nil {
				return err
			}
			scopeID = &id
		case "project":
			scopeID, err = projectIDByName(ctx, tx, a.Name)
		case "route":
			var id string
			var owner *string
			err = tx.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.route_drafts WHERE slug=$1 AND project_id IS NOT DISTINCT FROM $2::uuid ORDER BY updated_at DESC LIMIT 1", a.Name, project).Scan(&id, &owner)
			if err == nil {
				draftAttachment = true
			} else if errors.Is(err, pgx.ErrNoRows) {
				err = tx.QueryRow(ctx, "SELECT id::text,project_id::text FROM olp.routes WHERE slug=$1", a.Name).Scan(&id, &owner)
			}
			if err == nil && !reflect.DeepEqual(owner, project) {
				return access.Invalid("guardrail_attachments", "Route and policy must belong to the same project.")
			}
			scopeID = &id
		case "api-key":
			var id string
			keyProject, e := projectIDByName(ctx, tx, a.Project)
			if e != nil {
				return e
			}
			rows, e := tx.Query(ctx, "SELECT id::text FROM olp.api_keys WHERE project_id IS NOT DISTINCT FROM $1::uuid AND name=$2 AND revoked_at IS NULL", keyProject, a.Name)
			if e != nil {
				return e
			}
			ids, e := pgx.CollectRows(rows, pgx.RowTo[string])
			if e != nil {
				return e
			}
			if len(ids) != 1 || !reflect.DeepEqual(keyProject, project) {
				return access.Invalid("guardrail_attachments", "API key name must resolve uniquely in the policy project.")
			}
			id = ids[0]
			scopeID = &id
		}
		if err != nil {
			return err
		}
		if err = p.Project(project, access.Change); err != nil {
			return err
		}
		if draftAttachment {
			if err = stageGuardrailAttachment(ctx, tx, *scopeID, rid, a.Order, p.UserID()); err != nil {
				return err
			}
			continue
		}
		if err = guardrails.AuthorizeAttachment(ctx, tx, p, guardrails.Attachment{Scope: a.Scope, ScopeID: stringValue(scopeID)}, access.Change); err != nil {
			return err
		}
		var current string
		err = tx.QueryRow(ctx, "SELECT policy_revision_id::text FROM olp.guardrail_attachments WHERE scope=$1 AND scope_id IS NOT DISTINCT FROM $2::uuid AND position=$3", a.Scope, scopeID, a.Order).Scan(&current)
		if err == nil && current == rid {
			continue
		}
		if err == nil {
			var same bool
			if err = tx.QueryRow(ctx, "SELECT a.policy_id=b.policy_id FROM olp.guardrail_policy_revisions a,olp.guardrail_policy_revisions b WHERE a.id=$1 AND b.id=$2", current, rid).Scan(&same); err != nil {
				return err
			}
			if !same {
				return access.Invalid("guardrail_attachments", "An additive attachment cannot replace a different policy at this position.")
			}
			if _, err = tx.Exec(ctx, "UPDATE olp.guardrail_attachments SET policy_revision_id=$4,etag=$5 WHERE scope=$1 AND scope_id IS NOT DISTINCT FROM $2::uuid AND position=$3", a.Scope, scopeID, a.Order, rid, access.NewID()); err != nil {
				return err
			}
			continue
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if _, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_attachments(id,scope,scope_id,policy_revision_id,position,etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7)", access.NewID(), a.Scope, scopeID, rid, a.Order, access.NewID(), p.UserID()); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) bindGuardrailCredentials(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, bindings bindingSet) error {
	for i := range doc.Guardrails {
		d := &doc.Guardrails[i]
		if strings.HasPrefix(d.Type, "builtin.") {
			continue
		}
		var c guardrails.ExternalConfig
		if json.Unmarshal(d.Configuration, &c) != nil {
			return access.Invalid("configuration", "Invalid guardrail configuration.")
		}
		project, err := projectIDByName(ctx, tx, d.Project)
		if err != nil {
			return err
		}
		refs := []*string{&c.CredentialRef}
		if c.Network != nil {
			refs = append(refs, &c.Network.CredentialID)
		}
		for _, ref := range refs {
			if *ref == "" {
				continue
			}
			var id string
			err = tx.QueryRow(ctx, "SELECT id::text FROM olp.guardrail_credentials WHERE project_id=$1 AND name=$2", project, *ref).Scan(&id)
			if errors.Is(err, pgx.ErrNoRows) {
				binding, ok := bindings[*ref]
				if !ok || binding.secret == "" || binding.reference != nil {
					return access.Invalid("secret_bindings", "Bind every new guardrail credential to a sealed JSON credential.")
				}
				id = access.NewID()
				if err = s.Access.Keys.Store(ctx, tx, s.Access.Installation, id, secrets.GuardrailCredential, []byte(binding.secret), nil); err != nil {
					return err
				}
				if _, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_credentials(id,project_id,name,created_by) VALUES($1,$2,$3,$4)", id, project, *ref, p.UserID()); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			*ref = id
		}
		d.Configuration, _ = json.Marshal(c)
	}
	return nil
}

func stringValue(value *string) string {
	if value == nil {
		return "installation"
	}
	return *value
}
func stageGuardrailAttachment(ctx context.Context, tx pgx.Tx, draftID, revisionID string, position int, actor string) error {
	var current string
	err := tx.QueryRow(ctx, "SELECT policy_revision_id::text FROM olp.guardrail_draft_attachments WHERE draft_id=$1 AND position=$2", draftID, position).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if current != "" && current != revisionID {
		var same bool
		if err = tx.QueryRow(ctx, "SELECT a.policy_id=b.policy_id FROM olp.guardrail_policy_revisions a,olp.guardrail_policy_revisions b WHERE a.id=$1 AND b.id=$2", current, revisionID).Scan(&same); err != nil {
			return err
		}
		if !same {
			return access.Invalid("guardrail_attachments", "A draft attachment cannot remove a different policy at this position.")
		}
	}
	_, err = tx.Exec(ctx, "INSERT INTO olp.guardrail_draft_attachments(draft_id,policy_revision_id,position,created_by) VALUES($1,$2,$3,$4) ON CONFLICT(draft_id,position) DO UPDATE SET policy_revision_id=excluded.policy_revision_id", draftID, revisionID, position, actor)
	return err
}
