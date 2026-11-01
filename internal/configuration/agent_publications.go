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
	"github.com/tyk-swe/olp/internal/agentcontract"
	"github.com/tyk-swe/olp/internal/agents"
)

// PublicationEntry promotes configuration, never runs, approvals, messages or
// billing records. Immutable destination pins are checked, not silently rebased.
type PublicationEntry struct {
	Project                string                         `json:"project"`
	Kind                   string                         `json:"kind"`
	Draft                  agentcontract.Draft            `json:"draft"`
	Published              *agentcontract.Draft           `json:"published,omitempty"`
	Labels                 map[string]agentcontract.Draft `json:"labels,omitempty"`
	CredentialRef          *string                        `json:"credential_ref,omitempty"`
	PublishedCredentialRef *string                        `json:"published_credential_ref,omitempty"`
}

func publicationKey(e PublicationEntry) string {
	// Names may contain delimiters; encode the complete identity as a tuple.
	key, _ := json.Marshal([3]string{strings.ToLower(e.Project), e.Kind, strings.ToLower(e.Draft.Name)})
	return string(key)
}
func publicationCredentialRef(e PublicationEntry) string {
	sum := sha256.Sum256([]byte(publicationKey(e)))
	return "agent/" + hex.EncodeToString(sum[:])
}

func (e PublicationEntry) definitions() []agentcontract.Draft {
	out := []agentcontract.Draft{e.Draft}
	if e.Published != nil {
		out = append(out, *e.Published)
	}
	for _, label := range []string{"production", "staging"} {
		if d, ok := e.Labels[label]; ok {
			out = append(out, d)
		}
	}
	return out
}

func (s *Server) validateAgentPublications(doc *Document) error {
	if len(doc.AgentPublications) > 512 {
		return access.Invalid("agent_publications", "Declare at most 512 publications.")
	}
	seen := map[string]bool{}
	for _, e := range doc.AgentPublications {
		key := publicationKey(e)
		if !guardrailLabel(e.Project) || seen[key] {
			return access.Invalid("agent_publications", "Use unique owning project/kind/name identities.")
		}
		seen[key] = true
		if e.Kind != "prompt" && len(e.Labels) != 0 {
			return access.Invalid("agent_publications.labels", "Only prompts have labels.")
		}
		for label := range e.Labels {
			if label != "production" && label != "staging" {
				return access.Invalid("agent_publications.labels", "Use production or staging.")
			}
		}
		if e.CredentialRef != nil && (e.Kind != "agent" || *e.CredentialRef != publicationCredentialRef(e)) {
			return access.Invalid("agent_publications.credential_ref", "Use the deterministic exported agent credential reference.")
		}
		if e.PublishedCredentialRef != nil {
			if e.Kind != "agent" || e.Published == nil || (*e.PublishedCredentialRef != publicationCredentialRef(e)+"/published" && (*e.PublishedCredentialRef != publicationCredentialRef(e) || e.CredentialRef == nil || e.Draft.Agent == nil || e.Published.Agent == nil || e.Draft.Agent.Endpoint != e.Published.Agent.Endpoint)) {
				return access.Invalid("agent_publications.published_credential_ref", "Bind published credentials independently; sharing requires the same approved endpoint.")
			}
		}
		for _, d := range e.definitions() {
			// Owning project IDs are rebound from the explicit project name.
			d.ProjectID = "00000000-0000-4000-8000-000000000001"
			if err := d.Validate(e.Kind); err != nil {
				return access.Invalid("agent_publications", err.Error())
			}
			if d.Agent != nil {
				if d.Agent.CredentialID != nil {
					return access.Invalid("agent_publications", "Export credential references, never destination secret UUIDs.")
				}
				if s.Egress == nil {
					return access.Invalid("agent_publications.endpoint", "Egress policy is unavailable.")
				}
				endpoint, err := s.Egress.ValidateEndpoint(d.Agent.Endpoint)
				if err != nil || (endpoint == nil || endpoint.String() != d.Agent.Endpoint) {
					return access.Invalid("agent_publications.endpoint", "Use a canonical approved endpoint.")
				}
			}
		}
	}
	return nil
}

func exportAgentPublications(ctx context.Context, q access.Queryer) ([]PublicationEntry, error) {
	rows, err := q.Query(ctx, `SELECT p.id::text,pr.name,p.kind,p.draft,v.definition FROM olp.agent_publications p JOIN olp.projects pr ON pr.id=p.project_id LEFT JOIN olp.agent_publication_revisions v ON v.id=p.latest_revision_id WHERE NOT p.retired ORDER BY p.kind,p.id`)
	if err != nil {
		return nil, err
	}
	type entry struct {
		id string
		e  PublicationEntry
	}
	var entries []entry
	for rows.Next() {
		var v entry
		var published []byte
		if err = rows.Scan(&v.id, &v.e.Project, &v.e.Kind, &v.e.Draft, &published); err != nil {
			rows.Close()
			return nil, err
		}
		if len(published) > 0 {
			var d agentcontract.Draft
			if err = json.Unmarshal(published, &d); err != nil {
				rows.Close()
				return nil, err
			}
			v.e.Published = &d
		}
		if v.e.Draft.Agent != nil && v.e.Draft.Agent.CredentialID != nil {
			ref := publicationCredentialRef(v.e)
			v.e.CredentialRef = &ref
		}
		if v.e.Published != nil && v.e.Published.Agent != nil && v.e.Published.Agent.CredentialID != nil {
			ref := publicationCredentialRef(v.e) + "/published"
			if v.e.CredentialRef != nil && v.e.Draft.Agent.CredentialID != nil && *v.e.Draft.Agent.CredentialID == *v.e.Published.Agent.CredentialID {
				ref = *v.e.CredentialRef
			}
			v.e.PublishedCredentialRef = &ref
		}
		entries = append(entries, v)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := make([]PublicationEntry, 0, len(entries))
	for _, v := range entries {
		if v.e.Kind == "prompt" {
			labels, err := q.Query(ctx, `SELECT l.label,v.definition FROM olp.prompt_labels l JOIN olp.agent_publication_revisions v ON v.id=l.revision_id WHERE l.publication_id=$1 ORDER BY l.label`, v.id)
			if err != nil {
				return nil, err
			}
			v.e.Labels = map[string]agentcontract.Draft{}
			for labels.Next() {
				var name string
				var d agentcontract.Draft
				if err = labels.Scan(&name, &d); err != nil {
					labels.Close()
					return nil, err
				}
				v.e.Labels[name] = d
			}
			err = labels.Err()
			labels.Close()
			if err != nil {
				return nil, err
			}
		}
		v.e.Draft = exportPublicationDraft(v.e.Draft)
		if v.e.Published != nil {
			d := exportPublicationDraft(*v.e.Published)
			v.e.Published = &d
		}
		for label, d := range v.e.Labels {
			v.e.Labels[label] = exportPublicationDraft(d)
		}
		out = append(out, v.e)
	}
	return out, nil
}

func exportPublicationDraft(d agentcontract.Draft) agentcontract.Draft {
	d = agentcontract.CloneRevision(agentcontract.Revision{Definition: d}).Definition
	d.ProjectID = ""
	if d.Agent != nil {
		d.Agent.CredentialID = nil
	}
	return d
}

func findPublication(ctx context.Context, q access.Queryer, e PublicationEntry) (*agents.Publication, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT p.id::text FROM olp.agent_publications p JOIN olp.projects pr ON pr.id=p.project_id WHERE NOT p.retired AND lower(pr.name)=lower($1) AND p.kind=$2 AND lower(p.name)=lower($3)`, e.Project, e.Kind, e.Draft.Name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return agents.Load(ctx, q, id, e.Kind, false)
}

func publicationDraft(p *agents.Publication) *agentcontract.Draft {
	if p == nil {
		return nil
	}
	return &p.Draft
}
func publishedDefinition(ctx context.Context, q access.Queryer, p *agents.Publication) (*agentcontract.Draft, error) {
	if p == nil || p.RevisionID == nil {
		return nil, nil
	}
	v, err := agents.LoadRevision(ctx, q, p.ID, *p.RevisionID)
	if err != nil {
		return nil, err
	}
	return &v.Definition, nil
}
func reusableAgentCredential(old, next *agentcontract.Draft) bool {
	if old == nil || next == nil || old.Agent == nil || next.Agent == nil || old.Agent.Endpoint != next.Agent.Endpoint || old.Agent.CredentialID == nil {
		return false
	}
	oldEntra := old.Agent.OAuth != nil && old.Agent.OAuth.EntraOBO != nil
	nextEntra := next.Agent.OAuth != nil && next.Agent.OAuth.EntraOBO != nil
	if oldEntra != nextEntra {
		return false
	}
	return !oldEntra || (old.Agent.OAuth.ClientID == next.Agent.OAuth.ClientID && old.Agent.OAuth.TokenEndpoint == next.Agent.OAuth.TokenEndpoint && old.Agent.OAuth.EntraOBO.Issuer == next.Agent.OAuth.EntraOBO.Issuer)
}

type publicationCredentialResolver struct {
	server   *Server
	tx       pgx.Tx
	project  string
	bindings bindingSet
	stored   map[string]*string
}

func (r *publicationCredentialResolver) bind(ctx context.Context, d agentcontract.Draft, ref *string, old *agentcontract.Draft) (agentcontract.Draft, error) {
	d = agentcontract.CloneRevision(agentcontract.Revision{Definition: d}).Definition
	d.ProjectID = r.project
	if d.Agent == nil {
		return d, nil
	}
	d.Agent.CredentialID = nil
	if ref == nil {
		return d, nil
	}
	if id, ok := r.stored[*ref]; ok {
		d.Agent.CredentialID = id
		return d, nil
	}
	if binding, ok := r.bindings[*ref]; ok {
		material := []byte(binding.secret)
		defer clear(material)
		if reusableAgentCredential(old, &d) {
			matched, err := agents.CredentialMatches(ctx, r.tx, r.server.Access.Keys, r.server.Access.Installation, r.project, *old.Agent.CredentialID, material)
			if err != nil {
				return d, err
			}
			if matched {
				d.Agent.CredentialID = old.Agent.CredentialID
				r.stored[*ref] = d.Agent.CredentialID
				return d, nil
			}
		}
		id, err := func() (string, error) {
			if d.Agent.OAuth != nil && d.Agent.OAuth.EntraOBO != nil {
				return agents.StoreEntraCredential(ctx, r.tx, r.server.Access.Keys, r.server.Access.Installation, r.project, material)
			}
			return agents.StoreCredential(ctx, r.tx, r.server.Access.Keys, r.server.Access.Installation, r.project, material)
		}()
		if err != nil {
			return d, err
		}
		d.Agent.CredentialID = &id
	} else if reusableAgentCredential(old, &d) {
		d.Agent.CredentialID = old.Agent.CredentialID
	} else {
		return d, access.Invalid("agent_publications.credential_ref", "Supply the required destination credential; endpoint changes cannot inherit it.")
	}
	r.stored[*ref] = d.Agent.CredentialID
	return d, nil
}

func (s *Server) planAgentPublications(ctx context.Context, q access.Queryer, doc *Document, bindings bindingSet, state *stateView, result *planResult) error {
	for _, e := range doc.AgentPublications {
		project := state.projects[strings.ToLower(e.Project)]
		declared := project != ""
		for _, p := range doc.Projects {
			declared = declared || strings.EqualFold(p.Name, e.Project)
		}
		if !declared {
			result.conflict("agent_publication", publicationKey(e), "The owning project is not declared or present.")
			continue
		}
		current, err := findPublication(ctx, q, e)
		if err != nil {
			return err
		}
		action := "create"
		if current != nil {
			action = "replace"
		}
		previous, err := publishedDefinition(ctx, q, current)
		if err != nil {
			return err
		}
		retained, err := retainedPublicationDefinitions(ctx, q, current)
		if err != nil {
			return err
		}
		retainedDraft := ""
		if current != nil {
			retainedDraft = exportedPublicationDigest(current.Draft)
		}
		for _, item := range []struct {
			ref     *string
			desired *agentcontract.Draft
			old     *agentcontract.Draft
		}{{e.CredentialRef, &e.Draft, publicationDraft(current)}, {e.PublishedCredentialRef, e.Published, previous}} {
			if item.ref == nil {
				continue
			}
			_, bound := bindings[*item.ref]
			if !bound && !reusableAgentCredential(item.old, item.desired) {
				result.blocker("agent_publication", publicationKey(e), "Supply the destination agent bearer credential binding for the declared definition.")
			}
		}

		{
			referenceProject := project
			if referenceProject == "" {
				referenceProject = "00000000-0000-4000-8000-000000000001"
			}
			for index, d := range e.definitions() {
				// Retaining local history does not restore serving eligibility.
				digest := exportedPublicationDigest(d)
				if (index == 0 && digest == retainedDraft) || (index > 0 && retained[digest]) {
					continue
				}
				d.ProjectID = referenceProject
				if err = agents.ValidateReferences(ctx, q, e.Kind, d); err != nil {
					var expected *access.Problem
					if errors.As(err, &expected) {
						result.blocker("agent_publication", publicationKey(e), "Rebind missing immutable destination references explicitly.")
						break
					}
					return err
				}
			}
		}
		if current != nil && reflect.DeepEqual(exportPublicationDraft(current.Draft), e.Draft) && e.Published == nil && current.RevisionID == nil && len(e.Labels) == 0 && len(bindings) == 0 {
			action = "reuse"
		}
		result.item("agent_publication", publicationKey(e), action, "Promote reviewed immutable configuration only; execution metadata and secrets remain local.")
	}
	return nil
}

func exportedPublicationDigest(d agentcontract.Draft) string {
	return agentcontract.DraftDigest(agentcontract.NormalizeDraft(exportPublicationDraft(d)))
}

func retainedPublicationDefinitions(ctx context.Context, q access.Queryer, p *agents.Publication) (map[string]bool, error) {
	retained := map[string]bool{}
	if p == nil {
		return retained, nil
	}
	rows, err := q.Query(ctx, `SELECT definition FROM olp.agent_publication_revisions WHERE publication_id=$1`, p.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var d agentcontract.Draft
		if err = rows.Scan(&d); err != nil {
			return nil, err
		}
		retained[exportedPublicationDigest(d)] = true
	}
	return retained, rows.Err()
}

func publishConfigurationDefinition(ctx context.Context, tx pgx.Tx, actor string, p *agents.Publication, d agentcontract.Draft) error {
	p.Draft = agentcontract.NormalizeDraft(d)
	var id string
	err := tx.QueryRow(ctx, `SELECT id::text FROM olp.agent_publication_revisions WHERE publication_id=$1 AND digest=$2`, p.ID, agentcontract.DraftDigest(p.Draft)).Scan(&id)
	if err == nil {
		// Immutable history survives revocation; live authority is checked separately.
		p.RevisionID = &id
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return agents.Publish(ctx, tx, actor, p)
}

func (s *Server) applyAgentPublications(ctx context.Context, tx pgx.Tx, p access.Principal, doc *Document, bindings bindingSet) error {
	entries := slices.Clone(doc.AgentPublications)
	// Toolset publications must exist before agent pins are checked.
	slices.SortStableFunc(entries, func(a, b PublicationEntry) int {
		if a.Kind == b.Kind {
			return strings.Compare(publicationKey(a), publicationKey(b))
		}
		if a.Kind == "toolset" {
			return -1
		}
		if b.Kind == "toolset" {
			return 1
		}
		return strings.Compare(a.Kind, b.Kind)
	})
	for _, e := range entries {
		var project string
		if err := tx.QueryRow(ctx, `SELECT id::text FROM olp.projects WHERE lower(name)=lower($1)`, e.Project).Scan(&project); err != nil {
			return err
		}
		if err := p.Project(&project, access.Change); err != nil {
			return err
		}
		current, err := findPublication(ctx, tx, e)
		if err != nil {
			return err
		}
		if current != nil {
			current, err = agents.Load(ctx, tx, current.ID, e.Kind, true)
			if err != nil {
				return err
			}
		}
		previous, err := publishedDefinition(ctx, tx, current)
		if err != nil {
			return err
		}
		resolver := publicationCredentialResolver{server: s, tx: tx, project: project, bindings: bindings, stored: map[string]*string{}}
		d, err := resolver.bind(ctx, e.Draft, e.CredentialRef, publicationDraft(current))
		if err != nil {
			return err
		}

		if err = agents.AuthorizeTrustChange(p, current, d); err != nil {
			return err
		}
		next := &agents.Publication{Kind: e.Kind, Draft: d}
		if err = agents.SaveDraft(ctx, tx, p.UserID(), current, next, s.Egress); err != nil {
			return err
		}
		var publishedID *string
		if e.Published != nil {
			v, err := resolver.bind(ctx, *e.Published, e.PublishedCredentialRef, previous)
			if err != nil {
				return err
			}
			// Published configuration can differ from the mutable draft. Trust must
			// be checked on that definition too, not only on the saved editor state.
			prior := current
			if previous != nil {
				prior = &agents.Publication{Draft: *previous}
			}
			if err = agents.AuthorizeTrustChange(p, prior, v); err != nil {
				return err
			}

			if err = publishConfigurationDefinition(ctx, tx, p.UserID(), next, v); err != nil {
				return err
			}
			id := *next.RevisionID
			publishedID = &id
		}
		if e.Kind == "prompt" {
			if _, err = tx.Exec(ctx, `DELETE FROM olp.prompt_labels WHERE publication_id=$1`, next.ID); err != nil {
				return err
			}
		}
		for _, label := range []string{"production", "staging"} {
			if v, ok := e.Labels[label]; ok {
				v.ProjectID = project
				if err = publishConfigurationDefinition(ctx, tx, p.UserID(), next, v); err != nil {
					return err
				}
				if err = agents.SetLabel(ctx, tx, next, label, *next.RevisionID, access.NewID()); err != nil {
					return err
				}
			}
		}
		// Preserve draft/publication separation after publishing label history.
		if _, err = tx.Exec(ctx, `UPDATE olp.agent_publications SET latest_revision_id=$2 WHERE id=$1`, next.ID, publishedID); err != nil {
			return err
		}
		if _, err = access.AdvanceAuthority(ctx, tx); err != nil {
			return err
		}
	}
	if len(entries) > 0 && s.Access.RuntimePublisher != nil {
		return s.Access.RuntimePublisher(ctx, tx, p.UserID())
	}
	return nil
}
