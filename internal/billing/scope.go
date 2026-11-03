package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"regexp"

	"github.com/tyk-swe/olp/internal/access"
)

var (
	openAIProjectPattern    = regexp.MustCompile(`^proj_[A-Za-z0-9]{1,64}$`)
	anthropicWorkspacePatrn = regexp.MustCompile(`^wrkspc_[A-Za-z0-9]{1,64}$`)
)

// Scope is the strict document a billing import reads: an absent or empty
// object means the whole organization.
type Scope struct {
	ProjectIDs   []string `json:"project_ids,omitempty"`
	WorkspaceIDs []string `json:"workspace_ids,omitempty"`
}

func decodePollingScope(raw json.RawMessage) (Scope, bool, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return Scope{}, true, nil
	}
	var scope Scope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		return Scope{}, false, access.Invalid("scope", "scope must be a JSON object with only the source's keys")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return Scope{}, false, access.Invalid("scope", "scope must be a single JSON document")
	}
	return scope, false, nil
}

func validateOpenAIScope(raw json.RawMessage) (json.RawMessage, error) {
	scope, empty, err := decodePollingScope(raw)
	if err != nil {
		return nil, err
	}
	if empty {
		return json.RawMessage(`{}`), nil
	}
	if scope.WorkspaceIDs != nil {
		return nil, access.Invalid("scope", "openai_costs accepts only project_ids")
	}
	if len(scope.ProjectIDs) > 100 {
		return nil, access.Invalid("scope.project_ids", "at most 100 project_ids")
	}
	for _, id := range scope.ProjectIDs {
		if !openAIProjectPattern.MatchString(id) {
			return nil, access.Invalid("scope.project_ids",
				"project_ids must look like proj_ followed by 1 to 64 letters or digits")
		}
	}
	// Stored scope is the decoded document's canonical serialization, so equal
	// scopes compare equal regardless of how the request formatted them.
	return json.Marshal(scope)
}

func validateAnthropicScope(raw json.RawMessage) (json.RawMessage, error) {
	scope, empty, err := decodePollingScope(raw)
	if err != nil {
		return nil, err
	}
	if empty {
		return json.RawMessage(`{}`), nil
	}
	if scope.ProjectIDs != nil {
		return nil, access.Invalid("scope", "anthropic_cost_report accepts only workspace_ids")
	}
	if len(scope.WorkspaceIDs) > 100 {
		return nil, access.Invalid("scope.workspace_ids", "at most 100 workspace_ids")
	}
	for _, id := range scope.WorkspaceIDs {
		if id != "default" && !anthropicWorkspacePatrn.MatchString(id) {
			return nil, access.Invalid("scope.workspace_ids",
				"workspace_ids must look like wrkspc_ followed by 1 to 64 letters or digits, or be default")
		}
	}
	// Stored scope is the decoded document's canonical serialization, so equal
	// scopes compare equal regardless of how the request formatted them.
	return json.Marshal(scope)
}
