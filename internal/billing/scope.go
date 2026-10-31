package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"

	"github.com/tyk-swe/olp/internal/access"
)

const (
	// SourceOpenAI costs come from the organization's admin costs endpoint.
	SourceOpenAI = "openai_costs"
	// SourceAnthropic costs come from the organization's cost report.
	SourceAnthropic = "anthropic_cost_report"
)

// SourceFor names the vendor cost endpoint a provider kind imports, or
// refuses a kind that has none.
func SourceFor(kind string) (string, error) {
	switch kind {
	case "openai":
		return SourceOpenAI, nil
	case "anthropic":
		return SourceAnthropic, nil
	}
	return "", access.Invalid("billing_import",
		fmt.Sprintf("Billing imports are only supported for openai and anthropic providers, not %q.", kind))
}

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

// ValidateScope checks a scope document against its source's vocabulary and
// returns the canonical document stored. Anything the document does not list
// is rejected rather than silently narrowed.
func ValidateScope(source string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	var scope Scope
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&scope); err != nil {
		return nil, access.Invalid("scope", "scope must be a JSON object with only the source's keys")
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, access.Invalid("scope", "scope must be a single JSON document")
	}
	switch source {
	case SourceOpenAI:
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
	case SourceAnthropic:
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
	default:
		return nil, access.Invalid("scope", "unknown billing source")
	}
	// Stored scope is the decoded document's canonical serialization, so equal
	// scopes compare equal regardless of how the request formatted them.
	canonical, err := json.Marshal(scope)
	if err != nil {
		return nil, err
	}
	return canonical, nil
}

func ValidCredential(credential string) error {
	if n := len(credential); n < 20 || n > 512 {
		return access.Invalid("credential", "credential must be 20 to 512 bytes")
	}
	for _, r := range credential {
		if r < 0x21 || r > 0x7e {
			return access.Invalid("credential", "credential must be printable ASCII without whitespace")
		}
	}
	return nil
}
