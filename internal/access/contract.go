package access

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/tyk-swe/olp/openapi"
)

// Requirement is what a management route demands of its caller, as the
// management contract declares it in the operation's security requirements.
// The contract decides which operations a route needs; the policy decides who
// holds them.
type Requirement struct {
	// Public routes authenticate no principal: sign-in, setup, invitation
	// acceptance, and the published contract itself.
	Public bool
	// Alternatives are ORed; the operations within one are ANDed.
	Alternatives []Alternative
}

// Alternative is one way to satisfy a Requirement: a principal of Kind that
// may perform every Operation, with an installation-wide scope when
// Installation is set.
type Alternative struct {
	Kind         string // "user" (session) or "machine" (management token)
	Operations   []Operation
	Installation bool
}

// Admits reports whether p satisfies the requirement.
func (req Requirement) Admits(p Principal) error {
	var refusal error = Forbidden()
	for _, alternative := range req.Alternatives {
		if alternative.Kind != p.Kind {
			continue
		}
		err := error(nil)
		for _, op := range alternative.Operations {
			if err = p.Authorize(op); err != nil {
				break
			}
		}
		if err == nil && alternative.Installation && !p.AllProjects {
			err = Forbidden()
		}
		if err == nil {
			return nil
		}
		refusal = err
	}
	return refusal
}

// ContractRequirements parses and validates the security requirements of
// every management operation in the embedded contract, keyed by ServeMux
// pattern ("GET /api/v1/users/{user_id}").
func ContractRequirements() (map[string]Requirement, error) { return contractRequirements() }

var contractRequirements = sync.OnceValues(func() (map[string]Requirement, error) {
	return parseRequirements(openapi.Document)
})

// requirementFor returns the contract requirement for a matched pattern.
func requirementFor(pattern string) (Requirement, bool) {
	requirements, err := contractRequirements()
	if err != nil {
		panic(err)
	}
	req, ok := requirements[pattern]
	return req, ok
}

// parseRequirements reads every operation's security requirements and
// refuses a contract that does not read truthfully: unknown schemes or
// operations, a CSRF proof on a safe method or its absence on an unsafe one,
// a token scope that cannot be delegated, or an installation-wide operation
// that does not say so.
func parseRequirements(document []byte) (map[string]Requirement, error) {
	var contract struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(document, &contract); err != nil {
		return nil, err
	}
	requirements := map[string]Requirement{}
	for path, item := range contract.Paths {
		for method, raw := range item {
			if !slices.Contains([]string{"get", "put", "post", "delete", "patch"}, method) {
				continue
			}
			var operation struct {
				Security *[]map[string][]string `json:"security"`
			}
			if err := json.Unmarshal(raw, &operation); err != nil {
				return nil, err
			}
			pattern := strings.ToUpper(method) + " " + path
			if operation.Security == nil {
				return nil, fmt.Errorf("%s declares no security requirement", pattern)
			}
			req, err := parseRequirement(pattern, *operation.Security)
			if err != nil {
				return nil, err
			}
			requirements[pattern] = req
		}
	}
	return requirements, nil
}

func parseRequirement(pattern string, security []map[string][]string) (Requirement, error) {
	if len(security) == 0 {
		return Requirement{Public: true}, nil
	}
	if len(security) == 1 && len(security[0]) == 1 && security[0]["bootstrapSetupToken"] != nil {
		return Requirement{Public: true}, nil
	}
	unsafe := !strings.HasPrefix(pattern, "GET ")
	var req Requirement
	for _, alternative := range security {
		parsed := Alternative{}
		var scopes []string
		switch {
		case alternative["sessionCookie"] != nil:
			if _, proof := alternative["csrfToken"]; proof != unsafe || len(alternative) != 1+boolInt(proof) {
				return req, fmt.Errorf("%s: a session alternative needs a CSRF proof exactly for unsafe methods", pattern)
			}
			parsed.Kind, scopes = "user", alternative["sessionCookie"]
		case alternative["managementToken"] != nil:
			if len(alternative) != 1 {
				return req, fmt.Errorf("%s: a management token alternative stands alone", pattern)
			}
			parsed.Kind, scopes = "machine", alternative["managementToken"]
		case alternative["apiKeyBearer"] != nil:
			catalog := pattern == "GET /api/v1/catalog" || strings.HasPrefix(pattern, "GET /api/v1/catalog/")
			if !catalog || unsafe || len(alternative) != 1 || len(alternative["apiKeyBearer"]) != 1 || alternative["apiKeyBearer"][0] != "models_read" {
				return req, fmt.Errorf("%s: inference keys admit only safe models_read operations", pattern)
			}
			parsed.Kind, scopes = "key", []string{"read"}
		default:
			return req, fmt.Errorf("%s: unknown security alternative %v", pattern, alternative)
		}
		needsInstallation := false
		for _, scope := range scopes {
			if scope == "installation" {
				parsed.Installation = true
				continue
			}
			op, ok := ParseOperation(scope)
			if !ok {
				return req, fmt.Errorf("%s: unknown operation %q", pattern, scope)
			}
			if parsed.Kind == "machine" && !policy[op].delegable {
				return req, fmt.Errorf("%s: %s cannot be a management token scope", pattern, scope)
			}
			needsInstallation = needsInstallation || policy[op].installation
			parsed.Operations = append(parsed.Operations, op)
		}
		if len(parsed.Operations) == 0 {
			return req, fmt.Errorf("%s: an alternative names no operation", pattern)
		}
		if needsInstallation && !parsed.Installation {
			return req, fmt.Errorf("%s: an installation-wide operation must declare the installation scope", pattern)
		}
		req.Alternatives = append(req.Alternatives, parsed)
	}
	return req, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
