//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tyk-swe/olp/openapi"
)

type responseContract struct {
	method     string
	path       *regexp.Regexp
	parameters int
	status     int
	schema     *jsonschema.Schema
}

var managementContracts = sync.OnceValues(func() ([]responseContract, error) {
	var document map[string]any
	if err := json.Unmarshal(openapi.Document, &document); err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("https://contract.test/management.json", document); err != nil {
		return nil, err
	}
	var result []responseContract
	parameter := regexp.MustCompile(`\{[^}]+\}`)
	for path, entry := range document["paths"].(map[string]any) {
		for method, operation := range entry.(map[string]any) {
			op, ok := operation.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := op["responses"].(map[string]any)
			if !ok {
				continue
			}
			for status, response := range responses {
				body, ok := response.(map[string]any)["content"].(map[string]any)
				if !ok {
					continue
				}
				for _, media := range body {
					schema := media.(map[string]any)["schema"].(map[string]any)
					ref, ok := schema["$ref"].(string)
					if !ok {
						continue
					}
					compiled, err := c.Compile("https://contract.test/management.json" + ref)
					if err != nil {
						return nil, err
					}
					code, _ := strconv.Atoi(status)
					pattern := "^" + parameter.ReplaceAllString(path, "[^/]+") + "$"
					result = append(result, responseContract{strings.ToUpper(method), regexp.MustCompile(pattern), len(parameter.FindAllString(path, -1)), code, compiled})
				}
			}
		}
	}
	// Literal segments win over templated ones, so /revisions/diff is matched
	// by its own contract rather than by /revisions/{revision_id}.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].parameters != result[j].parameters {
			return result[i].parameters < result[j].parameters
		}
		return result[i].path.String() < result[j].path.String()
	})
	return result, nil
})

func validateManagementResponse(t *testing.T, method, path string, status int, body map[string]any) {
	t.Helper()
	contracts, err := managementContracts()
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range contracts {
		if contract.method == method && contract.path.MatchString(path) && contract.status == status {
			if err := contract.schema.Validate(body); err != nil {
				// Validation error values can contain one-time credentials. Report
				// only the structural paths, never the response or validator text.
				var locations []string
				var collect func(*jsonschema.ValidationError)
				collect = func(e *jsonschema.ValidationError) {
					locations = append(locations, fmt.Sprint(e.InstanceLocation))
					for _, child := range e.Causes {
						collect(child)
					}
				}
				collect(err.(*jsonschema.ValidationError))
				t.Fatalf("%s %s (%d) violates response contract at %v", method, path, status, locations)
			}
			return
		}
	}
}
