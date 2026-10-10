package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/tyk-swe/olp/internal/access"
)

const maxCapturePolicies = 1000

type createCapturePolicyInput struct {
	ProjectID   *string  `json:"project_id"`
	Route       *string  `json:"route_slug"`
	SinkID      string   `json:"sink"`
	SampleRatio string   `json:"sample_ratio"`
	Include     []string `json:"include"`
	Redact      []string `json:"redact"`
	KeyIDs      []string `json:"key_ids"`
	EndUsers    []string `json:"end_user_digests"`
	MaxBytes    int      `json:"max_bytes"`
	Enabled     *bool    `json:"enabled"`
}

func (m *Management) listCaptureSinks(r *http.Request, p access.Principal) (access.Reply, error) {
	rows, err := m.Pool.Query(r.Context(), captureSinkOptionsSQL)
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for rows.Next() {
		var id, name, typ string
		if err := rows.Scan(&id, &name, &typ); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		items = append(items, map[string]any{"export_sink_id": id, "name": name, "type": typ})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return access.Reply{}, err
	}
	return access.OK(map[string]any{"items": items}), nil
}

func (m *Management) captureConfiguration(r *http.Request, _ access.Principal) (access.Reply, error) {
	var enabled bool
	var etag string
	if err := m.Pool.QueryRow(r.Context(), captureConfigurationSQL).Scan(&enabled, &etag); err != nil {
		return access.Reply{}, err
	}
	return access.Detail(map[string]any{"enabled": enabled, "etag": etag}, etag), nil
}

func (m *Management) updateCaptureConfiguration(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	if input.Enabled == nil {
		return access.Reply{}, access.Invalid("enabled", "Send whether capture is enabled.")
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Authorize(access.Access); err != nil {
		return access.Reply{}, err
	}
	var current bool
	var etag string
	if err := tx.QueryRow(r.Context(), lockCaptureConfigurationSQL).Scan(&current, &etag); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	etag = access.NewID()
	if _, err = tx.Exec(r.Context(), updateCaptureConfigurationSQL, *input.Enabled, etag, p.UserID()); err != nil {
		return access.Reply{}, err
	}
	if err = access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_configuration.update", "capture_configuration", "singleton", "success", nil); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Detail(map[string]any{"enabled": *input.Enabled, "etag": etag}, etag))
}

func (m *Management) listCapturePolicies(r *http.Request, p access.Principal) (access.Reply, error) {
	page, err := access.Page(r)
	if err != nil {
		return access.Reply{}, err
	}
	rows, err := m.Pool.Query(r.Context(), listCapturePoliciesSQL, page.Before, p.AllProjects, p.ProjectIDs(), page.Limit+1)
	if err != nil {
		return access.Reply{}, err
	}
	items := []map[string]any{}
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		var item map[string]any
		if err := json.Unmarshal(data, &item); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		var policy CapturePolicy
		if err := json.Unmarshal(data, &policy); err != nil {
			rows.Close()
			return access.Reply{}, err
		}
		item["id"] = policy.ID
		items = append(items, item)
	}
	rows.Close()
	return access.ListReply(items, page), rows.Err()
}

func (m *Management) capturePolicy(r *http.Request, p access.Principal) (access.Reply, error) {
	policy, err := m.readCapturePolicy(r, p)
	if err != nil {
		return access.Reply{}, err
	}
	return access.Detail(policy, policy.ETag), nil
}

func (m *Management) readCapturePolicy(r *http.Request, p access.Principal) (CapturePolicy, error) {
	var data []byte
	var etag string
	var projectID *string
	if err := m.Pool.QueryRow(r.Context(), getCapturePolicySQL, r.PathValue("capture_policy_id")).Scan(&data, &etag, &projectID); err != nil {
		return CapturePolicy{}, notFoundErr(err)
	}
	if err := p.Project(projectID, access.View); err != nil {
		return CapturePolicy{}, err
	}
	var policy CapturePolicy
	return policy, json.Unmarshal(data, &policy)
}

func (m *Management) createCapturePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input createCapturePolicyInput
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	policy := CapturePolicy{ID: access.NewID(), ETag: access.NewID(), ProjectID: input.ProjectID, Route: input.Route, SinkID: input.SinkID, SampleRatio: input.SampleRatio, Include: input.Include, Redact: input.Redact, KeyIDs: input.KeyIDs, EndUsers: input.EndUsers, MaxBytes: input.MaxBytes, Enabled: true}
	if input.Enabled != nil {
		policy.Enabled = *input.Enabled
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := m.captureEnabled(r, tx); err != nil {
		return access.Reply{}, err
	}
	if err := m.validateCapturePolicy(r, tx, &policy); err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, policy.ProjectID); err != nil {
		return access.Reply{}, err
	}
	claim, replayed, err := m.Access.Replay(r, tx, p, input)
	if err != nil {
		return access.Reply{}, err
	}
	if replayed != nil {
		return access.Commit(r, tx, *replayed)
	}
	var count int64
	if err := tx.QueryRow(r.Context(), countCapturePoliciesSQL).Scan(&count); err != nil {
		return access.Reply{}, err
	}
	if count >= maxCapturePolicies {
		return access.Reply{}, access.Invalid("capture_policies", "This installation already has 1000 capture policies.")
	}
	if policy.KeyIDs == nil {
		policy.KeyIDs = []string{}
	}
	if policy.EndUsers == nil {
		policy.EndUsers = []string{}
	}
	if policy.Redact == nil {
		policy.Redact = []string{}
	}
	if _, err := tx.Exec(r.Context(), createCapturePolicySQL, policy.ID, policy.ProjectID, policy.Route, policy.SinkID, policy.SampleRatio, policy.Include, policy.Redact, policy.MaxBytes, policy.Enabled, policy.ETag, p.UserID(), policy.KeyIDs, policy.EndUsers); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.create", "capture_policy", policy.ID, "success", policy.ProjectID); err != nil {
		return access.Reply{}, err
	}
	result := access.Reply{Status: 201, ETag: policy.ETag, Location: "/api/v1/observability/capture-policies/" + policy.ID, Body: policy}
	if err := m.Access.CompleteReplay(r, tx, claim, result); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, result)
}

func (m *Management) updateCapturePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	var input struct {
		SinkID      *string  `json:"sink"`
		SampleRatio *string  `json:"sample_ratio"`
		Include     []string `json:"include"`
		Redact      []string `json:"redact"`
		KeyIDs      []string `json:"key_ids"`
		EndUsers    []string `json:"end_user_digests"`
		MaxBytes    *int     `json:"max_bytes"`
		Enabled     *bool    `json:"enabled"`
		ProjectID   *string  `json:"project_id"`
		Route       *string  `json:"route_slug"`
	}
	if err := access.Decode(r, &input); err != nil {
		return access.Reply{}, err
	}
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, etag, projectID, err := m.lockCapturePolicy(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if input.ProjectID != nil && (projectID == nil || *input.ProjectID != *projectID) {
		return access.Reply{}, access.Invalid("project_id", "policy project is immutable")
	}
	if input.Route != nil && (current.Route == nil || *input.Route != *current.Route) {
		return access.Reply{}, access.Invalid("route_slug", "policy route is immutable")
	}
	if input.SinkID != nil {
		current.SinkID = *input.SinkID
	}
	if input.SampleRatio != nil {
		current.SampleRatio = *input.SampleRatio
	}
	if input.Include != nil {
		current.Include = input.Include
	}
	if input.Redact != nil {
		current.Redact = input.Redact
	}
	if input.KeyIDs != nil {
		current.KeyIDs = input.KeyIDs
	}
	if input.EndUsers != nil {
		current.EndUsers = input.EndUsers
	}
	if input.MaxBytes != nil {
		current.MaxBytes = *input.MaxBytes
	}
	if input.Enabled != nil {
		current.Enabled = *input.Enabled
	}
	disableOnly := input.Enabled != nil && !*input.Enabled && input.SinkID == nil &&
		input.SampleRatio == nil && input.Include == nil && input.Redact == nil &&
		input.KeyIDs == nil && input.EndUsers == nil && input.MaxBytes == nil &&
		input.ProjectID == nil && input.Route == nil
	if !disableOnly {
		if err := m.captureEnabled(r, tx); err != nil {
			return access.Reply{}, err
		}
		if err := m.validateCapturePolicy(r, tx, &current); err != nil {
			return access.Reply{}, err
		}
	}
	etag = access.NewID()
	if current.KeyIDs == nil {
		current.KeyIDs = []string{}
	}
	if current.EndUsers == nil {
		current.EndUsers = []string{}
	}
	if current.Redact == nil {
		current.Redact = []string{}
	}
	if _, err := tx.Exec(r.Context(), updateCapturePolicySQL, current.ID, current.SinkID, current.SampleRatio, current.Include, current.Redact, current.MaxBytes, current.Enabled, etag, current.KeyIDs, current.EndUsers); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.update", "capture_policy", current.ID, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	current.ETag = etag
	return access.Commit(r, tx, access.Detail(current, etag))
}

func (m *Management) deleteCapturePolicy(r *http.Request, _ access.Principal) (access.Reply, error) {
	tx, err := m.Access.Begin(r)
	if err != nil {
		return access.Reply{}, err
	}
	defer tx.Rollback(r.Context())
	p, err := m.Access.Reauthorize(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	current, etag, projectID, err := m.lockCapturePolicy(r, tx)
	if err != nil {
		return access.Reply{}, err
	}
	if err := p.Project(projectID, access.View); err != nil {
		return access.Reply{}, err
	}
	if err := m.requireSinkScope(r, tx, p, projectID); err != nil {
		return access.Reply{}, err
	}
	if err := access.Match(r, etag); err != nil {
		return access.Reply{}, err
	}
	if _, err := tx.Exec(r.Context(), deleteCapturePolicySQL, current.ID); err != nil {
		return access.Reply{}, err
	}
	if err := access.AuditForProject(r.Context(), tx, r, p.Actor(), "capture_policy.delete", "capture_policy", current.ID, "success", projectID); err != nil {
		return access.Reply{}, err
	}
	return access.Commit(r, tx, access.Reply{Status: 204})
}

func (m *Management) lockCapturePolicy(r *http.Request, tx pgx.Tx) (CapturePolicy, string, *string, error) {
	var data []byte
	var etag string
	var projectID *string
	if err := tx.QueryRow(r.Context(), lockCapturePolicySQL, r.PathValue("capture_policy_id")).Scan(&data, &etag, &projectID); err != nil {
		return CapturePolicy{}, "", nil, notFoundErr(err)
	}
	var policy CapturePolicy
	return policy, etag, projectID, json.Unmarshal(data, &policy)
}

func (m *Management) captureEnabled(r *http.Request, q access.Queryer) error {
	var enabled bool
	var etag string
	if err := q.QueryRow(r.Context(), captureConfigurationSQL).Scan(&enabled, &etag); err != nil {
		return err
	}
	if !enabled {
		return access.Invalid("capture", "Enable payload capture before managing capture policies.")
	}
	return nil
}

func (m *Management) validateCapturePolicy(r *http.Request, q access.Queryer, p *CapturePolicy) error {
	if p.ProjectID == nil && p.Route == nil {
		return access.Invalid("capture_policy", "scope the policy to a project or a route")
	}
	if err := p.Validate(); err != nil {
		return access.Invalid("capture_policy", err.Error())
	}
	if p.SinkID == "" {
		return access.Invalid("sink", "capture requires an installation-owned sink")
	}
	sinkID, err := access.ParseUUID(p.SinkID)
	if err != nil {
		return access.Invalid("sink", "sink must be a valid UUID")
	}
	if p.ProjectID != nil {
		project, err := access.ParseUUID(*p.ProjectID)
		if err != nil {
			return access.Invalid("project_id", "project_id must be a valid UUID")
		}
		p.ProjectID = &project
		var exists bool
		if err := q.QueryRow(r.Context(), filterProjectScopeSQL, project).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return access.Invalid("project_id", "the policy project does not exist")
		}
	}
	var sinkProject *string
	var sinkEnabled bool
	if err := q.QueryRow(r.Context(), captureSinkScopeSQL, sinkID).Scan(&sinkProject, &sinkEnabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return access.Invalid("sink", "capture requires an installation-owned enabled sink")
		}
		return err
	}
	if sinkProject != nil || !sinkEnabled {
		return access.Invalid("sink", "capture requires an installation-owned enabled sink")
	}
	if p.Route != nil {
		var routeProject *string
		if err := q.QueryRow(r.Context(), captureRouteScopeSQL, *p.Route).Scan(&routeProject); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return access.Invalid("route_slug", "the policy route does not exist")
			}
			return err
		}
		if p.ProjectID != nil && routeProject != nil && *routeProject != *p.ProjectID {
			return access.Invalid("route_slug", "the policy route must belong to the policy project")
		}
	}
	for index, keyID := range p.KeyIDs {
		key, err := access.ParseUUID(keyID)
		if err != nil {
			return access.Invalid("key_ids", "key_ids must contain valid UUIDs")
		}
		p.KeyIDs[index] = key
		var keyProject *string
		if err := q.QueryRow(r.Context(), captureKeyScopeSQL, key).Scan(&keyProject); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return access.Invalid("key_ids", fmt.Sprintf("key %s does not exist", keyID))
			}
			return err
		}
		if p.ProjectID != nil && (keyProject == nil || *keyProject != *p.ProjectID) {
			return access.Invalid("key_ids", fmt.Sprintf("key %s does not belong to the policy project", keyID))
		}
	}
	return nil
}
