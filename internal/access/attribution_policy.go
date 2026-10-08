package access

import (
	"bytes"
	"encoding/json"
	"maps"
	"net/http"

	"github.com/tyk-swe/olp/internal/attribution"
)

func (s *Server) projectAttributionPolicy(r *http.Request, p Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, View); err != nil {
		return Reply{}, err
	}
	var policy json.RawMessage
	var etag string
	if err = s.Pool.QueryRow(r.Context(), "SELECT COALESCE(attribution_policy,'null'::jsonb),etag::text FROM olp.projects WHERE id=$1", id).Scan(&policy, &etag); err != nil {
		return Reply{}, err
	}
	return Detail(map[string]any{"policy": policy, "etag": etag}, etag), nil
}

func (s *Server) putProjectAttributionPolicy(r *http.Request, _ Principal) (Reply, error) {
	id, err := IDParam(r, "project_id")
	if err != nil {
		return Reply{}, err
	}
	var body struct {
		Policy json.RawMessage `json:"policy"`
	}
	if err = Decode(r, &body); err != nil {
		return Reply{}, err
	}
	if len(body.Policy) == 0 {
		return Reply{}, Invalid("policy", "A policy or explicit null is required.")
	}
	var input struct{ Policy *attribution.Policy }
	decoder := json.NewDecoder(bytes.NewReader(body.Policy))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&input.Policy); err != nil {
		return Reply{}, Invalid("policy", "The attribution policy is invalid.")
	}
	if err = validateAttributionPolicy(input.Policy); err != nil {
		return Reply{}, err
	}
	tx, err := s.Begin(r)
	if err != nil {
		return Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := s.Reauthorize(r, tx)
	if err != nil {
		return Reply{}, err
	}
	if err = p.Project(&id, Change); err != nil {
		return Reply{}, err
	}
	etag, err := loadProject(r, tx, id)
	if err != nil {
		return Reply{}, err
	}
	if err = Match(r, etag); err != nil {
		return Reply{}, err
	}
	encoded, err := json.Marshal(input.Policy)
	if err != nil {
		return Reply{}, err
	}
	etag = NewID()
	if _, err = tx.Exec(r.Context(), "UPDATE olp.projects SET attribution_policy=NULLIF($2::jsonb,'null'::jsonb),etag=$3,updated_at=now() WHERE id=$1", id, encoded, etag); err != nil {
		return Reply{}, err
	}
	if _, err = AdvanceAuthority(r.Context(), tx); err != nil {
		return Reply{}, err
	}
	if err = Audit(r.Context(), tx, r, p.Actor(), "project.attribution_policy.update", "project", id, "success"); err != nil {
		return Reply{}, err
	}
	return Commit(r, tx, Detail(map[string]any{"policy": input.Policy, "etag": etag}, etag))
}

func validateAttributionPolicy(p *attribution.Policy) error {
	if p == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return Invalid("attribution_policy", "Use at most four distinct machine-token keys, unique requirements, and valid pinned values.")
	}
	return nil
}
func (p KeyPolicy) AttributionPolicy() attribution.Policy {
	return attribution.Policy{Required: p.RequiredAttributionKeys, Defaults: p.AttributionDefaults}
}
func (a Authority) ResolveAttribution(labels map[string]string) (map[string]string, error) {
	return attribution.Resolve(labels, a.ProjectAttributionPolicy, a.Policy.AttributionPolicy())
}

// Established sessions must retain their original accounting labels under current policy.
func (a Authority) AllowsAttribution(labels map[string]string) bool {
	resolved, err := a.ResolveAttribution(labels)
	return err == nil && maps.Equal(labels, resolved)
}
