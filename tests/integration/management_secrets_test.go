//go:build integration

package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/tyk-swe/olp/internal/access"
	"github.com/tyk-swe/olp/internal/guardrails/credential"
	"github.com/tyk-swe/olp/internal/platform/egress"
	"github.com/tyk-swe/olp/internal/platform/secrets"
	"github.com/tyk-swe/olp/internal/sinks"
	"github.com/tyk-swe/olp/tests/harness"
)

func TestManagementWritesDiscardTheSecretsTheyRetire(t *testing.T) {
	t.Parallel()
	pool, _ := harness.Database(t)
	fixture := newUsageFixture(t, pool)
	client := managementHTTPAs(t, pool, fixture.ownerID, func(mux *http.ServeMux, server *access.Server) {
		credential.NewAPI(server).Register(mux)
		(&sinks.Server{Access: server, Egress: &egress.Policy{}}).Register(mux)
	})
	created := func(step, path, body string) (string, string) {
		t.Helper()
		reply := expectStatus(t, step, client.call(t, "POST", path, "", body), http.StatusCreated)
		var resource struct{ ID string }
		if err := json.Unmarshal(reply.body, &resource); err != nil {
			t.Fatal(err)
		}
		return path + "/" + resource.ID, reply.etag
	}
	expired := placeholderSecret(t, pool, secrets.MutationReplay)
	mustExec(t, pool, "UPDATE olp.secrets SET expires_at=now()-interval '1 minute' WHERE id=$1", expired)
	guardrail, _ := created("create guardrail credential", "/api/v1/guardrail-credentials",
		fmt.Sprintf(`{"project_id":%q,"name":"vendor","credential":{"api_key":"fixture-value"}}`, fixture.projectID))
	if secretStored(t, pool, expired) {
		t.Error("a management write kept an expired secret")
	}
	current := expectStatus(t, "read guardrail credential", client.call(t, "GET", guardrail, "", ""), http.StatusOK)
	expectStatus(t, "delete guardrail credential", client.call(t, "DELETE", guardrail, current.etag, ""), http.StatusNoContent)
	if left := storedSecrets(t, pool, secrets.GuardrailCredential); len(left) != 0 {
		t.Errorf("deleting the guardrail credential kept secrets %v", left)
	}

	const definition = `"name":"warehouse","destination":"https://sinks.example.com/export","streams":["requests"],"enabled":true`
	sink, etag := created("create sink", "/api/v1/sinks", `{`+definition+`,"type":"https","credential":"first-signing-key"}`)
	first := storedSecrets(t, pool, secrets.SinkCredential)
	rotated := expectStatus(t, "rotate sink credential", client.call(t, "PUT", sink, etag, `{`+definition+`,"credential":"second-signing-key"}`), http.StatusOK)
	if second := storedSecrets(t, pool, secrets.SinkCredential); len(first) != 1 || len(second) != 1 || second[0] == first[0] {
		t.Errorf("sink secrets went from %v to %v, want only the replacement", first, second)
	}
	expectStatus(t, "retire sink", client.call(t, "DELETE", sink, rotated.etag, ""), http.StatusNoContent)
	if left := storedSecrets(t, pool, secrets.SinkCredential); len(left) != 0 {
		t.Errorf("retiring the sink kept secrets %v", left)
	}
}
