package code

import (
	"fmt"
	"slices"

	"github.com/google/uuid"
)

type ResearchService = string

const (
	ServiceSearch ResearchService = "search"
	ServiceReader ResearchService = "reader"
	ServiceZRead  ResearchService = "zread"
)

func ResearchServices() [3]ResearchService {
	return [3]ResearchService{ServiceSearch, ServiceReader, ServiceZRead}
}

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

func ResearchEndpoint(service string) string {
	switch service {
	case ServiceSearch:
		return "https://api.z.ai/api/mcp/web_search_prime/mcp"
	case ServiceReader:
		return "https://api.z.ai/api/mcp/web_reader/mcp"
	case ServiceZRead:
		return "https://api.z.ai/api/mcp/zread/mcp"
	}
	return ""
}

func ResearchToolAllowed(service, tool string) bool {
	switch service {
	case ServiceSearch:
		return tool == "webSearchPrime"
	case ServiceReader:
		return tool == "webReader"
	case ServiceZRead:
		return tool == "search_doc" || tool == "get_repo_structure" || tool == "read_file"
	}
	return false
}

type ConfigurationFile struct {
	File          string `json:"file"`
	Format        string `json:"format"`
	Configuration string `json:"configuration"`
}

type ResearchSessionBinding struct {
	APIKeyID, RevisionID, Service, AccountID, CredentialID string
	Generation                                             int64
}
