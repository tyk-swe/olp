//go:build integration

package integration_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/tyk-swe/olp/internal/operatorcli"
	"github.com/tyk-swe/olp/sdk/management"
)

func TestGeneratedManagementClientsAndMCPFollowEveryMachineAuthorization(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	projectID := createProject(h, owner, "Generated authorization sweep")
	raw, err := os.ReadFile("../../internal/access/testdata/authorization.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden routeGolden
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	machines := golden
	machines.Archetypes = nil
	for _, archetype := range golden.Archetypes {
		if archetype.Kind == "machine" {
			machines.Archetypes = append(machines.Archetypes, archetype)
		}
	}
	callers := sweepCallers(h, owner, machines, projectID)
	operations := management.Operations()
	dir := t.TempDir()
	tokenFile, bodyFile := filepath.Join(dir, "token"), filepath.Join(dir, "body.json")
	write := func(path string, body []byte) {
		t.Helper()
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	client, err := management.NewClient(h.HTTP.URL, tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	runner := operatorcli.Runner{Out: io.Discard, Err: io.Discard, Getenv: func(name string) string {
		if name == "OLP_MANAGEMENT_URL" {
			return h.HTTP.URL
		}
		if name == "OLP_MANAGEMENT_TOKEN_FILE" {
			return tokenFile
		}
		return ""
	}}
	query := map[string]string{"start": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), "end": time.Now().UTC().Format(time.RFC3339), "from": "1", "to": "2", "variant": "openai", "dimension": "route", "gateway_url": h.HTTP.URL}
	rpc := func(caller sweepCaller, method string, params any) map[string]any {
		t.Helper()
		status, _, body := h.call(caller, "POST", "/api/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, nil)
		if status != 200 {
			t.Fatalf("MCP %s: HTTP %d", method, status)
		}
		var result map[string]any
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for index, archetype := range golden.Archetypes {
		if archetype.Kind != "machine" {
			continue
		}
		caller := callers[archetype.Name]
		write(tokenFile, []byte(caller.token))
		discovery := rpc(caller, "tools/list", map[string]any{})
		result, ok := discovery["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s tool discovery failed", archetype.Name)
		}
		listed := map[string]bool{}
		for _, item := range result["tools"].([]any) {
			listed[item.(map[string]any)["name"].(string)] = true
		}
		for _, operation := range operations {
			t.Run(archetype.Name+"/"+operation.Name, func(t *testing.T) {
				pattern := operation.Method + " " + operation.Path
				row, ok := golden.Routes[pattern]
				if !ok {
					t.Fatalf("generated operation has no authorization golden: %s", pattern)
				}
				allowed := row[index] == 'Y'
				if listed[operation.Name] != allowed {
					t.Fatal("MCP discovery disagrees with the authorization golden")
				}
				arguments := management.Arguments{Path: map[string]string{}, Query: map[string]any{}, IfMatch: uuid.NewString(), IdempotencyKey: uuid.NewString()}
				command := []string{"api", operation.Name, "--if-match", arguments.IfMatch, "--idempotency-key", arguments.IdempotencyKey}
				for _, parameter := range operation.Parameters {
					switch parameter.In {
					case "path":
						_, path := sweepPath("GET /{"+parameter.Name+"}", nil)
						value := strings.TrimPrefix(path, "/")
						arguments.Path[parameter.Name] = value
						command = append(command, "--path", parameter.Name+"="+value)
					case "query":
						if parameter.Required {
							value, ok := query[parameter.Name]
							if !ok {
								t.Fatalf("sweep needs a value for required query %s", parameter.Name)
							}
							arguments.Query[parameter.Name] = value
							command = append(command, "--query", parameter.Name+"="+value)
						}
					}
				}
				if body := sweepBody(pattern, projectID); body != nil {
					arguments.Body, err = json.Marshal(body)
					if err != nil {
						t.Fatal(err)
					}
					write(bodyFile, arguments.Body)
					command = append(command, "--body-file", bodyFile)
				}
				_, callErr := client.Call(t.Context(), operation, arguments)
				var problem *management.APIError
				if callErr != nil && !errors.As(callErr, &problem) {
					t.Fatal(callErr)
				}
				cliErr := runner.Run(t.Context(), command)
				var cliProblem *management.APIError
				if cliErr != nil && !errors.As(cliErr, &cliProblem) {
					t.Fatal(cliErr)
				}
				if allowed {
					if problem != nil && (problem.Status == 401 || problem.Status == 403) {
						t.Fatalf("admitted client refused: %v", problem)
					}
					if cliProblem != nil && (cliProblem.Status == 401 || cliProblem.Status == 403) {
						t.Fatalf("admitted CLI refused: %v", cliProblem)
					}
				} else {
					// SCIM uses its own error envelope. The bounded client keeps
					// server details private; the HTTP refusal is authoritative.
					if problem == nil || problem.Status != 403 {
						t.Fatalf("SDK failed to refuse operation: %v", callErr)
					}
					if cliProblem == nil || cliProblem.Status != 403 {
						t.Fatalf("CLI failed to refuse operation: %v", cliErr)
					}
					denial := rpc(caller, "tools/call", map[string]any{"name": operation.Name, "arguments": arguments})
					failure, ok := denial["error"].(map[string]any)
					if !ok || failure["message"] != "Tool is outside the token's authority" {
						t.Fatal("MCP did not refuse the disallowed tool before dispatch")
					}
				}
			})
		}
		for _, name := range []string{"create_management_token", "change_password", "management_mcp", "unknown_operation"} {
			if result := rpc(caller, "tools/call", map[string]any{"name": name}); result["error"] == nil {
				t.Fatalf("MCP exposed excluded tool %s", name)
			}
		}
	}
}
