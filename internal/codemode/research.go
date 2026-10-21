package codemode

import (
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// ResearchTools binds research to one subscription independently of inference
// models. The complete binding is frozen in each published route revision.
type ResearchTools struct {
	AccountID string   `json:"account_id"`
	Services  []string `json:"services"`
}

func (t *ResearchTools) Validate() error {
	if t == nil {
		return nil
	}
	if id, err := uuid.Parse(t.AccountID); err != nil || id.String() != t.AccountID {
		return fmt.Errorf("choose a research account")
	}
	if len(t.Services) < 1 || len(t.Services) > 3 {
		return fmt.Errorf("choose between one and three research services")
	}
	for i, service := range t.Services {
		if ResearchEndpoint(service) == "" || slices.Contains(t.Services[:i], service) {
			return fmt.Errorf("choose unique supported research services")
		}
	}
	return nil
}

func (t *ResearchTools) Enables(service string) bool {
	return t != nil && ResearchEndpoint(service) != "" && slices.Contains(t.Services, service)
}

// ResearchEndpoint deliberately has no configurable host or path.
func ResearchEndpoint(service string) string {
	switch service {
	case "search":
		return "https://api.z.ai/api/mcp/web_search_prime/mcp"
	case "reader":
		return "https://api.z.ai/api/mcp/web_reader/mcp"
	case "zread":
		return "https://api.z.ai/api/mcp/zread/mcp"
	}
	return ""
}

func ResearchToolAllowed(service, tool string) bool {
	switch service {
	case "search":
		return tool == "webSearchPrime"
	case "reader":
		return tool == "webReader"
	case "zread":
		return tool == "search_doc" || tool == "get_repo_structure" || tool == "read_file"
	}
	return false
}

type ConfigurationFile struct {
	File          string `json:"file"`
	Format        string `json:"format"`
	Configuration string `json:"configuration"`
}

// ResearchSessionBinding contains only authorization metadata, never content.
type ResearchSessionBinding struct {
	APIKeyID, RevisionID, Service, AccountID, CredentialID string
	Generation                                             int64
}
