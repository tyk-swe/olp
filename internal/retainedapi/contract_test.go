package retainedapi

import (
	"errors"
	"strings"
	"testing"

	"github.com/tyk-swe/olp/internal/oif"
)

func TestEndpointContracts(t *testing.T) {
	seen := map[string]bool{}
	for _, endpoint := range Endpoints() {
		key := endpoint.Method + " " + endpoint.Pattern
		if seen[key] {
			t.Fatalf("duplicate endpoint %s", key)
		}
		seen[key] = true
		if !Operation(endpoint.Operation) || endpoint.Kind == "" || endpoint.Identity == "" {
			t.Fatalf("incomplete contract: %+v", endpoint)
		}
		if strings.Contains(endpoint.Pattern, "...") {
			t.Fatalf("wildcard contract: %s", key)
		}
	}
	for _, refused := range []string{"PATCH /gemini/v1beta/agents/{id}", "GET /v1/conversations", "POST /v1/fine_tuning/jobs/{id}/resume"} {
		if seen[refused] {
			t.Fatalf("unverified method enabled: %s", refused)
		}
	}
	families := Families()
	families[0].Methods[0] = "arbitrary"
	families[0].Children[0].Kind = "arbitrary"
	if Families()[0].Methods[0] == "arbitrary" || Families()[0].Children[0].Kind == "arbitrary" {
		t.Fatal("registry exposed writable aliases")
	}
}

func TestRewriteOwnerReferences(t *testing.T) {
	raw := `{"training_file":"file_local","validation_file":null,"file_ids":["file_local"],"metadata":{"file_id":"user-value"},"seed":9007199254740993}`
	source, err := oif.ParseJSON([]byte(raw), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{"training_file": "file", "validation_file": "file", "file_ids": "file"}
	result, err := Rewrite(source, refs, func(kind, id string) (string, error) {
		if kind != "file" || id != "file_local" {
			t.Fatalf("unexpected reference %s:%s", kind, id)
		}
		return "file_native", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Raw(), `9007199254740993`) || !strings.Contains(result.Raw(), `"file_id":"user-value"`) || strings.Count(result.Raw(), "file_native") != 2 {
		t.Fatal(result.Raw())
	}
	if source.Raw() != raw {
		t.Fatal("source mutated")
	}
	_, err = Rewrite(source, refs, func(string, string) (string, error) { return "", errors.New("foreign owner") })
	if err == nil {
		t.Fatal("foreign reference admitted")
	}
}

func TestRewriteRejectsUndeclaredReferences(t *testing.T) {
	for _, raw := range []string{`{"new_file_id":"native"}`, `{"file_ids":[1]}`, `{"file_ids":""}`} {
		source, err := oif.ParseJSON([]byte(raw), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Rewrite(source, map[string]string{"file_ids": "file"}, func(string, string) (string, error) { return "bound", nil }); err == nil {
			t.Fatalf("admitted %s", raw)
		}
	}
}

func TestRewriteDistinguishesLiteralAndRetainedIDs(t *testing.T) {
	source, err := oif.ParseJSON([]byte(`{"id":"native-job","organization_id":"native-org","metadata":{"other_id":"user"}}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Rewrite(source, map[string]string{"/id": "fine_tuning_job", "organization_id": Literal}, func(kind, id string) (string, error) {
		if kind != "fine_tuning_job" || id != "native-job" {
			t.Fatalf("literal resolved as a resource: %s %s", kind, id)
		}
		return "local-job", nil
	})
	if err != nil || !strings.Contains(result.Raw(), `"organization_id":"native-org"`) || !strings.Contains(result.Raw(), `"id":"local-job"`) {
		t.Fatalf("rewrite=%s error=%v", result.Raw(), err)
	}
}

func TestRewriteEnvironmentVariableNames(t *testing.T) {
	for _, raw := range []string{
		`{"base_environment":{"env":{"client_id":"app","id":"value","base_agent":"literal"}}}`,
		`{"base_environment":{"env":{"client_id":{"value":"app"},"base_agent":{"value":"literal"}}}}`,
	} {
		source, err := oif.ParseJSON([]byte(raw), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		projected, err := Rewrite(source, map[string]string{"base_agent": "agent_resource"}, func(string, string) (string, error) {
			t.Fatal("environment variable treated as a resource reference")
			return "", nil
		})
		if err != nil || projected.Raw() != raw {
			t.Fatalf("environment variable changed: %s %v", projected.Raw(), err)
		}
	}
	// Dictionary names do not make the binding's own fields opaque.
	source, err := oif.ParseJSON([]byte(`{"base_environment":{"env":{"schema":{"unexpected_id":"native"}}}}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rewrite(source, nil, nil); err == nil {
		t.Fatal("undeclared binding identity accepted")
	}
}

func TestRewriteInlineHeaderNames(t *testing.T) {
	for _, raw := range []string{
		`{"tools":[{"type":"mcp_server","headers":{"client_id":"app","base_agent":"literal"}}]}`,
		`{"base_environment":{"network":{"allowlist":[{"domain":"example.com","transform":{"client_id":"app","id":"value"}}]}}}`,
		`{"base_environment":{"network":{"allowlist":[{"domain":"example.com","transform":[{"base_agent":"literal"}]}]}}}`,
	} {
		source, err := oif.ParseJSON([]byte(raw), oif.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		projected, err := Rewrite(source, map[string]string{"base_agent": "agent_resource"}, func(string, string) (string, error) {
			t.Fatal("inline header treated as a resource reference")
			return "", nil
		})
		if err != nil || projected.Raw() != raw {
			t.Fatalf("inline header changed: %s %v", projected.Raw(), err)
		}
	}
	source, err := oif.ParseJSON([]byte(`{"tools":[{"headers":{"client_id":{"unexpected_id":"native"}}}]}`), oif.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Rewrite(source, nil, nil); err == nil {
		t.Fatal("non-string header value accepted")
	}
}
