//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/operatorcli"
	"github.com/tyk-swe/olp/sdk/management"
)

func TestManagementCLIConfigurationPromotionRefusesStaleDestination(t *testing.T) {
	source, destination := newAccessHarness(t), newAccessHarness(t)
	sourceOwner, destinationOwner := source.owner(), destination.owner()
	createProject(source, sourceOwner, "CLI promoted project")
	scopes := []string{}
	for _, scope := range access.TokenScopes() {
		scopes = append(scopes, scope.String())
	}
	_, sourceToken := createToken(source, sourceOwner, "cli-source", scopes)
	_, destinationToken := createToken(destination, destinationOwner, "cli-destination", scopes)
	dir := t.TempDir()
	tokenPath, document, plan := filepath.Join(dir, "token"), filepath.Join(dir, "document.json"), filepath.Join(dir, "plan.json")
	endpoint := source.HTTP.URL
	os.WriteFile(tokenPath, []byte(sourceToken), 0600)
	runner := operatorcli.Runner{Out: &bytes.Buffer{}, Err: io.Discard, Getenv: func(name string) string {
		if name == "OLP_MANAGEMENT_URL" {
			return endpoint
		}
		return tokenPath
	}}
	if err := runner.Run(t.Context(), []string{"config", "export", "--output", document}); err != nil {
		t.Fatal(err)
	}
	endpoint = destination.HTTP.URL
	os.WriteFile(tokenPath, []byte(destinationToken), 0600)
	planArgs := []string{"config", "plan", "--file", document, "--output", plan}
	if err := runner.Run(t.Context(), planArgs); err != nil {
		t.Fatal(err)
	}
	createProject(destination, destinationOwner, "Intervening project")
	applyArgs := []string{"config", "apply", "--plan-file", plan, "--idempotency-key", "cli-promote"}
	if err := runner.Run(t.Context(), applyArgs); err == nil {
		t.Fatal("CLI applied a stale configuration plan")
	}
	if err := runner.Run(t.Context(), planArgs); err != nil {
		t.Fatal(err)
	}
	if err := runner.Run(t.Context(), applyArgs); err != nil {
		t.Fatal(err)
	}
	var promoted int
	if err := destination.Pool.QueryRow(t.Context(), "SELECT count(*) FROM olp.projects WHERE name='CLI promoted project'").Scan(&promoted); err != nil || promoted != 1 {
		t.Fatalf("promoted=%d err=%v", promoted, err)
	}
}

func TestManagementMCPUsesTokenAuthorityPreconditionsAndSecretFreeResults(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, read := createToken(h, owner, "mcp-read", []string{"read"})
	_, write := createToken(h, owner, "mcp-write", []string{"read", "keys"})
	invoke := func(token, method string, params any) (int, map[string]any) {
		status, _, body := h.call(sweepCaller{token: token}, "POST", "/api/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, nil)
		var response map[string]any
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		return status, response
	}
	status, initialized := invoke(read, "initialize", map[string]any{"protocolVersion": "2025-06-18", "clientInfo": map[string]any{"name": "test", "version": "1"}, "capabilities": map[string]any{}})
	if status != 200 || initialized["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %+v", status, initialized)
	}
	_, listed := invoke(read, "tools/list", map[string]any{})
	tools := listed["result"].(map[string]any)["tools"].([]any)
	foundRead := false
	for _, tool := range tools {
		name := tool.(map[string]any)["name"]
		if name == "list_api_keys" {
			foundRead = true
		}
		if name == "create_api_key" || name == "update_api_key" {
			t.Fatal("read-only token advertised a write")
		}
	}
	if !foundRead {
		t.Fatal("read tool absent")
	}
	_, denied := invoke(read, "tools/call", map[string]any{"name": "create_api_key", "arguments": map[string]any{"body": map[string]any{"name": "forbidden"}, "idempotency_key": "forbidden-key"}})
	if denied["error"] == nil {
		t.Fatal("read-only token could call a write")
	}
	_, missingKey := invoke(write, "tools/call", map[string]any{"name": "create_api_key", "arguments": map[string]any{"body": map[string]any{"name": "missing precondition"}}})
	if missingKey["error"] == nil {
		t.Fatal("MCP silently generated an idempotency key")
	}
	_, created := invoke(write, "tools/call", map[string]any{"name": "create_api_key", "arguments": map[string]any{"body": map[string]any{"name": "MCP key"}, "idempotency_key": "mcp-create"}})
	result := created["result"].(map[string]any)
	if result["isError"] != false {
		t.Fatalf("create failed: %+v", created)
	}
	structured := result["structuredContent"].(map[string]any)
	body := structured["body"].(map[string]any)
	if body["secret"] != nil || strings.Contains(result["content"].([]any)[0].(map[string]any)["text"].(string), `"secret"`) {
		t.Fatalf("MCP returned a one-time key: %+v", created)
	}
	keyID := body["id"].(string)
	_, stale := invoke(write, "tools/call", map[string]any{"name": "update_api_key", "arguments": map[string]any{"path": map[string]string{"api_key_id": keyID}, "body": map[string]any{"name": "not written"}, "if_match": uuid.NewString()}})
	if stale["result"].(map[string]any)["isError"] != true {
		t.Fatal("stale ETag accepted through MCP")
	}
	row := h.want(owner, "GET", "/api/v1/api-keys/"+keyID, nil, nil, 200)
	if row["name"] != "MCP key" {
		t.Fatal("stale mutation changed resource")
	}
	_, sessionOnly := invoke(write, "tools/call", map[string]any{"name": "create_management_token", "arguments": map[string]any{}})
	if sessionOnly["error"] == nil {
		t.Fatal("session-only API exposed to MCP")
	}
}

func TestGeneratedManagementClientSharesTheAPIAuthorizationAndETags(t *testing.T) {
	h := newAccessHarness(t)
	owner := h.owner()
	_, secret := createToken(h, owner, "cli", []string{"read", "keys"})
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := management.NewClient(h.HTTP.URL, file)
	if err != nil {
		t.Fatal(err)
	}
	create, _ := management.Lookup("create_api_key")
	created, err := client.Call(t.Context(), create, management.Arguments{Body: json.RawMessage(`{"name":"CLI key"}`), IdempotencyKey: "cli-create"})
	if err != nil {
		t.Fatal(err)
	}
	var key map[string]any
	json.Unmarshal(created.Body, &key)
	id := key["id"].(string)
	if key["secret"] == nil {
		t.Fatal("CLI creation lost explicit one-time secret")
	}
	get, _ := management.Lookup("get_api_key")
	read, err := client.Call(t.Context(), get, management.Arguments{Path: map[string]string{"api_key_id": id}})
	if err != nil || read.ETag == "" {
		t.Fatalf("read=%+v err=%v", read, err)
	}
	update, _ := management.Lookup("update_api_key")
	args := management.Arguments{Path: map[string]string{"api_key_id": id}, Body: json.RawMessage(`{"name":"CLI changed"}`), IfMatch: read.ETag}
	if _, err = client.Call(t.Context(), update, args); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Call(t.Context(), update, args); err == nil {
		t.Fatal("API accepted stale client ETag")
	}
}
